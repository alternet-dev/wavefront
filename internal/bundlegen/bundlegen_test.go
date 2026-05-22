package bundlegen_test

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/alternet-dev/wavefront/internal/bundle"
	"github.com/alternet-dev/wavefront/internal/bundlegen"
	"github.com/alternet-dev/wavefront/internal/bundletest"
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

// --- Retire tests ---

// resolutionOverrideCount parses resolution.yaml in bundleDir and returns the
// number of overrides present.
func resolutionOverrideCount(t *testing.T, bundleDir string) int {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(bundleDir, "resolution.yaml"))
	if err != nil {
		t.Fatalf("read resolution.yaml: %v", err)
	}
	var rf struct {
		Overrides []struct {
			ContractVersion string `yaml:"contract_version"`
			Transform       *struct {
				Target   string `yaml:"target"`
				Request  any    `yaml:"request"`
				Response any    `yaml:"response"`
			} `yaml:"transform"`
		} `yaml:"overrides"`
	}
	if err := yaml.Unmarshal(raw, &rf); err != nil {
		t.Fatalf("parse resolution.yaml: %v", err)
	}
	return len(rf.Overrides)
}

// resolutionTransformFor returns the transform block for a given version in
// resolution.yaml, or fails the test if none is found.
func resolutionTransformFor(t *testing.T, bundleDir, version string) struct {
	Target   string `yaml:"target"`
	Request  any    `yaml:"request"`
	Response any    `yaml:"response"`
} {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(bundleDir, "resolution.yaml"))
	if err != nil {
		t.Fatalf("read resolution.yaml: %v", err)
	}
	var rf struct {
		Overrides []struct {
			ContractVersion string `yaml:"contract_version"`
			Transform       *struct {
				Target   string `yaml:"target"`
				Request  any    `yaml:"request"`
				Response any    `yaml:"response"`
			} `yaml:"transform"`
		} `yaml:"overrides"`
	}
	if err := yaml.Unmarshal(raw, &rf); err != nil {
		t.Fatalf("parse resolution.yaml: %v", err)
	}
	for _, ov := range rf.Overrides {
		if ov.ContractVersion == version && ov.Transform != nil {
			return *ov.Transform
		}
	}
	t.Fatalf("no transform override found for %q in resolution.yaml", version)
	return struct {
		Target   string `yaml:"target"`
		Request  any    `yaml:"request"`
		Response any    `yaml:"response"`
	}{}
}

func TestRetireScaffoldsAnOverride(t *testing.T) {
	bundleDir := t.TempDir()
	addLayer(t, bundleDir, "2024-01")
	addLayer(t, bundleDir, "2025-06")
	addLayer(t, bundleDir, "2026-11")

	if err := bundlegen.Retire(bundleDir, "2024-01"); err != nil {
		t.Fatalf("Retire: %v", err)
	}

	// resolution.yaml must exist with one override.
	if count := resolutionOverrideCount(t, bundleDir); count != 1 {
		t.Fatalf("override count = %d, want 1", count)
	}

	tr := resolutionTransformFor(t, bundleDir, "2024-01")
	if tr.Target != "2025-06" {
		t.Errorf("transform target = %q, want %q", tr.Target, "2025-06")
	}
	// Request and response must be present as empty sequences.
	if tr.Request == nil {
		t.Error("transform.request must be present (empty sequence)")
	}
	if tr.Response == nil {
		t.Error("transform.response must be present (empty sequence)")
	}
}

func TestRetireRefusesTheCurrentVersion(t *testing.T) {
	bundleDir := t.TempDir()
	addLayer(t, bundleDir, "2024-01")
	addLayer(t, bundleDir, "2026-11")

	if err := bundlegen.Retire(bundleDir, "2026-11"); err == nil {
		t.Fatal("Retire of the newest/current version: expected a hard error, got nil")
	}
}

func TestRetireRefusesAnUnknownVersion(t *testing.T) {
	bundleDir := t.TempDir()
	addLayer(t, bundleDir, "2024-01")
	addLayer(t, bundleDir, "2026-11")

	if err := bundlegen.Retire(bundleDir, "2020-01"); err == nil {
		t.Fatal("Retire of an absent version: expected a hard error, got nil")
	}
}

