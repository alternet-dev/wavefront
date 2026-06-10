// Package probe implements the container self-probe behind
// `wavefront probe --ready`. The runtime image is distroless — no shell, no
// wget/curl — so an exec-style container healthcheck cannot probe the ops
// listener from outside the binary; the binary probes itself instead, and the
// image's HEALTHCHECK execs it (#145).
package probe

import (
	"fmt"
	"net"
	"net/http"
	"time"
)

// Ready GETs http://<addr>/ready and returns nil only on a 200 — the ops
// listener's signal that a valid bundle is loaded. addr is the configured ops
// listen address (WAVEFRONT_METRICS_ADDR); a wildcard listen host is rewritten
// to loopback so the same value serves both purposes. Any other outcome —
// non-200, connection refused, timeout — is an error, which the command maps
// to a non-zero exit.
func Ready(addr string, timeout time.Duration) error {
	client := &http.Client{Timeout: timeout}
	resp, err := client.Get("http://" + dialableHostPort(addr) + "/ready")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("/ready returned %d", resp.StatusCode)
	}
	return nil
}

// dialableHostPort rewrites a wildcard listen host (0.0.0.0, ::, or empty) to
// loopback. A listen address accepts on all interfaces, but a dial needs a
// concrete destination; inside the container they are the same network
// namespace, so loopback reaches the listener.
func dialableHostPort(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	switch host {
	case "", "0.0.0.0", "::":
		return net.JoinHostPort("127.0.0.1", port)
	}
	return addr
}
