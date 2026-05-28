package server_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/dynamicpb"

	"github.com/alternet-dev/wavefront/internal/server"
)

// TestUnsupportedMediaTypeWrongContentType covers every shape of a wrong
// Content-Type on a request that DOES carry a body. Each is rejected with
// the 415 unsupported_media_type envelope — distinct from decode_failed,
// which is reserved for a valid envelope whose bytes don't parse.
//
// The check runs after the route gate (so unknown_route still wins on
// unmatched paths) and before the body is decoded (so decode_failed stays
// scoped to malformed protobuf inside a valid envelope).
func TestUnsupportedMediaTypeWrongContentType(t *testing.T) {
	b := loadBundle(t) // bundletest.ValidVersions binds POST /v3/echo
	s := server.New(baseCfg("http://unused"))
	s.SetBundle(b)
	front := httptest.NewServer(s.DataHandler())
	defer front.Close()

	md := wavefrontErrorMD(t)

	cases := []struct {
		name        string
		contentType string
	}{
		{name: "text/plain", contentType: "text/plain"},
		{name: "application/json", contentType: "application/json"},
		{name: "application/xml", contentType: "application/xml"},
		{name: "application/octet-stream", contentType: "application/octet-stream"},
		// A bare value (no slash) is still wrong — wavefront does no parsing
		// gymnastics, it just compares against the contract media type.
		{name: "garbage", contentType: "garbage"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// A non-empty body is what triggers the check. The bytes themselves
			// don't matter — the envelope is rejected before any decode runs.
			req, _ := http.NewRequest(http.MethodPost, front.URL+"/v3/echo", strings.NewReader("ignored body"))
			req.Header.Set("Content-Type", tc.contentType)
			req.Header.Set("X-Api-Contract-Version", "2024-11")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("request: %v", err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusUnsupportedMediaType {
				t.Errorf("status=%d want 415", resp.StatusCode)
			}
			if got := resp.Header.Get("X-Wavefront-Error"); got != "unsupported_media_type" {
				t.Errorf("X-Wavefront-Error=%q want unsupported_media_type", got)
			}
			// Response Content-Type is the envelope's protobuf type even though
			// the client sent something else (body-type invariant).
			if got := resp.Header.Get("Content-Type"); got != "application/protobuf" {
				t.Errorf("Content-Type=%q want application/protobuf", got)
			}
			// The check runs pre-negotiate (it sits between route-match and
			// body-read), so the response header echoes the raw client value.
			if got := resp.Header.Get("X-Wavefront-Contract-Version"); got != "2024-11" {
				t.Errorf("X-Wavefront-Contract-Version=%q want %q", got, "2024-11")
			}

			body, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatalf("read body: %v", err)
			}
			m := dynamicpb.NewMessage(md)
			if err := proto.Unmarshal(body, m); err != nil {
				t.Fatalf("response body is not wavefront.v0.Error: %v (raw=%q)", err, body)
			}
			if got := m.Get(md.Fields().ByName("code")).String(); got != "unsupported_media_type" {
				t.Errorf("decoded code=%q want unsupported_media_type", got)
			}
			if got := m.Get(md.Fields().ByName("message")).String(); got == "" {
				t.Errorf("decoded message is empty; want a non-empty default")
			}
		})
	}
}

// TestUnsupportedMediaTypeContentTypeWithParameters pins parameter tolerance:
// `application/protobuf; charset=binary` (or any other parameter list) is
// accepted because the media type itself is correct. The check strips
// parameters before comparing.
func TestUnsupportedMediaTypeContentTypeWithParameters(t *testing.T) {
	b := loadBundle(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"text":"pong"}`)
	}))
	defer upstream.Close()

	s := server.New(baseCfg(upstream.URL))
	s.SetBundle(b)
	front := httptest.NewServer(s.DataHandler())
	defer front.Close()

	cases := []string{
		"application/protobuf",
		"application/protobuf; charset=binary",
		"application/protobuf;charset=binary",       // no whitespace
		"application/protobuf; charset=binary; q=1", // multiple parameters
		"APPLICATION/PROTOBUF",                      // case-insensitive media type
	}
	for _, ct := range cases {
		t.Run(ct, func(t *testing.T) {
			body := pingBytes(t, b, "hi", 1)
			req, _ := http.NewRequest(http.MethodPost, front.URL+"/v3/echo", strings.NewReader(string(body)))
			req.Header.Set("Content-Type", ct)
			req.Header.Set("X-Api-Contract-Version", "2024-11")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("request: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Errorf("status=%d want 200 — application/protobuf with parameters must be accepted (got X-Wavefront-Error=%q)", resp.StatusCode, resp.Header.Get("X-Wavefront-Error"))
			}
		})
	}
}

// TestUnsupportedMediaTypeAbsentContentTypeNoBody pins the "no body, no check"
// rule: a request that carries no body need not declare a Content-Type. This
// is the safest interpretation — the check applies only to requests that
// actually present an envelope to the proxy. (A no-body GET-style request to
// a route that expects a body still flows through to decode_failed/transform
// errors downstream; this test only confirms the 415 gate does not fire on
// absence-of-body.)
func TestUnsupportedMediaTypeAbsentContentTypeNoBody(t *testing.T) {
	b := loadBundle(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"text":"pong"}`)
	}))
	defer upstream.Close()

	s := server.New(baseCfg(upstream.URL))
	s.SetBundle(b)
	front := httptest.NewServer(s.DataHandler())
	defer front.Close()

	// No body, no Content-Type header — the 415 gate must not fire. The
	// downstream decode of an empty body against acme.v1.Ping succeeds (proto3
	// treats empty bytes as a zero-valued message), so the request reaches the
	// upstream normally and we don't get a 415 envelope back.
	req, _ := http.NewRequest(http.MethodPost, front.URL+"/v3/echo", nil)
	req.Header.Set("X-Api-Contract-Version", "2024-11")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	if got := resp.Header.Get("X-Wavefront-Error"); got == "unsupported_media_type" {
		t.Errorf("X-Wavefront-Error=%q — 415 must not fire on a no-body request without Content-Type", got)
	}
	if resp.StatusCode == http.StatusUnsupportedMediaType {
		t.Errorf("status=%d — 415 must not fire on a no-body request without Content-Type", resp.StatusCode)
	}
}

