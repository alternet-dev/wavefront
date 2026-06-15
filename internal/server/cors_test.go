package server_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/alternet-dev/wavefront/internal/server"
)

const corsOrigin = "https://domain.localhost"

// corsUpstream stands in for a CORS-correct backend (core-api's CORSMiddleware):
// it answers OPTIONS preflights with the Access-Control-* set and stamps
// Access-Control-Allow-Origin on the actual response.
func corsUpstream() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodOptions {
			w.Header().Set("Access-Control-Allow-Origin", r.Header.Get("Origin"))
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "authorization")
			w.Header().Set("Access-Control-Allow-Credentials", "true")
			w.Header().Set("Vary", "Origin")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.Header().Set("Access-Control-Allow-Origin", r.Header.Get("Origin"))
		w.Header().Set("Access-Control-Allow-Credentials", "true")
		w.Header().Set("Vary", "Origin")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"text":"pong"}`))
	})
}

// TestCORSPreflightForwarded: an OPTIONS preflight is forwarded to the upstream
// and its CORS response relayed. wavefront does not own CORS; the upstream is
// the source of truth (#147). The presence of Access-Control-Allow-Methods —
// which the stub only sets in its OPTIONS branch — proves the preflight reached
// the upstream rather than being synthesized or 404'd.
func TestCORSPreflightForwarded(t *testing.T) {
	b := loadBundle(t)
	upstream := httptest.NewServer(corsUpstream())
	defer upstream.Close()
	s := server.New(baseCfg(upstream.URL))
	s.SetBundle(b)
	front := httptest.NewServer(s.DataHandler())
	defer front.Close()

	req, _ := http.NewRequest(http.MethodOptions, front.URL+"/v3/echo", nil)
	req.Header.Set("Origin", corsOrigin)
	req.Header.Set("Access-Control-Request-Method", "POST")
	req.Header.Set("Access-Control-Request-Headers", "authorization, x-api-contract-version")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("preflight status = %d, want 204 (relayed from upstream)", resp.StatusCode)
	}
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != corsOrigin {
		t.Errorf("Access-Control-Allow-Origin = %q, want %q", got, corsOrigin)
	}
	if got := resp.Header.Get("Access-Control-Allow-Methods"); got == "" {
		t.Error("Access-Control-Allow-Methods missing — preflight was not forwarded to the upstream")
	}
	if got := resp.Header.Get("Access-Control-Allow-Credentials"); got != "true" {
		t.Errorf("Access-Control-Allow-Credentials = %q, want true", got)
	}
}

// TestCORSActualResponseCarriesUpstreamHeaders: the proxied (non-preflight)
// response carries the upstream's Access-Control-* / Vary headers, while the
// body is still the encoded response_message and the proxy contract headers are
// intact. (#147)
func TestCORSActualResponseCarriesUpstreamHeaders(t *testing.T) {
	b := loadBundle(t)
	upstream := httptest.NewServer(corsUpstream())
	defer upstream.Close()
	s := server.New(baseCfg(upstream.URL))
	s.SetBundle(b)
	front := httptest.NewServer(s.DataHandler())
	defer front.Close()

	req, _ := http.NewRequest(http.MethodPost, front.URL+"/v3/echo", strings.NewReader(string(pingBytes(t, b, "hi", 1))))
	req.Header.Set("Content-Type", "application/protobuf")
	req.Header.Set("X-Api-Contract-Version", "2024-11")
	req.Header.Set("Origin", corsOrigin)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != corsOrigin {
		t.Errorf("Access-Control-Allow-Origin = %q, want %q (upstream CORS must pass through)", got, corsOrigin)
	}
	if got := resp.Header.Get("Access-Control-Allow-Credentials"); got != "true" {
		t.Errorf("Access-Control-Allow-Credentials = %q, want true", got)
	}
	if got := resp.Header.Get("Vary"); !strings.Contains(got, "Origin") {
		t.Errorf("Vary = %q, want it to contain Origin", got)
	}
	// The proxy contract is unchanged: protobuf body, contract-version header.
	if ct := resp.Header.Get("Content-Type"); ct != "application/protobuf" {
		t.Errorf("Content-Type = %q, want application/protobuf", ct)
	}
	if v := resp.Header.Get("X-Wavefront-Contract-Version"); v != "2024-11" {
		t.Errorf("X-Wavefront-Contract-Version = %q, want 2024-11", v)
	}
	if out, _ := io.ReadAll(resp.Body); len(out) == 0 {
		t.Error("expected an encoded protobuf body")
	}
}

// TestCORSWavefrontOriginatedErrorHasNoCORS pins the deliberate pass-through
// gap (#147): a wavefront-originated error (here unknown_route — no upstream is
// ever called) has no upstream CORS to mirror, so no Access-Control-Allow-Origin
// is set. These are misconfigured-request / dev-time conditions, not the normal
// browser flow.
func TestCORSWavefrontOriginatedErrorHasNoCORS(t *testing.T) {
	b := loadBundle(t)
	upstream := httptest.NewServer(corsUpstream())
	defer upstream.Close()
	s := server.New(baseCfg(upstream.URL))
	s.SetBundle(b)
	front := httptest.NewServer(s.DataHandler())
	defer front.Close()

	req, _ := http.NewRequest(http.MethodGet, front.URL+"/no/such/route", nil)
	req.Header.Set("Origin", corsOrigin)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 unknown_route", resp.StatusCode)
	}
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("Access-Control-Allow-Origin = %q on a wavefront-originated error, want empty (no upstream to mirror)", got)
	}
}
