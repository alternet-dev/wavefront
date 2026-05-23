package e2e

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
)

// TestRequestLoggingEmitsStructuredLine — one structured log line per proxied
// request, carrying the operational fields an operator needs to attribute a
// failure to a cohort: contract_version, route, target, resolution_kind,
// upstream_status, outcome, latency_ms.
func TestRequestLoggingEmitsStructuredLine(t *testing.T) {
	h := Spawn(t, SpawnOpts{
		BackendHandler: func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"text":"x"}`)
		},
	})

	buf := &bytes.Buffer{}
	h.Server.SetLogger(slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelInfo})))

	req, _ := http.NewRequest(http.MethodPost, h.Proxy.URL, bytes.NewReader(PingBytes(t, h.Bundle, "hi", 1)))
	req.Header.Set("X-Api-Contract-Version", "2024-11")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	entry := exactlyOneLogLine(t, buf.String())
	expectString(t, entry, "contract_version", "2024-11")
	expectString(t, entry, "route", "/v3/echo")
	expectString(t, entry, "resolution_kind", "route")
	expectString(t, entry, "outcome", "ok")
	expectString(t, entry, "level", "INFO")
	if got, _ := entry["upstream_status"].(float64); int(got) != 200 {
		t.Errorf("upstream_status = %v, want 200", entry["upstream_status"])
	}
	if got, ok := entry["latency_ms"].(float64); !ok || got < 0 {
		t.Errorf("latency_ms = %v, want non-negative number", entry["latency_ms"])
	}
}

// TestRequestLoggingMarksUnsupportedVersionAsWarn — a contract-negotiation
// failure logs at WARN with outcome=unsupported_contract_version and
// contract_version=unknown; upstream_status stays 0 because no upstream call
// happened.
func TestRequestLoggingMarksUnsupportedVersionAsWarn(t *testing.T) {
	h := Spawn(t, SpawnOpts{})

	buf := &bytes.Buffer{}
	h.Server.SetLogger(slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelInfo})))

	req, _ := http.NewRequest(http.MethodPost, h.Proxy.URL, strings.NewReader("ignored"))
	req.Header.Set("X-Api-Contract-Version", "9999-99")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	entry := exactlyOneLogLine(t, buf.String())
	expectString(t, entry, "contract_version", "unknown")
	expectString(t, entry, "outcome", "unsupported_contract_version")
	expectString(t, entry, "level", "WARN")
	if got, _ := entry["upstream_status"].(float64); int(got) != 0 {
		t.Errorf("upstream_status = %v, want 0 (no upstream call on pre-negotiate failure)", entry["upstream_status"])
	}
}

func exactlyOneLogLine(t *testing.T, raw string) map[string]any {
	t.Helper()
	lines := []string{}
	for _, line := range strings.Split(raw, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			lines = append(lines, line)
		}
	}
	if len(lines) != 1 {
		t.Fatalf("expected 1 log line, got %d:\n%s", len(lines), raw)
	}
	var entry map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &entry); err != nil {
		t.Fatalf("log line not JSON: %v\n%s", err, lines[0])
	}
	return entry
}

func expectString(t *testing.T, entry map[string]any, key, want string) {
	t.Helper()
	got, _ := entry[key].(string)
	if got != want {
		t.Errorf("%s = %q, want %q (entry=%v)", key, got, want, entry)
	}
}
