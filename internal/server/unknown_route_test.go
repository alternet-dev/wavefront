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

// TestUnknownRouteReturns404Envelope covers every way an inbound request can
// miss the bundle's registered routes: a path no contract binds, and a path
// that IS bound but with the wrong method. Both fold into the same
// `unknown_route` 404 envelope — wavefront does not emit 405, does not
// enumerate Allow, and does not leak Go's default text 404.
func TestUnknownRouteReturns404Envelope(t *testing.T) {
	b := loadBundle(t) // bundletest.ValidVersions binds POST /v3/echo
	s := server.New(baseCfg("http://unused"))
	s.SetBundle(b)
	front := httptest.NewServer(s.DataHandler())
	defer front.Close()

	md := wavefrontErrorMD(t)

	cases := []struct {
		name   string
		method string
		path   string
	}{
		{name: "no contract binds this path", method: http.MethodGet, path: "/nonexistent"},
		{name: "POST to a path no contract binds", method: http.MethodPost, path: "/also-nope"},
		// /v3/echo IS bound — but to POST, not GET. v0.5 design: wrong-method
		// folds into unknown_route; we do NOT emit 405 or an Allow header.
		{name: "right path wrong method", method: http.MethodGet, path: "/v3/echo"},
		{name: "right path different wrong method", method: http.MethodDelete, path: "/v3/echo"},
		// The root path is never a registered route in the fixture.
		{name: "root path", method: http.MethodGet, path: "/"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req, _ := http.NewRequest(tc.method, front.URL+tc.path, strings.NewReader(""))
			// Deliberately do NOT set the contract-version header — the route
			// lookup happens before negotiation, so the absence of the header
			// must NOT change the outcome (still unknown_route, not
			// unsupported_contract_version).
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("request: %v", err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusNotFound {
				t.Errorf("status=%d want 404", resp.StatusCode)
			}
			if got := resp.Header.Get("X-Wavefront-Error"); got != "unknown_route" {
				t.Errorf("X-Wavefront-Error=%q want unknown_route", got)
			}
			if got := resp.Header.Get("Content-Type"); got != "application/protobuf" {
				t.Errorf("Content-Type=%q want application/protobuf", got)
			}
			// The client sent no contract-version header on these requests,
			// so the response header falls back to the "unknown" sentinel.
			// (When the client DOES send a value pre-negotiate, the raw
			// value is echoed — covered by TestUnknownRouteIgnoresContractVersionHeader
			// and TestUnknownRouteWithoutContractVersionHeader.)
			if got := resp.Header.Get("X-Wavefront-Contract-Version"); got != "unknown" {
				t.Errorf("X-Wavefront-Contract-Version=%q want unknown", got)
			}
			// Wrong-method must NOT enumerate Allow.
			if got := resp.Header.Get("Allow"); got != "" {
				t.Errorf("Allow=%q must be empty (wrong-method folds into unknown_route, no Allow)", got)
			}

			body, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatalf("read body: %v", err)
			}
			m := dynamicpb.NewMessage(md)
			if err := proto.Unmarshal(body, m); err != nil {
				t.Fatalf("response body is not wavefront.v0.Error: %v (raw=%q)", err, body)
			}
			if got := m.Get(md.Fields().ByName("code")).String(); got != "unknown_route" {
				t.Errorf("decoded code=%q want unknown_route", got)
			}
			if got := m.Get(md.Fields().ByName("message")).String(); got == "" {
				t.Errorf("decoded message is empty; want a non-empty default")
			}
		})
	}
}

// TestUnknownRouteIgnoresContractVersionHeader confirms that the route check
// runs BEFORE contract-version negotiation: even a perfectly valid version
// header on a request to an unbound path still produces unknown_route, not
// some downstream error. It also pins the response-header echo behaviour:
// the gate runs pre-negotiate, but the raw client-sent value is echoed
// verbatim in X-Wavefront-Contract-Version so the caller can correlate the
// failure with what it sent.
func TestUnknownRouteIgnoresContractVersionHeader(t *testing.T) {
	b := loadBundle(t)
	s := server.New(baseCfg("http://unused"))
	s.SetBundle(b)
	front := httptest.NewServer(s.DataHandler())
	defer front.Close()

	req, _ := http.NewRequest(http.MethodGet, front.URL+"/no-such-route", strings.NewReader(""))
	req.Header.Set("X-Api-Contract-Version", "2024-11") // valid version, but route is wrong
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status=%d want 404 (route check must precede negotiation)", resp.StatusCode)
	}
	if got := resp.Header.Get("X-Wavefront-Error"); got != "unknown_route" {
		t.Errorf("X-Wavefront-Error=%q want unknown_route", got)
	}
	// The route gate runs before negotiate validates the header, but the raw
	// client-sent value is still echoed so the caller sees what wavefront
	// received. The metric label and structured log keep "unknown" — only
	// the response header carries the raw value.
	if got := resp.Header.Get("X-Wavefront-Contract-Version"); got != "2024-11" {
		t.Errorf("X-Wavefront-Contract-Version=%q want %q (raw client value echoed pre-negotiate)", got, "2024-11")
	}
}

// TestUnknownRouteWithoutContractVersionHeader pins the fallback: when the
// client sends no X-Api-Contract-Version header, the pre-negotiate
// unknown_route 404 still emits a well-formed response with the
// versionUnknown sentinel ("unknown") in X-Wavefront-Contract-Version.
func TestUnknownRouteWithoutContractVersionHeader(t *testing.T) {
	b := loadBundle(t)
	s := server.New(baseCfg("http://unused"))
	s.SetBundle(b)
	front := httptest.NewServer(s.DataHandler())
	defer front.Close()

	req, _ := http.NewRequest(http.MethodGet, front.URL+"/no-such-route", strings.NewReader(""))
	// Deliberately no X-Api-Contract-Version header.
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status=%d want 404", resp.StatusCode)
	}
	if got := resp.Header.Get("X-Wavefront-Error"); got != "unknown_route" {
		t.Errorf("X-Wavefront-Error=%q want unknown_route", got)
	}
	if got := resp.Header.Get("X-Wavefront-Contract-Version"); got != "unknown" {
		t.Errorf("X-Wavefront-Contract-Version=%q want %q (fallback when client sent no header)", got, "unknown")
	}
}

// TestKnownRouteStillReaches the proxy still admits a request whose
// (path, method) matches a registered route. This protects against an
// over-broad route check that would 404 everything.
func TestKnownRouteStillReaches(t *testing.T) {
	b := loadBundle(t)
	// Wire a real upstream so the request can complete normally.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"text":"pong"}`)
	}))
	defer upstream.Close()

	s := server.New(baseCfg(upstream.URL))
	s.SetBundle(b)
	front := httptest.NewServer(s.DataHandler())
	defer front.Close()

	body := pingBytes(t, b, "hi", 1)
	req, _ := http.NewRequest(http.MethodPost, front.URL+"/v3/echo", strings.NewReader(string(body)))
	req.Header.Set("X-Api-Contract-Version", "2024-11")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status=%d want 200 — known route must pass the route check", resp.StatusCode)
	}
}
