package e2e

import (
	"bytes"
	"io"
	"net/http"
	"regexp"
	"strings"
	"testing"
)

func TestMetricsErrorsLabeledByCode(t *testing.T) {
	// Claim: wavefront_errors_total is labeled by error `code`, and the
	// emitted code matches the X-Wavefront-Error the client sees.
	h := Spawn(t, SpawnOpts{})

	// Trigger unsupported_contract_version to populate the counter.
	req, _ := http.NewRequest(http.MethodPost, h.Proxy.URL, strings.NewReader("ignored"))
	req.Header.Set("X-Api-Contract-Version", "1999-01")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	mresp, err := http.Get(h.Ops.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer mresp.Body.Close()
	body, _ := io.ReadAll(mresp.Body)

	re := regexp.MustCompile(`wavefront_errors_total\{[^}]*code="unsupported_contract_version"[^}]*\}\s+(\d+)`)
	m := re.FindStringSubmatch(string(body))
	if m == nil {
		t.Fatalf(`metrics missing wavefront_errors_total{code="unsupported_contract_version"}; body:\n%s`, body)
	}
	if m[1] == "0" {
		t.Errorf(`wavefront_errors_total{code="unsupported_contract_version"} = 0, want >= 1`)
	}
}

func TestMetricsRequestsCounterIncrements(t *testing.T) {
	// Claim: wavefront_requests_total increments once per proxy request,
	// regardless of outcome.
	h := Spawn(t, SpawnOpts{
		BackendHandler: func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"text":"x"}`)
		},
	})

	const n = 3
	for i := 0; i < n; i++ {
		req, _ := http.NewRequest(http.MethodPost, h.Proxy.URL, bytes.NewReader(PingBytes(t, h.Bundle, "hi", 1)))
		req.Header.Set("X-Api-Contract-Version", "2024-11")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
		resp.Body.Close()
	}

	mresp, err := http.Get(h.Ops.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer mresp.Body.Close()
	body, _ := io.ReadAll(mresp.Body)

	re := regexp.MustCompile(`wavefront_requests_total\s+(\d+)`)
	m := re.FindStringSubmatch(string(body))
	if m == nil {
		t.Fatalf("metrics missing wavefront_requests_total; body:\n%s", body)
	}
	if m[1] != "3" {
		t.Errorf("wavefront_requests_total = %s, want 3", m[1])
	}
}
