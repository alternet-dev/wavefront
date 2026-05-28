package e2e

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestUnsupportedContractVersionReturns400(t *testing.T) {
	// Claim: an unknown / missing contract version returns the typed
	// unsupported_contract_version error (HTTP 400), never a silent fallback.
	h := Spawn(t, SpawnOpts{})

	req, _ := http.NewRequest(http.MethodPost, h.Proxy.URL+"/v3/echo", strings.NewReader("ignored"))
	req.Header.Set("X-Api-Contract-Version", "1999-01")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	if resp.Header.Get("X-Wavefront-Error") != "unsupported_contract_version" {
		t.Errorf("X-Wavefront-Error = %q", resp.Header.Get("X-Wavefront-Error"))
	}
	if body, _ := io.ReadAll(resp.Body); len(body) == 0 {
		t.Error("wavefront.v0.Error body should be non-empty")
	}
}
