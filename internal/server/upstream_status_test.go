package server_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/dynamicpb"

	"github.com/alternet-dev/wavefront/internal/server"
)

// TestUpstreamStatusPassthroughTable codifies the upstream-status passthrough
// contract from docs/protocol.md and issue #39: when the upstream returns one
// of {401, 403, 404, 405, 409, 410, 422, 429, 451}, wavefront preserves the
// upstream's exact status and emits the standard wavefront.v0.Error envelope
// with X-Wavefront-Error: upstream_status. The upstream's raw body is
// deliberately NOT relayed — the v0.1 body-type invariant requires every
// non-success body to be a wavefront.v0.Error proto.
func TestUpstreamStatusPassthroughTable(t *testing.T) {
	cases := []int{
		http.StatusUnauthorized,               // 401
		http.StatusForbidden,                  // 403
		http.StatusNotFound,                   // 404
		http.StatusMethodNotAllowed,           // 405
		http.StatusConflict,                   // 409
		http.StatusGone,                       // 410
		http.StatusUnprocessableEntity,        // 422
		http.StatusTooManyRequests,            // 429
		http.StatusUnavailableForLegalReasons, // 451
	}
	md := wavefrontErrorMD(t)
	for _, status := range cases {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			b := loadBundle(t)

			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				// Upstream sends a raw body that wavefront must NOT relay.
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(status)
				_, _ = w.Write([]byte(`{"upstream":"raw body that must not reach the client"}`))
			}))
			defer upstream.Close()

			s := server.New(baseCfg(upstream.URL))
			s.SetBundle(b)
			front := httptest.NewServer(s.DataHandler())
			defer front.Close()

			req, _ := http.NewRequest(http.MethodPost, front.URL+"/v3/echo", strings.NewReader(string(pingBytes(t, b, "hi", 1))))
			req.Header.Set("X-Api-Contract-Version", "2024-11")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != status {
				t.Fatalf("status = %d, want %d (upstream status must be preserved)", resp.StatusCode, status)
			}
			if got := resp.Header.Get("X-Wavefront-Error"); got != "upstream_status" {
				t.Errorf("X-Wavefront-Error = %q, want upstream_status", got)
			}
			if ct := resp.Header.Get("Content-Type"); ct != "application/protobuf" {
				t.Errorf("Content-Type = %q, want application/protobuf (body type invariant)", ct)
			}
			if v := resp.Header.Get("X-Wavefront-Contract-Version"); v != "2024-11" {
				t.Errorf("X-Wavefront-Contract-Version = %q, want 2024-11", v)
			}

			body, _ := io.ReadAll(resp.Body)
			// The upstream's raw JSON must not bleed through.
			if strings.Contains(string(body), "raw body that must not reach the client") {
				t.Errorf("upstream raw body leaked into wavefront response: %q", body)
			}
			m := dynamicpb.NewMessage(md)
			if err := proto.Unmarshal(body, m); err != nil {
				t.Fatalf("response body is not wavefront.v0.Error: %v (raw=%q)", err, body)
			}
			if got := m.Get(md.Fields().ByName("code")).String(); got != "upstream_status" {
				t.Errorf("decoded code = %q, want upstream_status", got)
			}
			if got := m.Get(md.Fields().ByName("message")).String(); got == "" {
				t.Errorf("decoded message is empty; want a non-empty default")
			}
		})
	}
}

// TestUpstream429PreservesRetryAfter pins the special-case Retry-After
// preservation for upstream 429 — the upstream's value flows verbatim onto
// the wavefront response so clients can honor it.
func TestUpstream429PreservesRetryAfter(t *testing.T) {
	b := loadBundle(t)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "60")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer upstream.Close()

	s := server.New(baseCfg(upstream.URL))
	s.SetBundle(b)
	front := httptest.NewServer(s.DataHandler())
	defer front.Close()

	req, _ := http.NewRequest(http.MethodPost, front.URL+"/v3/echo", strings.NewReader(string(pingBytes(t, b, "hi", 1))))
	req.Header.Set("X-Api-Contract-Version", "2024-11")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", resp.StatusCode)
	}
	if got := resp.Header.Get("X-Wavefront-Error"); got != "upstream_status" {
		t.Errorf("X-Wavefront-Error = %q, want upstream_status", got)
	}
	if got := resp.Header.Get("Retry-After"); got != "60" {
		t.Errorf("Retry-After = %q, want 60 (upstream value must be relayed verbatim)", got)
	}
}

