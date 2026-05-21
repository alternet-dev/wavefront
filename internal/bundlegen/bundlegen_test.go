package bundlegen_test

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alternet-dev/wavefront/internal/bundle"
	"github.com/alternet-dev/wavefront/internal/bundlegen"
)

const sampleOpenAPI = `{
  "openapi": "3.0.0",
  "info": {"title": "acme", "version": "2026-05-17"},
  "paths": {
    "/v3/echo": {
      "post": {
        "operationId": "echo",
        "requestBody": {"content": {"application/json": {"schema": {"$ref": "#/components/schemas/EchoRequest"}}}},
        "responses": {"200": {"content": {"application/json": {"schema": {"$ref": "#/components/schemas/EchoReply"}}}}}
      }
    }
  },
  "components": {
    "schemas": {
      "EchoRequest": {"type": "object", "properties": {
        "text": {"type": "string"},
        "count": {"type": "integer", "format": "int64"}
      }},
      "EchoReply": {"type": "object", "properties": {
        "text": {"type": "string"},
        "note": {"type": "string", "nullable": true},
        "tags": {"type": "array", "items": {"type": "string"}},
        "inner": {"$ref": "#/components/schemas/Inner"}
      }},
      "Inner": {"type": "object", "properties": {"v": {"type": "string"}}}
    }
  }
}`

func writeOpenAPI(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "openapi.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatalf("write openapi: %v", err)
	}
	return p
}

func TestAddFromURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(sampleOpenAPI))
	}))
	defer srv.Close()

	out := t.TempDir()
	if err := bundlegen.Add(srv.URL+"/openapi.json", out); err != nil {
		t.Fatalf("Add from URL: %v", err)
	}
	b, err := bundle.Load(out)
	if err != nil {
		t.Fatalf("generated bundle did not load: %v", err)
	}
	if _, ok := b.Contract("2026-05-17"); !ok {
		t.Fatal(`Contract("2026-05-17") not found`)
	}
}

func TestAddFromURLNon200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	if err := bundlegen.Add(srv.URL, t.TempDir()); err == nil {
		t.Fatal("expected a hard error on non-200 OpenAPI fetch, got nil")
	}
}

func TestAddProducesLoadableLayer(t *testing.T) {
	in := writeOpenAPI(t, sampleOpenAPI)
	out := t.TempDir()
	if err := bundlegen.Add(in, out); err != nil {
		t.Fatalf("Add: %v", err)
	}
	layerDir := filepath.Join(out, "2026-05-17")
	for _, f := range []string{"descriptors.binpb", "openapi.json", "versions.yaml"} {
		if _, err := os.Stat(filepath.Join(layerDir, f)); err != nil {
			t.Fatalf("missing %s in layer dir: %v", f, err)
		}
	}

	b, err := bundle.Load(out)
	if err != nil {
		t.Fatalf("generated bundle did not load: %v", err)
	}
	c, ok := b.Contract("2026-05-17")
	if !ok {
		t.Fatal(`Contract("2026-05-17") not found`)
	}
	if c.Route() != "/v3/echo" || c.Method() != "POST" {
		t.Errorf("route/method = %q %q", c.Route(), c.Method())
	}
	if !strings.HasSuffix(c.RequestMessage(), ".EchoRequest") {
		t.Errorf("request_message = %q", c.RequestMessage())
	}
	if !strings.HasSuffix(c.ResponseMessage(), ".EchoReply") {
		t.Errorf("response_message = %q", c.ResponseMessage())
	}

	rm, err := b.Message(c.RequestMessage())
	if err != nil {
		t.Fatalf("resolve request message: %v", err)
	}
	if rm.Fields().ByName("text") == nil || rm.Fields().ByName("count") == nil {
		t.Error("EchoRequest missing fields text/count")
	}
	if f := rm.Fields().ByName("count"); f != nil && f.Kind().String() != "int64" {
		t.Errorf("count kind = %s, want int64", f.Kind())
	}

	rp, err := b.Message(c.ResponseMessage())
	if err != nil {
		t.Fatalf("resolve response message: %v", err)
	}
	tags := rp.Fields().ByName("tags")
	if tags == nil || !tags.IsList() {
		t.Error("EchoReply.tags should be a repeated field")
	}
	note := rp.Fields().ByName("note")
	if note == nil || !note.HasPresence() {
		t.Error("EchoReply.note (nullable) should be a proto3 optional with presence")
	}
	inner := rp.Fields().ByName("inner")
	if inner == nil || inner.Message() == nil || !strings.HasSuffix(string(inner.Message().FullName()), ".Inner") {
		t.Error("EchoReply.inner should be a message field referencing Inner")
	}
}

