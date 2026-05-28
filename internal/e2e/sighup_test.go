package e2e

import (
	"bytes"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/alternet-dev/wavefront/internal/bundletest"
)

// TestReloadBundleSwapsContractVersion — Server.ReloadBundle re-reads the
// bundle directory and swaps the live bundle atomically. After a swap that
// changes the served contract version, the old version stops being
// recognized and the new version starts serving — proof that the swap
// reaches the request path, not just the loader.
func TestReloadBundleSwapsContractVersion(t *testing.T) {
	h := Spawn(t, SpawnOpts{
		BackendHandler: func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"text":"ok"}`)
		},
	})

	if status, _ := sendSkewProbe(t, h, "2024-11"); status != http.StatusOK {
		t.Fatalf("baseline 2024-11 should succeed, got %d", status)
	}

	swapSingleLayer(t, h.Config.BundlePath, "2026-05", `version: 1
contracts:
  - contract_version: "2026-05"
    route: /v3/echo
    method: POST
    request_message: acme.v1.Ping
    response_message: acme.v1.Pong
`)
	if err := h.Server.ReloadBundle(); err != nil {
		t.Fatalf("ReloadBundle: %v", err)
	}

	if status, errCode := sendSkewProbe(t, h, "2024-11"); status != http.StatusBadRequest || errCode != "unsupported_contract_version" {
		t.Errorf("after reload, 2024-11 should be unsupported; got status=%d errCode=%q", status, errCode)
	}
	if status, _ := sendSkewProbe(t, h, "2026-05"); status != http.StatusOK {
		t.Errorf("after reload, 2026-05 should succeed; got status=%d", status)
	}
}

// TestReloadBundleFailurePreservesPrevious — when the bundle directory has
// drifted to a malformed state, ReloadBundle reports the error and the
// previous bundle keeps serving.
func TestReloadBundleFailurePreservesPrevious(t *testing.T) {
	h := Spawn(t, SpawnOpts{
		BackendHandler: func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"text":"ok"}`)
		},
	})

	// Replace the layer with a hollow directory; loader rejects it.
	layerDir := filepath.Join(h.Config.BundlePath, "2024-11")
	if err := os.RemoveAll(layerDir); err != nil {
		t.Fatalf("remove layer: %v", err)
	}
	if err := os.MkdirAll(layerDir, 0o755); err != nil {
		t.Fatalf("mkdir empty layer: %v", err)
	}

	if err := h.Server.ReloadBundle(); err == nil {
		t.Error("ReloadBundle should fail on a malformed bundle")
	}

	if status, _ := sendSkewProbe(t, h, "2024-11"); status != http.StatusOK {
		t.Errorf("after failed reload, old bundle should still serve; got status=%d", status)
	}
}

// sendSkewProbe sends a minimal Ping request at the supplied contract
// version and returns the (HTTP status, X-Wavefront-Error code) pair.
func sendSkewProbe(t *testing.T, h *Harness, version string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, h.Proxy.URL+"/v3/echo", bytes.NewReader(PingBytes(t, h.Bundle, "hi", 1)))
	req.Header.Set("X-Api-Contract-Version", version)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("http.Do: %v", err)
	}
	defer resp.Body.Close()
	return resp.StatusCode, resp.Header.Get("X-Wavefront-Error")
}

// swapSingleLayer wipes every layer subdirectory of bundleDir and writes a
// single new layer named after the supplied contract version. The proto
// FileDescriptorSet (and the throwaway openapi.json) are reused from
// bundletest — descriptors are stable across these tests; only the
// contract-version binding changes.
func swapSingleLayer(t *testing.T, bundleDir, version, versions string) {
	t.Helper()
	entries, err := os.ReadDir(bundleDir)
	if err != nil {
		t.Fatalf("read bundle dir: %v", err)
	}
	for _, e := range entries {
		if e.IsDir() {
			if err := os.RemoveAll(filepath.Join(bundleDir, e.Name())); err != nil {
				t.Fatalf("remove old layer %s: %v", e.Name(), err)
			}
		}
	}
	layerDir := filepath.Join(bundleDir, version)
	if err := os.MkdirAll(layerDir, 0o755); err != nil {
		t.Fatalf("mkdir layer: %v", err)
	}
	writes := map[string][]byte{
		"descriptors.binpb": bundletest.FDSBytes(t),
		"openapi.json":      []byte(bundletest.ValidOpenAPI),
		"versions.yaml":     []byte(versions),
	}
	for name, b := range writes {
		if err := os.WriteFile(filepath.Join(layerDir, name), b, 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
}
