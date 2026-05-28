package server_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/alternet-dev/wavefront/internal/bundle"
	"github.com/alternet-dev/wavefront/internal/bundletest"
	"github.com/alternet-dev/wavefront/internal/config"
	"github.com/alternet-dev/wavefront/internal/server"
)

// twoVersionDir builds a two-layer bundle:
//   - "2024-11": POST /v3/echo, acme.v1.Ping → acme.v1.Pong  (no override = default target)
//   - "2026-05": POST /v3/echo, acme.v1.Ping → acme.v1.Pong  (route override = named target "v2")
//
// A resolution.yaml is written at the bundle root that sends "2026-05" to
// the named target "v2". The caller sets WAVEFRONT_TARGETS=v2=<url> in the
// Config to wire it up.
func twoVersionDir(t *testing.T) string {
	t.Helper()
	const versionsV1 = `version: 1
contracts:
  - contract_version: "2024-11"
    route: /v3/echo
    method: POST
    request_message: acme.v1.Ping
    response_message: acme.v1.Pong
`
	const versionsV2 = `version: 1
contracts:
  - contract_version: "2026-05"
    route: /v3/echo
    method: POST
    request_message: acme.v1.Ping
    response_message: acme.v1.Pong
`
	dir := bundletest.MultiDir(t,
		bundletest.Layer{
			Name:        "2024-11",
			Descriptors: bundletest.FDSBytes(t),
			OpenAPI:     bundletest.ValidOpenAPI,
			Versions:    versionsV1,
		},
		bundletest.Layer{
			Name:        "2026-05",
			Descriptors: bundletest.FDSBytes(t),
			OpenAPI:     bundletest.ValidOpenAPI,
			Versions:    versionsV2,
		},
	)
	bundletest.WriteResolution(t, dir, `version: 1
overrides:
  - contract_version: "2026-05"
    route:
      target: v2
`)
	return dir
}

func TestRouteOverrideSendsVersionToNamedTarget(t *testing.T) {
	dir := twoVersionDir(t)
	b, err := bundle.Load(dir)
	if err != nil {
		t.Fatalf("load bundle: %v", err)
	}

	var defaultHit, namedHit int

	defaultUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		defaultHit++
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"text":"from-default"}`)
	}))
	defer defaultUpstream.Close()

	namedUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		namedHit++
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"text":"from-named"}`)
	}))
	defer namedUpstream.Close()

	cfg := &config.Config{
		UpstreamBaseURL:       defaultUpstream.URL,
		Targets:               map[string]string{"v2": namedUpstream.URL},
		ContractVersionHeader: "X-Api-Contract-Version",
		RequestTimeout:        baseCfg("x").RequestTimeout,
		MaxBodyBytes:          baseCfg("x").MaxBodyBytes,
	}
	s := server.New(cfg)
	s.SetBundle(b)
	front := httptest.NewServer(s.DataHandler())
	defer front.Close()

	body := pingBytes(t, b, "hi", 1)

	// Request with version "2024-11" (no override) must reach the default upstream.
	req1, _ := http.NewRequest(http.MethodPost, front.URL+"/v3/echo", strings.NewReader(string(body)))
	req1.Header.Set("X-Api-Contract-Version", "2024-11")
	resp1, err := http.DefaultClient.Do(req1)
	if err != nil {
		t.Fatalf("request 2024-11: %v", err)
	}
	resp1.Body.Close()
	if resp1.StatusCode != 200 {
		t.Fatalf("2024-11 status = %d, want 200", resp1.StatusCode)
	}
	if defaultHit != 1 || namedHit != 0 {
		t.Errorf("after 2024-11 request: defaultHit=%d namedHit=%d, want 1 0", defaultHit, namedHit)
	}

	// Request with version "2026-05" (route override → "v2") must reach the named upstream.
	req2, _ := http.NewRequest(http.MethodPost, front.URL+"/v3/echo", strings.NewReader(string(body)))
	req2.Header.Set("X-Api-Contract-Version", "2026-05")
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatalf("request 2026-05: %v", err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != 200 {
		t.Fatalf("2026-05 status = %d, want 200", resp2.StatusCode)
	}
	if defaultHit != 1 || namedHit != 1 {
		t.Errorf("after 2026-05 request: defaultHit=%d namedHit=%d, want 1 1", defaultHit, namedHit)
	}
}

func TestRouteOverrideUnknownTargetIs502(t *testing.T) {
	dir := twoVersionDir(t)
	b, err := bundle.Load(dir)
	if err != nil {
		t.Fatalf("load bundle: %v", err)
	}

	// Config has no "v2" target defined — the proxy must return a 502.
	cfg := baseCfg("http://unused")
	// Targets map is nil — TargetURL("v2") will return ("", false).
	s := server.New(cfg)
	s.SetBundle(b)
	front := httptest.NewServer(s.DataHandler())
	defer front.Close()

	body := pingBytes(t, b, "hi", 1)
	req, _ := http.NewRequest(http.MethodPost, front.URL+"/v3/echo", strings.NewReader(string(body)))
	req.Header.Set("X-Api-Contract-Version", "2026-05")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}
	if resp.Header.Get("X-Wavefront-Error") != "upstream_error" {
		t.Errorf("X-Wavefront-Error = %q, want upstream_error", resp.Header.Get("X-Wavefront-Error"))
	}
}
