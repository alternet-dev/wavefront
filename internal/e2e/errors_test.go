package e2e

import (
	"bytes"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/alternet-dev/wavefront/internal/config"
)

func TestUpstreamTimeoutReturns504(t *testing.T) {
	// Claim: an upstream exceeding WAVEFRONT_REQUEST_TIMEOUT_MS returns the
	// typed upstream_timeout error (HTTP 504) with Retry-After.
	h := Spawn(t, SpawnOpts{
		BackendHandler: func(_ http.ResponseWriter, _ *http.Request) {
			time.Sleep(300 * time.Millisecond)
		},
		ConfigOverride: func(c *config.Config) { c.RequestTimeout = 40 * time.Millisecond },
	})

	req, _ := http.NewRequest(http.MethodPost, h.Proxy.URL+"/v3/echo", bytes.NewReader(PingBytes(t, h.Bundle, "hi", 1)))
	req.Header.Set("Content-Type", "application/protobuf")
	req.Header.Set("X-Api-Contract-Version", "2024-11")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusGatewayTimeout {
		t.Fatalf("status = %d, want 504", resp.StatusCode)
	}
	if resp.Header.Get("X-Wavefront-Error") != "upstream_timeout" {
		t.Errorf("X-Wavefront-Error = %q", resp.Header.Get("X-Wavefront-Error"))
	}
	if resp.Header.Get("Retry-After") != "0" {
		t.Errorf("Retry-After = %q, want 0", resp.Header.Get("Retry-After"))
	}
}

func TestRequestBodyTooLargeReturns413(t *testing.T) {
	// Claim: an inbound body exceeding WAVEFRONT_MAX_BODY_BYTES returns the
	// typed request_body_too_large error (HTTP 413). The response header
	// echoes the raw client-sent contract-version value so the caller can
	// correlate the rejection with what it sent (even though the failure
	// is pre-negotiate).
	h := Spawn(t, SpawnOpts{
		ConfigOverride: func(c *config.Config) { c.MaxBodyBytes = 8 },
	})

	req, _ := http.NewRequest(http.MethodPost, h.Proxy.URL+"/v3/echo", strings.NewReader("far more than eight bytes of body"))
	req.Header.Set("Content-Type", "application/protobuf")
	req.Header.Set("X-Api-Contract-Version", "2024-11")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", resp.StatusCode)
	}
	if resp.Header.Get("X-Wavefront-Error") != "request_body_too_large" {
		t.Errorf("X-Wavefront-Error = %q", resp.Header.Get("X-Wavefront-Error"))
	}
	if got := resp.Header.Get("X-Wavefront-Contract-Version"); got != "2024-11" {
		t.Errorf("X-Wavefront-Contract-Version = %q, want %q (raw client value echoed)", got, "2024-11")
	}
}

func TestUpstreamUnreachableReturns502(t *testing.T) {
	// Claim: an unreachable upstream (network-level error — connection
	// refused, no route to host) returns the typed upstream_error (HTTP 502),
	// distinct from upstream_timeout (504) and from an upstream non-2xx.
	h := Spawn(t, SpawnOpts{
		ConfigOverride: func(c *config.Config) {
			// Port 1 is reserved; nothing listens. The proxy will get a
			// connect failure that is not a deadline exceeded.
			c.UpstreamBaseURL = "http://127.0.0.1:1"
		},
	})

	req, _ := http.NewRequest(http.MethodPost, h.Proxy.URL+"/v3/echo", bytes.NewReader(PingBytes(t, h.Bundle, "hi", 1)))
	req.Header.Set("Content-Type", "application/protobuf")
	req.Header.Set("X-Api-Contract-Version", "2024-11")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}
	if got := resp.Header.Get("X-Wavefront-Error"); got != "upstream_error" {
		t.Errorf("X-Wavefront-Error = %q, want upstream_error", got)
	}
}

func TestRequestTransformFailureReturns422(t *testing.T) {
	// Claim: a request transform verb that cannot apply at runtime returns
	// transform_failed (HTTP 422) and does NOT call the upstream.
	// Coerce text (non-numeric "hi") to number: passes the load-time
	// descriptor cross-check, fails at runtime.
	h := Spawn(t, SpawnOpts{
		Resolution: `version: 1
overrides:
  - contract_version: "2024-11"
    transform:
      request:
        - coerce: { field: text, to: number }
`,
	})

	req, _ := http.NewRequest(http.MethodPost, h.Proxy.URL+"/v3/echo", bytes.NewReader(PingBytes(t, h.Bundle, "hi", 7)))
	req.Header.Set("Content-Type", "application/protobuf")
	req.Header.Set("X-Api-Contract-Version", "2024-11")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("want 422, got %d", resp.StatusCode)
	}
	if resp.Header.Get("X-Wavefront-Error") != "transform_failed" {
		t.Errorf("X-Wavefront-Error=%q", resp.Header.Get("X-Wavefront-Error"))
	}
	if c := h.Backend.Count(); c != 0 {
		t.Errorf("upstream must not be called on request transform failure, got %d requests", c)
	}
}

func TestRenameAbsentSourceReturns422(t *testing.T) {
	// Claim: a rename whose source field is absent at runtime returns
	// transform_failed (HTTP 422) and does NOT call the upstream.
	// rename n → renamed_n: n exists on the descriptor (load-time check
	// passes), but the request has n unset (proto3 zero value is omitted by
	// protojson), so the rename source is absent at runtime.
	h := Spawn(t, SpawnOpts{
		Resolution: `version: 1
overrides:
  - contract_version: "2024-11"
    transform:
      request:
        - rename: { from: n, to: renamed_n }
`,
	})

	req, _ := http.NewRequest(http.MethodPost, h.Proxy.URL+"/v3/echo", bytes.NewReader(PingTextOnly(t, h.Bundle, "hi")))
	req.Header.Set("Content-Type", "application/protobuf")
	req.Header.Set("X-Api-Contract-Version", "2024-11")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("want 422, got %d", resp.StatusCode)
	}
	if resp.Header.Get("X-Wavefront-Error") != "transform_failed" {
		t.Errorf("X-Wavefront-Error=%q want transform_failed", resp.Header.Get("X-Wavefront-Error"))
	}
	if c := h.Backend.Count(); c != 0 {
		t.Errorf("upstream must not be called on request transform failure, got %d requests", c)
	}
}