// TestUnsupportedMediaTypeBodyWithoutContentType pins the inverse: a request
// that DOES carry a body but omits Content-Type is rejected with 415. An
// envelope must be declared whenever there's something to envelope.
func TestUnsupportedMediaTypeBodyWithoutContentType(t *testing.T) {
	b := loadBundle(t)
	s := server.New(baseCfg("http://unused"))
	s.SetBundle(b)
	front := httptest.NewServer(s.DataHandler())
	defer front.Close()

	req, _ := http.NewRequest(http.MethodPost, front.URL+"/v3/echo", strings.NewReader("some bytes"))
	// Deliberately no Content-Type. net/http's transport may default-set
	// "application/octet-stream" for a non-nil body that lacks one; the test
	// passes through Go's defaulting because that value is also not
	// application/protobuf, so the gate still fires either way.
	req.Header.Set("X-Api-Contract-Version", "2024-11")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnsupportedMediaType {
		t.Errorf("status=%d want 415 (body present, Content-Type missing or non-protobuf)", resp.StatusCode)
	}
	if got := resp.Header.Get("X-Wavefront-Error"); got != "unsupported_media_type" {
		t.Errorf("X-Wavefront-Error=%q want unsupported_media_type", got)
	}
}

// TestUnsupportedMediaTypeRunsAfterRouteGate pins the ordering: even with a
// wrong Content-Type, an unknown route still emits unknown_route (404), not
// unsupported_media_type (415). The route gate must win.
func TestUnsupportedMediaTypeRunsAfterRouteGate(t *testing.T) {
	b := loadBundle(t)
	s := server.New(baseCfg("http://unused"))
	s.SetBundle(b)
	front := httptest.NewServer(s.DataHandler())
	defer front.Close()

	req, _ := http.NewRequest(http.MethodPost, front.URL+"/no-such-route", strings.NewReader("garbage"))
	req.Header.Set("Content-Type", "text/plain")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status=%d want 404 (unknown_route must precede unsupported_media_type)", resp.StatusCode)
	}
	if got := resp.Header.Get("X-Wavefront-Error"); got != "unknown_route" {
		t.Errorf("X-Wavefront-Error=%q want unknown_route", got)
	}
}

// TestUnsupportedMediaTypeDisambiguatesFromDecodeFailed pins the issue #39
// matrix: wrong envelope (415) and bad bytes inside a valid envelope (400)
// are distinct codes against the same route.
//
//   - Wrong Content-Type (text/plain) + body that would parse as valid
//     protobuf for the route → 415 unsupported_media_type. The envelope is
//     wrong, so the body never reaches decode.
//   - Correct Content-Type (application/protobuf) + body that is NOT valid
//     protobuf for the route → 400 decode_failed. The envelope is fine, but
//     the bytes can't decode.
func TestUnsupportedMediaTypeDisambiguatesFromDecodeFailed(t *testing.T) {
	b := loadBundle(t)
	s := server.New(baseCfg("http://unused"))
	s.SetBundle(b)
	front := httptest.NewServer(s.DataHandler())
	defer front.Close()

	// Valid protobuf bytes for acme.v1.Ping — would decode cleanly if the
	// envelope were right.
	validProto := pingBytes(t, b, "hi", 1)

	// Wrong envelope + valid bytes → 415, never reaches decode.
	t.Run("wrong-envelope-valid-bytes", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodPost, front.URL+"/v3/echo", strings.NewReader(string(validProto)))
		req.Header.Set("Content-Type", "text/plain")
		req.Header.Set("X-Api-Contract-Version", "2024-11")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("request: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusUnsupportedMediaType {
			t.Errorf("status=%d want 415", resp.StatusCode)
		}
		if got := resp.Header.Get("X-Wavefront-Error"); got != "unsupported_media_type" {
			t.Errorf("X-Wavefront-Error=%q want unsupported_media_type", got)
		}
	})

	// Right envelope + bad bytes → 400 decode_failed. This pins the existing
	// behavior so we know the new 415 gate didn't accidentally absorb it.
	t.Run("right-envelope-bad-bytes", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodPost, front.URL+"/v3/echo", strings.NewReader("not valid protobuf bytes"))
		req.Header.Set("Content-Type", "application/protobuf")
		req.Header.Set("X-Api-Contract-Version", "2024-11")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("request: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("status=%d want 400", resp.StatusCode)
		}
		if got := resp.Header.Get("X-Wavefront-Error"); got != "decode_failed" {
			t.Errorf("X-Wavefront-Error=%q want decode_failed", got)
		}
	})
}
