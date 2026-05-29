package bundlegen_test

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/alternet-dev/wavefront/internal/bundle"
	"github.com/alternet-dev/wavefront/internal/bundlegen"
)

// multiOpOpenAPI is a 3-route OpenAPI with a shared schema (Item) referenced
// by every operation. It exercises the per-op walk, schema dedup across
// ops, and the deterministic contracts-sort.
const multiOpOpenAPI = `{
  "openapi": "3.0.0",
  "info": {"title": "acme", "version": "2026-05-17"},
  "paths": {
    "/v3/items": {
      "post": {
        "operationId": "createItem",
        "requestBody": {"content": {"application/json": {"schema": {"$ref": "#/components/schemas/CreateItemRequest"}}}},
        "responses": {"200": {"content": {"application/json": {"schema": {"$ref": "#/components/schemas/Item"}}}}}
      }
    },
    "/v3/items/{id}": {
      "get": {
        "operationId": "getItem",
        "requestBody": {"content": {"application/json": {"schema": {"$ref": "#/components/schemas/GetItemRequest"}}}},
        "responses": {"200": {"content": {"application/json": {"schema": {"$ref": "#/components/schemas/Item"}}}}}
      },
      "delete": {
        "operationId": "deleteItem",
        "requestBody": {"content": {"application/json": {"schema": {"$ref": "#/components/schemas/DeleteItemRequest"}}}},
        "responses": {"200": {"content": {"application/json": {"schema": {"$ref": "#/components/schemas/DeleteItemReply"}}}}}
      }
    }
  },
  "components": {
    "schemas": {
      "CreateItemRequest": {"type": "object", "properties": {
        "name": {"type": "string"},
        "tags": {"type": "array", "items": {"type": "string"}}
      }},
      "GetItemRequest": {"type": "object", "properties": {
        "id": {"type": "string"}
      }},
      "DeleteItemRequest": {"type": "object", "properties": {
        "id": {"type": "string"}
      }},
      "DeleteItemReply": {"type": "object", "properties": {
        "deleted": {"type": "boolean"}
      }},
      "Item": {"type": "object", "properties": {
        "id": {"type": "string"},
        "name": {"type": "string"},
        "tags": {"type": "array", "items": {"type": "string"}}
      }}
    }
  }
}`

// TestAddMultiRouteProducesAllContracts walks every (path, method) in a
// multi-route OpenAPI and emits one contract per operation, plus the union
// of referenced schemas in the descriptor set. The output round-trips
// through bundle.Load so the proxy can consume it.
func TestAddMultiRouteProducesAllContracts(t *testing.T) {
	in := writeOpenAPI(t, multiOpOpenAPI)
	out := t.TempDir()
	if err := bundlegen.Add(in, out, false); err != nil {
		t.Fatalf("Add: %v", err)
	}

	layerDir := filepath.Join(out, "2026-05-17")
	for _, f := range []string{"descriptors.binpb", "openapi.json", "versions.yaml"} {
		if _, err := os.Stat(filepath.Join(layerDir, f)); err != nil {
			t.Fatalf("missing %s in layer dir: %v", f, err)
		}
	}

	// versions.yaml must list all 3 contracts, sorted by (path, method).
	raw, err := os.ReadFile(filepath.Join(layerDir, "versions.yaml"))
	if err != nil {
		t.Fatalf("read versions.yaml: %v", err)
	}
	var yb struct {
		Version   int `yaml:"version"`
		Contracts []struct {
			ContractVersion string `yaml:"contract_version"`
			Route           string `yaml:"route"`
			Method          string `yaml:"method"`
			RequestMessage  string `yaml:"request_message"`
			ResponseMessage string `yaml:"response_message"`
		} `yaml:"contracts"`
	}
	if err := yaml.Unmarshal(raw, &yb); err != nil {
		t.Fatalf("parse versions.yaml: %v", err)
	}
	if yb.Version != 1 {
		t.Errorf("versions.yaml version = %d, want 1", yb.Version)
	}
	if len(yb.Contracts) != 3 {
		t.Fatalf("contract count = %d, want 3", len(yb.Contracts))
	}

	// Sorted by (path, method) ascending: /v3/items POST, /v3/items/{id} DELETE,
	// /v3/items/{id} GET.
	want := []struct {
		route, method string
		req, resp     string
	}{
		{"/v3/items", "POST", ".CreateItemRequest", ".Item"},
		{"/v3/items/{id}", "DELETE", ".DeleteItemRequest", ".DeleteItemReply"},
		{"/v3/items/{id}", "GET", ".GetItemRequest", ".Item"},
	}
	for i, w := range want {
		got := yb.Contracts[i]
		if got.Route != w.route || got.Method != w.method {
			t.Errorf("contract[%d] route/method = %q %q, want %q %q",
				i, got.Route, got.Method, w.route, w.method)
		}
		if got.ContractVersion != "2026-05-17" {
			t.Errorf("contract[%d] contract_version = %q, want %q", i, got.ContractVersion, "2026-05-17")
		}
		if !strings.HasSuffix(got.RequestMessage, w.req) {
			t.Errorf("contract[%d] request_message = %q, want suffix %q", i, got.RequestMessage, w.req)
		}
		if !strings.HasSuffix(got.ResponseMessage, w.resp) {
			t.Errorf("contract[%d] response_message = %q, want suffix %q", i, got.ResponseMessage, w.resp)
		}
	}

	// Round-trip through bundle.Load to confirm the proxy can consume it.
	b, err := bundle.Load(out)
	if err != nil {
		t.Fatalf("generated bundle did not load: %v", err)
	}
	// Single contract_version across all ops — only one version layer here,
	// but it contains 3 operations.
	if vs := b.Versions(); len(vs) != 1 || vs[0] != "2026-05-17" {
		t.Errorf("Versions() = %v, want [2026-05-17]", vs)
	}

	// LookupRoute must find each binding.
	for _, w := range want {
		c, ok := b.LookupRoute(w.route, w.method)
		if !ok {
			t.Errorf("LookupRoute(%q, %q): not found", w.route, w.method)
			continue
		}
		if !strings.HasSuffix(c.RequestMessage(), w.req) {
			t.Errorf("LookupRoute(%q, %q).RequestMessage() = %q, want suffix %q",
				w.route, w.method, c.RequestMessage(), w.req)
		}
		if !strings.HasSuffix(c.ResponseMessage(), w.resp) {
			t.Errorf("LookupRoute(%q, %q).ResponseMessage() = %q, want suffix %q",
				w.route, w.method, c.ResponseMessage(), w.resp)
		}
	}

	// Descriptors must contain the shared schema (Item) once and every
	// referenced schema. Check by FQ name.
	wantMsgs := []string{
		"wavefront.gen.v2026_05_17.CreateItemRequest",
		"wavefront.gen.v2026_05_17.GetItemRequest",
		"wavefront.gen.v2026_05_17.DeleteItemRequest",
		"wavefront.gen.v2026_05_17.DeleteItemReply",
		"wavefront.gen.v2026_05_17.Item",
	}
	for _, name := range wantMsgs {
		if _, err := b.Message(name); err != nil {
			t.Errorf("descriptor missing message %q: %v", name, err)
		}
	}
}