func TestAddIsDeterministic(t *testing.T) {
	in := writeOpenAPI(t, sampleOpenAPI)
	o1, o2 := t.TempDir(), t.TempDir()
	if err := bundlegen.Add(in, o1); err != nil {
		t.Fatalf("add1: %v", err)
	}
	if err := bundlegen.Add(in, o2); err != nil {
		t.Fatalf("add2: %v", err)
	}
	a, _ := os.ReadFile(filepath.Join(o1, "2026-05-17", "descriptors.binpb"))
	b, _ := os.ReadFile(filepath.Join(o2, "2026-05-17", "descriptors.binpb"))
	if string(a) != string(b) {
		t.Error("descriptors.binpb is not byte-reproducible across runs")
	}
}

func TestAddHardErrors(t *testing.T) {
	cases := map[string]string{
		"oneOf": `{"openapi":"3.0.0","info":{"version":"1"},"paths":{"/x":{"post":{
			"requestBody":{"content":{"application/json":{"schema":{"$ref":"#/components/schemas/A"}}}},
			"responses":{"200":{"content":{"application/json":{"schema":{"$ref":"#/components/schemas/A"}}}}}}}},
			"components":{"schemas":{"A":{"type":"object","properties":{"x":{"oneOf":[{"type":"string"}]}}}}}}`,
		"additionalProperties": `{"openapi":"3.0.0","info":{"version":"1"},"paths":{"/x":{"post":{
			"requestBody":{"content":{"application/json":{"schema":{"$ref":"#/components/schemas/A"}}}},
			"responses":{"200":{"content":{"application/json":{"schema":{"$ref":"#/components/schemas/A"}}}}}}}},
			"components":{"schemas":{"A":{"type":"object","additionalProperties":true,"properties":{"x":{"type":"string"}}}}}}`,
		"no requestBody": `{"openapi":"3.0.0","info":{"version":"1"},"paths":{"/x":{"post":{
			"responses":{"200":{"content":{"application/json":{"schema":{"$ref":"#/components/schemas/A"}}}}}}}},
			"components":{"schemas":{"A":{"type":"object","properties":{"x":{"type":"string"}}}}}}`,
		"two operations": `{"openapi":"3.0.0","info":{"version":"1"},"paths":{
			"/x":{"post":{"requestBody":{"content":{"application/json":{"schema":{"$ref":"#/components/schemas/A"}}}},"responses":{"200":{"content":{"application/json":{"schema":{"$ref":"#/components/schemas/A"}}}}}}},
			"/y":{"post":{"requestBody":{"content":{"application/json":{"schema":{"$ref":"#/components/schemas/A"}}}},"responses":{"200":{"content":{"application/json":{"schema":{"$ref":"#/components/schemas/A"}}}}}}}},
			"components":{"schemas":{"A":{"type":"object","properties":{"x":{"type":"string"}}}}}}`,
		"inline non-ref schema": `{"openapi":"3.0.0","info":{"version":"1"},"paths":{"/x":{"post":{
			"requestBody":{"content":{"application/json":{"schema":{"type":"object","properties":{"x":{"type":"string"}}}}}},
			"responses":{"200":{"content":{"application/json":{"schema":{"$ref":"#/components/schemas/A"}}}}}}}},
			"components":{"schemas":{"A":{"type":"object","properties":{"x":{"type":"string"}}}}}}`,
		"untyped property": `{"openapi":"3.0.0","info":{"version":"1"},"paths":{"/x":{"post":{
			"requestBody":{"content":{"application/json":{"schema":{"$ref":"#/components/schemas/A"}}}},
			"responses":{"200":{"content":{"application/json":{"schema":{"$ref":"#/components/schemas/A"}}}}}}}},
			"components":{"schemas":{"A":{"type":"object","properties":{"x":{}}}}}}`,
		"missing info.version": `{"openapi":"3.0.0","info":{"title":"x"},"paths":{"/x":{"post":{
			"requestBody":{"content":{"application/json":{"schema":{"$ref":"#/components/schemas/A"}}}},
			"responses":{"200":{"content":{"application/json":{"schema":{"$ref":"#/components/schemas/A"}}}}}}}},
			"components":{"schemas":{"A":{"type":"object","properties":{"x":{"type":"string"}}}}}}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			in := writeOpenAPI(t, body)
			if err := bundlegen.Add(in, t.TempDir()); err == nil {
				t.Fatalf("%s: expected a hard error, got nil", name)
			}
		})
	}
}

// addLayer writes a minimal valid layer <bundleDir>/<version>/ by hand —
// Remove only inspects the directory layout, not the layer contents.
func addLayer(t *testing.T, bundleDir, version string) {
	t.Helper()
	ld := filepath.Join(bundleDir, version)
	if err := os.MkdirAll(ld, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", ld, err)
	}
	if err := os.WriteFile(filepath.Join(ld, "versions.yaml"), []byte("version: 1\n"), 0o600); err != nil {
		t.Fatalf("write versions.yaml: %v", err)
	}
}

func TestRemoveDeletesTheOldestLayer(t *testing.T) {
	bundleDir := t.TempDir()
	addLayer(t, bundleDir, "2024-11")
	addLayer(t, bundleDir, "2026-05")
	if err := bundlegen.Remove(bundleDir, "2024-11"); err != nil {
		t.Fatalf("Remove oldest: %v", err)
	}
	if _, err := os.Stat(filepath.Join(bundleDir, "2024-11")); !os.IsNotExist(err) {
		t.Fatal("layer 2024-11 still present after Remove")
	}
	if _, err := os.Stat(filepath.Join(bundleDir, "2026-05")); err != nil {
		t.Fatalf("layer 2026-05 should remain: %v", err)
	}
}

func TestRemoveRefusesANonOldestLayer(t *testing.T) {
	bundleDir := t.TempDir()
	addLayer(t, bundleDir, "2024-11")
	addLayer(t, bundleDir, "2026-05")
	if err := bundlegen.Remove(bundleDir, "2026-05"); err == nil {
		t.Fatal("Remove of a non-oldest layer: expected a hard error, got nil")
	}
	if _, err := os.Stat(filepath.Join(bundleDir, "2026-05")); err != nil {
		t.Fatalf("layer 2026-05 must remain after a refused Remove: %v", err)
	}
}

func TestRemoveRefusesTheOnlyLayer(t *testing.T) {
	bundleDir := t.TempDir()
	addLayer(t, bundleDir, "2024-11")
	if err := bundlegen.Remove(bundleDir, "2024-11"); err == nil {
		t.Fatal("Remove of the only/current layer: expected a hard error, got nil")
	}
}

func TestRemoveRefusesAnUnknownVersion(t *testing.T) {
	bundleDir := t.TempDir()
	addLayer(t, bundleDir, "2024-11")
	addLayer(t, bundleDir, "2026-05")
	if err := bundlegen.Remove(bundleDir, "2030-01"); err == nil {
		t.Fatal("Remove of an absent version: expected a hard error, got nil")
	}
}

func TestAddRefusesToOverwriteAnExistingVersion(t *testing.T) {
	in := writeOpenAPI(t, sampleOpenAPI)
	bundleDir := t.TempDir()
	if err := bundlegen.Add(in, bundleDir); err != nil {
		t.Fatalf("first Add: %v", err)
	}
	if err := bundlegen.Add(in, bundleDir); err == nil {
		t.Fatal("second Add of the same version: expected a hard error, got nil")
	}
}
