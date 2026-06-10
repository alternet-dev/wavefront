package probe_test

import (
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/alternet-dev/wavefront/internal/probe"
)

// opsStub serves /ready with the given status on a loopback listener and
// returns its host:port.
func opsStub(t *testing.T, status int) string {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/ready", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://")
}

func TestReadyOK(t *testing.T) {
	addr := opsStub(t, http.StatusOK)
	if err := probe.Ready(addr, 2*time.Second); err != nil {
		t.Fatalf("Ready against a 200 /ready: %v, want nil", err)
	}
}

func TestReadyNotReady(t *testing.T) {
	addr := opsStub(t, http.StatusServiceUnavailable)
	err := probe.Ready(addr, 2*time.Second)
	if err == nil {
		t.Fatal("Ready against a 503 /ready: nil, want an error")
	}
	if !strings.Contains(err.Error(), "503") {
		t.Errorf("error %q should name the status 503", err)
	}
}

func TestReadyConnectionRefused(t *testing.T) {
	// Grab a loopback port and close it so the dial is refused.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	if err := probe.Ready(addr, 2*time.Second); err == nil {
		t.Fatal("Ready against a closed port: nil, want an error")
	}
}

// TestReadyWildcardHost: the ops listener default is 0.0.0.0:9090 — a listen
// address, not a dialable one. The probe must rewrite a wildcard host to
// loopback so the same configured value works for both serving and probing.
func TestReadyWildcardHost(t *testing.T) {
	addr := opsStub(t, http.StatusOK)
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("split %q: %v", addr, err)
	}
	if err := probe.Ready("0.0.0.0:"+port, 2*time.Second); err != nil {
		t.Fatalf("Ready with a wildcard host: %v, want nil (rewritten to loopback)", err)
	}
}
