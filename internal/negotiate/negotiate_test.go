package negotiate_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/alternet-dev/wavefront/internal/bundle"
	"github.com/alternet-dev/wavefront/internal/bundletest"
	"github.com/alternet-dev/wavefront/internal/negotiate"
)

func loadBundle(t *testing.T) *bundle.Bundle {
	t.Helper()
	b, err := bundle.Load(bundletest.Dir(t, ""))
	if err != nil {
		t.Fatalf("load bundle: %v", err)
	}
	return b
}

func TestResolveKnownContract(t *testing.T) {
	b := loadBundle(t)
	// The default bundle binds POST /v3/echo at "2024-11".
	c, werr := negotiate.Resolve(b, "/v3/echo", "POST", "2024-11")
	if werr != nil {
		t.Fatalf("Resolve: %v", werr)
	}
	if c.Route() != "/v3/echo" || c.Method() != "POST" {
		t.Errorf("resolved wrong contract: %q %q", c.Route(), c.Method())
	}
}

func TestResolveMissingHeaderIsUnsupported(t *testing.T) {
	b := loadBundle(t)
	_, werr := negotiate.Resolve(b, "/v3/echo", "POST", "")
	if werr == nil || werr.Code() != "unsupported_contract_version" {
		t.Fatalf("want unsupported_contract_version, got %v", werr)
	}
}

func TestResolveBlankHeaderIsUnsupported(t *testing.T) {
	b := loadBundle(t)
	_, werr := negotiate.Resolve(b, "/v3/echo", "POST", "   ")
	if werr == nil || werr.Code() != "unsupported_contract_version" {
		t.Fatalf("want unsupported_contract_version, got %v", werr)
	}
}

func TestResolveUnknownContractIsUnsupported(t *testing.T) {
	b := loadBundle(t)
	_, werr := negotiate.Resolve(b, "/v3/echo", "POST", "2099-01")
	if werr == nil || werr.Code() != "unsupported_contract_version" {
		t.Fatalf("want unsupported_contract_version, got %v", werr)
	}
	if werr.HTTPStatus() != 400 {
		t.Errorf("status = %d, want 400", werr.HTTPStatus())
	}
}

// TestResolveMultiRouteDispatchesByPathMethod confirms the new resolution key:
// a multi-route bundle with three contracts under one contract_version routes
// to a different contract for each (path, method) — not always to the first
// loaded.
func TestResolveMultiRouteDispatchesByPathMethod(t *testing.T) {
	dir := t.TempDir()
	layer := filepath.Join(dir, "2024-11")
	if err := os.MkdirAll(layer, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Multi-route layer: same contract_version binds POST /a, GET /b,
	// DELETE /b — each with the same Ping/Pong messages (the binding under
	// test is (path, method), not the message).
	versions := `version: 1
contracts:
  - contract_version: "2024-11"
    route: /a
    method: POST
    request_message: acme.v1.Ping
    response_message: acme.v1.Pong
  - contract_version: "2024-11"
    route: /b
    method: GET
    request_message: acme.v1.Ping
    response_message: acme.v1.Pong
  - contract_version: "2024-11"
    route: /b
    method: DELETE
    request_message: acme.v1.Ping
    response_message: acme.v1.Pong
`
	if err := os.WriteFile(filepath.Join(layer, "descriptors.binpb"), bundletest.FDSBytes(t), 0o600); err != nil {
		t.Fatalf("write descriptors: %v", err)
	}
	if err := os.WriteFile(filepath.Join(layer, "openapi.json"), []byte(bundletest.ValidOpenAPI), 0o600); err != nil {
		t.Fatalf("write openapi: %v", err)
	}
	if err := os.WriteFile(filepath.Join(layer, "versions.yaml"), []byte(versions), 0o600); err != nil {
		t.Fatalf("write versions: %v", err)
	}
	b, err := bundle.Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	cases := []struct {
		path, method string
	}{
		{"/a", "POST"},
		{"/b", "GET"},
		{"/b", "DELETE"},
	}
	for _, tc := range cases {
		c, werr := negotiate.Resolve(b, tc.path, tc.method, "2024-11")
		if werr != nil {
			t.Errorf("Resolve(%q, %q, 2024-11): %v", tc.path, tc.method, werr)
			continue
		}
		if c.Route() != tc.path || c.Method() != tc.method {
			t.Errorf("Resolve(%q, %q): got contract for (%q, %q)",
				tc.path, tc.method, c.Route(), c.Method())
		}
	}
}

// TestResolveVersionExistsButDoesNotBindRoute pins the documented choice for
// the edge case in Chunk 5.2: a contract version that exists in the bundle
// but does not bind the requested (path, method) returns
// unsupported_contract_version (400). The route gate at the proxy boundary
// guarantees some version binds (path, method); if the named version is not
// among them, the version-mismatch story is more informative for the client
// than unknown_route.
func TestResolveVersionExistsButDoesNotBindRoute(t *testing.T) {
	// Two layers, each binding a DIFFERENT (path, method):
	//   2024-11: POST /v3/echo
	//   2025-03: GET  /v3/echo
	v1 := `version: 1
contracts:
  - contract_version: "2024-11"
    route: /v3/echo
    method: POST
    request_message: acme.v1.Ping
    response_message: acme.v1.Pong
`
	v2 := `version: 1
contracts:
  - contract_version: "2025-03"
    route: /v3/echo
    method: GET
    request_message: acme.v1.Ping
    response_message: acme.v1.Pong
`
	fds := bundletest.FDSBytes(t)
	dir := bundletest.MultiDir(t,
		bundletest.Layer{Name: "2024-11", Descriptors: fds, OpenAPI: bundletest.ValidOpenAPI, Versions: v1},
		bundletest.Layer{Name: "2025-03", Descriptors: fds, OpenAPI: bundletest.ValidOpenAPI, Versions: v2},
	)
	b, err := bundle.Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// GET /v3/echo + version "2024-11": 2024-11 exists but does not bind
	// GET. The route gate (some version binds GET /v3/echo) has passed at
	// the proxy boundary, so negotiate must surface unsupported_contract_version.
	_, werr := negotiate.Resolve(b, "/v3/echo", "GET", "2024-11")
	if werr == nil {
		t.Fatal("Resolve: want unsupported_contract_version, got nil")
	}
	if werr.Code() != "unsupported_contract_version" {
		t.Errorf("Code = %q, want unsupported_contract_version", werr.Code())
	}
	if werr.HTTPStatus() != 400 {
		t.Errorf("status = %d, want 400", werr.HTTPStatus())
	}
}
