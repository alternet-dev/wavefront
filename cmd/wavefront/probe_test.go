package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// readyStub serves /ready with the given status and points
// WAVEFRONT_METRICS_ADDR at it for the duration of the test, so runProbe
// resolves the same configured ops address the server would.
func readyStub(t *testing.T, status int) {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/ready", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	t.Setenv("WAVEFRONT_METRICS_ADDR", strings.TrimPrefix(srv.URL, "http://"))
}

// The healthcheck contract is the exit code: 200 → 0, anything else → 1,
// usage error → 2. (#145)
func TestRunProbeExitCodes(t *testing.T) {
	t.Run("ready 200 exits 0", func(t *testing.T) {
		readyStub(t, http.StatusOK)
		if got := runProbe([]string{"--ready"}); got != 0 {
			t.Errorf("runProbe = %d, want 0", got)
		}
	})
	t.Run("not-ready 503 exits 1", func(t *testing.T) {
		readyStub(t, http.StatusServiceUnavailable)
		if got := runProbe([]string{"--ready"}); got != 1 {
			t.Errorf("runProbe = %d, want 1", got)
		}
	})
	t.Run("connection refused exits 1", func(t *testing.T) {
		// Point at a loopback port nothing listens on.
		t.Setenv("WAVEFRONT_METRICS_ADDR", "127.0.0.1:1")
		if got := runProbe([]string{"--ready"}); got != 1 {
			t.Errorf("runProbe = %d, want 1", got)
		}
	})
	t.Run("missing --ready is a usage error", func(t *testing.T) {
		if got := runProbe(nil); got != 2 {
			t.Errorf("runProbe = %d, want 2", got)
		}
	})
}