func TestRetireRefusesAnAlreadyOverriddenVersion(t *testing.T) {
	bundleDir := t.TempDir()
	addLayer(t, bundleDir, "2024-01")
	addLayer(t, bundleDir, "2026-11")

	// First retire succeeds.
	if err := bundlegen.Retire(bundleDir, "2024-01"); err != nil {
		t.Fatalf("first Retire: %v", err)
	}
	// Second retire of the same version must fail.
	if err := bundlegen.Retire(bundleDir, "2024-01"); err == nil {
		t.Fatal("second Retire of the same version: expected a hard error, got nil")
	}
}

func TestRetirePreservesExistingOverrides(t *testing.T) {
	// Use a real bundle so we can call bundle.Load at the end.
	fds := bundletest.FDSBytes(t)
	openapi := bundletest.ValidOpenAPI
	const versionsA = `version: 1
contracts:
  - contract_version: "2024-01"
    route: /v3/echo
    method: POST
    request_message: acme.v1.Ping
    response_message: acme.v1.Pong
`
	const versionsB = `version: 1
contracts:
  - contract_version: "2025-06"
    route: /v3/echo
    method: POST
    request_message: acme.v1.Ping
    response_message: acme.v1.Pong
`
	const versionsC = `version: 1
contracts:
  - contract_version: "2026-11"
    route: /v3/echo
    method: POST
    request_message: acme.v1.Ping
    response_message: acme.v1.Pong
`
	bundleDir := bundletest.MultiDir(t,
		bundletest.Layer{Name: "2024-01", Descriptors: fds, OpenAPI: openapi, Versions: versionsA},
		bundletest.Layer{Name: "2025-06", Descriptors: fds, OpenAPI: openapi, Versions: versionsB},
		bundletest.Layer{Name: "2026-11", Descriptors: fds, OpenAPI: openapi, Versions: versionsC},
	)

	// Seed a pre-existing override for 2025-06 (a route override).
	bundletest.WriteResolution(t, bundleDir, `version: 1
overrides:
  - contract_version: "2025-06"
    transform:
      target: "2026-11"
      request: []
      response: []
`)

	// Retire 2024-01 — must preserve the 2025-06 override.
	if err := bundlegen.Retire(bundleDir, "2024-01"); err != nil {
		t.Fatalf("Retire: %v", err)
	}

	// Both overrides must be present.
	if count := resolutionOverrideCount(t, bundleDir); count != 2 {
		t.Fatalf("override count = %d after retire, want 2", count)
	}

	// The new override for 2024-01 must have the right target.
	tr := resolutionTransformFor(t, bundleDir, "2024-01")
	if tr.Target != "2025-06" {
		t.Errorf("transform target for 2024-01 = %q, want %q", tr.Target, "2025-06")
	}

	// The whole bundle must still load cleanly (chains are valid).
	if _, err := bundle.Load(bundleDir); err != nil {
		t.Fatalf("bundle.Load after retire: %v", err)
	}
}

// --- Verify tests ---

func TestVerifyAcceptsAGoodBundle(t *testing.T) {
	// bundletest.Dir builds a single-layer bundle that Load accepts.
	bundleDir := bundletest.Dir(t, "")
	if err := bundlegen.Verify(bundleDir); err != nil {
		t.Fatalf("Verify on a good bundle: %v", err)
	}
}

func TestVerifyRejectsABrokenBundle(t *testing.T) {
	// Start with a valid bundle, then attach a resolution.yaml override that
	// names a version not present in the bundle — Load will reject it.
	bundleDir := bundletest.Dir(t, "")
	bundletest.WriteResolution(t, bundleDir, `version: 1
overrides:
  - contract_version: "9999-99"
    transform:
      target: "2024-11"
      request: []
      response: []
`)
	if err := bundlegen.Verify(bundleDir); err == nil {
		t.Fatal("Verify on a bundle with an unknown override version: expected a non-nil error, got nil")
	}
}
