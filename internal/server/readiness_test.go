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

// TestProxyPathReturnsUnavailableWhenNoBundle covers the boot-time race
// captured in issue #39's matrix: a request reaches the proxy data plane
// before SetBundle has run (e.g., the data listener is up but the bundle
// load is still in-flight, or the operator started the server with a
// deliberately empty bundle pointer). Before Chunk 1.7 this returned a
// confusing `502 upstream_error` ("no bundle loaded") — wavefront had no
// upstream to blame. The contract treats it as transient backend
// readiness: `503 unavailable` with `Retry-After: 1` so a well-behaved
// client backs off briefly and retries.
//
// This parallels /ready returning 503 when the bundle is absent — the
// proxy path now reuses the same code so the readiness signal is
// consistent across the data plane and the ops endpoint.
func TestProxyPathReturnsUnavailableWhenNoBundle(t *testing.T) {
	s := server.New(baseCfg("http://unused"))
	// Deliberately do NOT call SetBundle.
	front := httptest.NewServer(s.DataHandler())
	defer front.Close()

	req, _ := http.NewRequest(http.MethodPost, front.URL+"/v3/echo", strings.NewReader("ignored"))
	req.Header.Set("X-Api-Contract-Version", "2024-11")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
	if got := resp.Header.Get("X-Wavefront-Error"); got != "unavailable" {
		t.Errorf("X-Wavefront-Error = %q, want unavailable", got)
	}
	if got := resp.Header.Get("Retry-After"); got != "1" {
		t.Errorf("Retry-After = %q, want 1", got)
	}
	if got := resp.Header.Get("Content-Type"); got != "application/protobuf" {
		t.Errorf("Content-Type = %q, want application/protobuf", got)
	}
	// The proxy bundle-gate runs before negotiation, but the raw
	// client-sent version is still echoed so the caller can correlate the
	// failure with what it sent — same shape as the unknown_route gate.
	if got := resp.Header.Get("X-Wavefront-Contract-Version"); got != "2024-11" {
		t.Errorf("X-Wavefront-Contract-Version = %q, want %q (raw client value echoed)", got, "2024-11")
	}

	md := wavefrontErrorMD(t)
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	m := dynamicpb.NewMessage(md)
	if err := proto.Unmarshal(body, m); err != nil {
		t.Fatalf("response body is not wavefront.v0.Error: %v (raw=%q)", err, body)
	}
	if got := m.Get(md.Fields().ByName("code")).String(); got != "unavailable" {
		t.Errorf("decoded code = %q, want unavailable", got)
	}
	if got := m.Get(md.Fields().ByName("message")).String(); got == "" {
		t.Errorf("decoded message is empty; want a non-empty default")
	}
}

// TestProxyPathNoBundleWithoutContractVersionHeader pins the fallback
// branch of the bundle gate: when the client sends no
// X-Api-Contract-Version, the response header still emits the
// `unknown` sentinel so it is always well-defined.
func TestProxyPathNoBundleWithoutContractVersionHeader(t *testing.T) {
	s := server.New(baseCfg("http://unused"))
	front := httptest.NewServer(s.DataHandler())
	defer front.Close()

	req, _ := http.NewRequest(http.MethodPost, front.URL+"/v3/echo", strings.NewReader(""))
	// Deliberately no X-Api-Contract-Version header.
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", resp.StatusCode)
	}
	if got := resp.Header.Get("X-Wavefront-Error"); got != "unavailable" {
		t.Errorf("X-Wavefront-Error = %q, want unavailable", got)
	}
	if got := resp.Header.Get("X-Wavefront-Contract-Version"); got != "unknown" {
		t.Errorf("X-Wavefront-Contract-Version = %q, want %q (fallback when client sent no header)", got, "unknown")
	}
}