// TestUpstream429NoRetryAfter verifies that wavefront never fabricates a
// Retry-After header — when the upstream omits one on a 429, the wavefront
// response also omits it. The fixed Retry-After: 0 belongs to upstream_timeout
// alone.
func TestUpstream429NoRetryAfter(t *testing.T) {
	b := loadBundle(t)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer upstream.Close()

	s := server.New(baseCfg(upstream.URL))
	s.SetBundle(b)
	front := httptest.NewServer(s.DataHandler())
	defer front.Close()

	req, _ := http.NewRequest(http.MethodPost, front.URL+"/v3/echo", strings.NewReader(string(pingBytes(t, b, "hi", 1))))
	req.Header.Set("X-Api-Contract-Version", "2024-11")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", resp.StatusCode)
	}
	if got := resp.Header.Get("Retry-After"); got != "" {
		t.Errorf("Retry-After = %q, want empty (must not be fabricated)", got)
	}
}

// TestUpstream404IsUpstreamStatus pins the disambiguation half of the dual-
// origin 404 contract: when the route gate passes (a contract DOES bind this
// (path, method)) and the UPSTREAM returns 404, the wavefront response carries
// X-Wavefront-Error: upstream_status — not unknown_route. The unknown_route
// branch is exercised by TestUnknownRouteReturns404Envelope in
// unknown_route_test.go.
func TestUpstream404IsUpstreamStatus(t *testing.T) {
	b := loadBundle(t)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer upstream.Close()

	s := server.New(baseCfg(upstream.URL))
	s.SetBundle(b)
	front := httptest.NewServer(s.DataHandler())
	defer front.Close()

	// /v3/echo IS bound by the fixture, so the route gate admits the request
	// and the upstream is consulted.
	req, _ := http.NewRequest(http.MethodPost, front.URL+"/v3/echo", strings.NewReader(string(pingBytes(t, b, "hi", 1))))
	req.Header.Set("X-Api-Contract-Version", "2024-11")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	if got := resp.Header.Get("X-Wavefront-Error"); got != "upstream_status" {
		t.Errorf("X-Wavefront-Error = %q, want upstream_status (route gate passed, upstream returned 404)", got)
	}
}

// TestUpstream422IsUpstreamStatus pins the disambiguation half of the dual-
// origin 422 contract: when no wavefront request transform fails (the bundle
// has no transform stanzas in this fixture) and the upstream returns 422, the
// wavefront response carries X-Wavefront-Error: upstream_status — not
// transform_failed. The transform_failed branch is exercised by
// TestPipelineRequestTransformFailureIs422 in transform_pipeline_test.go.
func TestUpstream422IsUpstreamStatus(t *testing.T) {
	b := loadBundle(t)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
	}))
	defer upstream.Close()

	s := server.New(baseCfg(upstream.URL))
	s.SetBundle(b)
	front := httptest.NewServer(s.DataHandler())
	defer front.Close()

	req, _ := http.NewRequest(http.MethodPost, front.URL+"/v3/echo", strings.NewReader(string(pingBytes(t, b, "hi", 1))))
	req.Header.Set("X-Api-Contract-Version", "2024-11")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", resp.StatusCode)
	}
	if got := resp.Header.Get("X-Wavefront-Error"); got != "upstream_status" {
		t.Errorf("X-Wavefront-Error = %q, want upstream_status (no transform configured, upstream returned 422)", got)
	}
}

// TestUpstreamNon2xxOutsideSetIs502 confirms that codes outside the
// passthrough set still collapse to upstream_error 502 — the selective
// passthrough is selective, not blanket. 418 (teapot) and 500 stand in for
// "anything that is not in the bounded set."
func TestUpstreamNon2xxOutsideSetIs502(t *testing.T) {
	cases := []int{
		http.StatusTeapot,              // 418 — out of contract
		http.StatusInternalServerError, // 500 — upstream 5xx is not in the passthrough set
		http.StatusBadGateway,          // 502 — out of the set
		http.StatusServiceUnavailable,  // 503 — out of the set
	}
	for _, status := range cases {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			b := loadBundle(t)

			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(status)
				_, _ = w.Write([]byte(`{"err":"boom"}`))
			}))
			defer upstream.Close()

			s := server.New(baseCfg(upstream.URL))
			s.SetBundle(b)
			front := httptest.NewServer(s.DataHandler())
			defer front.Close()

			req, _ := http.NewRequest(http.MethodPost, front.URL+"/v3/echo", strings.NewReader(string(pingBytes(t, b, "hi", 1))))
			req.Header.Set("X-Api-Contract-Version", "2024-11")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusBadGateway {
				t.Fatalf("status = %d, want 502 (upstream %d is outside the passthrough set)", resp.StatusCode, status)
			}
			if got := resp.Header.Get("X-Wavefront-Error"); got != "upstream_error" {
				t.Errorf("X-Wavefront-Error = %q, want upstream_error", got)
			}
		})
	}
}