// TestAddMultiRouteIsDeterministic emits the same multi-route OpenAPI
// twice; descriptors.binpb and versions.yaml must be byte-identical. The
// consumer's buf-breaking gate relies on this.
func TestAddMultiRouteIsDeterministic(t *testing.T) {
	in := writeOpenAPI(t, multiOpOpenAPI)
	o1, o2 := t.TempDir(), t.TempDir()
	if err := bundlegen.Add(in, o1, false); err != nil {
		t.Fatalf("add1: %v", err)
	}
	if err := bundlegen.Add(in, o2, false); err != nil {
		t.Fatalf("add2: %v", err)
	}
	for _, f := range []string{"descriptors.binpb", "versions.yaml", "openapi.json"} {
		a, _ := os.ReadFile(filepath.Join(o1, "2026-05-17", f))
		b, _ := os.ReadFile(filepath.Join(o2, "2026-05-17", f))
		if string(a) != string(b) {
			t.Errorf("%s is not byte-reproducible across runs", f)
		}
	}
}

// TestAddEmptyOpenAPIIsAHardError: an OpenAPI doc with zero operations
// can't emit a layer with no contracts (versions.yaml.contracts must be
// non-empty per the bundle schema).
func TestAddEmptyOpenAPIIsAHardError(t *testing.T) {
	const empty = `{"openapi":"3.0.0","info":{"version":"1"},"paths":{},"components":{"schemas":{}}}`
	in := writeOpenAPI(t, empty)
	if err := bundlegen.Add(in, t.TempDir(), false); err == nil {
		t.Fatal("expected a hard error on a zero-operation OpenAPI, got nil")
	}
}

// TestAddDedupesSharedSchemas confirms a schema referenced by multiple
// operations (Item is the response of POST /v3/items and the response of
// GET /v3/items/{id}) appears exactly once in the descriptor set.
func TestAddDedupesSharedSchemas(t *testing.T) {
	in := writeOpenAPI(t, multiOpOpenAPI)
	out := t.TempDir()
	if err := bundlegen.Add(in, out, false); err != nil {
		t.Fatalf("Add: %v", err)
	}
	b, err := bundle.Load(out)
	if err != nil {
		t.Fatalf("bundle.Load: %v", err)
	}
	// Two ops reference Item; the FileDescriptorSet must hold one Item
	// message, not two. Resolving the FQ name must succeed exactly once
	// (it would also succeed once if there were a duplicate, but the
	// descriptor merge would fail — bundle.Load would have rejected it
	// upstream).
	if _, err := b.Message("wavefront.gen.v2026_05_17.Item"); err != nil {
		t.Errorf("shared schema Item not found: %v", err)
	}
}

// TestAddVersionsYAMLIsSorted confirms the ordering invariant: contracts
// are sorted by (route, method) ascending. The byte-reproducibility test
// covers determinism end-to-end; this one calls it out at the per-entry
// level so a future regression that produces a stable-but-unsorted order
// gets a targeted failure.
func TestAddVersionsYAMLIsSorted(t *testing.T) {
	in := writeOpenAPI(t, multiOpOpenAPI)
	out := t.TempDir()
	if err := bundlegen.Add(in, out, false); err != nil {
		t.Fatalf("Add: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(out, "2026-05-17", "versions.yaml"))
	if err != nil {
		t.Fatalf("read versions.yaml: %v", err)
	}
	var yb struct {
		Contracts []struct {
			Route  string `yaml:"route"`
			Method string `yaml:"method"`
		} `yaml:"contracts"`
	}
	if err := yaml.Unmarshal(raw, &yb); err != nil {
		t.Fatalf("parse versions.yaml: %v", err)
	}
	keys := make([]string, len(yb.Contracts))
	for i, c := range yb.Contracts {
		keys[i] = c.Route + " " + c.Method
	}
	sortedKeys := make([]string, len(keys))
	copy(sortedKeys, keys)
	sort.Strings(sortedKeys)
	for i := range keys {
		if keys[i] != sortedKeys[i] {
			t.Errorf("contracts not sorted by (route, method); got %v, want %v", keys, sortedKeys)
			break
		}
	}
}
