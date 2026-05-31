package bundlegen

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alternet-dev/wavefront/internal/bundle"
)

// errorSchemaNames resolves a route's declared non-2xx response schemas to
// component names, skipping the success side (refSchemaName owns 2xx), the
// capability ceiling (always 502), and non-numeric keys like "default".
func TestErrorSchemaNames(t *testing.T) {
	op := operation{
		Responses: map[string]struct {
			Content map[string]mediaType `json:"content"`
		}{
			"200":     {Content: jsonRef("#/components/schemas/Item")},
			"404":     {Content: jsonRef("#/components/schemas/Problem")},
			"409":     {Content: jsonRef("#/components/schemas/Conflict")},
			"206":     {Content: jsonRef("#/components/schemas/Partial")}, // ceiling → skipped
			"default": {Content: jsonRef("#/components/schemas/Problem")}, // non-numeric → skipped
		},
	}
	got, err := errorSchemaNames(op)
	if err != nil {
		t.Fatalf("errorSchemaNames: %v", err)
	}
	want := map[string]string{"404": "Problem", "409": "Conflict"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for code, name := range want {
		if got[code] != name {
			t.Errorf("got[%q] = %q, want %q", code, got[code], name)
		}
	}
}

// A non-bodyless inline (non-$ref) error schema is rejected, exactly like a
// bodied success side.
func TestErrorSchemaNamesRejectsInline(t *testing.T) {
	op := operation{
		Responses: map[string]struct {
			Content map[string]mediaType `json:"content"`
		}{
			"422": {Content: map[string]mediaType{
				"application/json": {Schema: &schema{Type: "object", Properties: map[string]*schema{"x": {Type: "string"}}}},
			}},
		},
	}
	if _, err := errorSchemaNames(op); err == nil {
		t.Fatal("errorSchemaNames accepted an inline error schema; want rejection")
	}
}

// jsonRef builds an application/json content map pointing at a $ref.
func jsonRef(ref string) map[string]mediaType {
	return map[string]mediaType{"application/json": {Schema: &schema{Ref: ref}}}
}

// A declared 409 with a $ref body must (a) appear in versions.yaml as an
// error_messages binding under the version's package, and (b) have its message
// present in the descriptor set, so bundle.Load resolves the whole layer.
const errorOpenAPI = `{
  "openapi": "3.0.0",
  "info": {"title": "acme", "version": "2027-01-01"},
  "paths": {
    "/v3/widgets": {
      "post": {
        "operationId": "createWidget",
        "requestBody": {"content": {"application/json": {"schema": {"$ref": "#/components/schemas/Widget"}}}},
        "responses": {
          "200": {"content": {"application/json": {"schema": {"$ref": "#/components/schemas/Widget"}}}},
          "409": {"content": {"application/json": {"schema": {"$ref": "#/components/schemas/Problem"}}}}
        }
      }
    }
  },
  "components": {"schemas": {
    "Widget": {"type": "object", "properties": {"id": {"type": "string"}}},
    "Problem": {"type": "object", "properties": {"detail": {"type": "string"}, "status": {"type": "integer"}}}
  }}
}`

func TestAddBindsErrorMessages(t *testing.T) {
	in := filepath.Join(t.TempDir(), "openapi.json")
	if err := os.WriteFile(in, []byte(errorOpenAPI), 0o600); err != nil {
		t.Fatalf("write openapi: %v", err)
	}
	out := t.TempDir()
	if err := Add(in, out, false); err != nil {
		t.Fatalf("Add: %v", err)
	}

	vy, err := os.ReadFile(filepath.Join(out, "2027-01-01", "versions.yaml"))
	if err != nil {
		t.Fatalf("read versions.yaml: %v", err)
	}
	if !strings.Contains(string(vy), "error_messages:") ||
		!strings.Contains(string(vy), `"409": wavefront.gen.v2027_01_01.Problem`) {
		t.Fatalf("versions.yaml missing 409 error binding:\n%s", vy)
	}

	b, err := bundle.Load(out)
	if err != nil {
		t.Fatalf("bundle.Load on generated layer: %v", err)
	}
	c, ok := b.LookupRoute("/v3/widgets", "POST")
	if !ok {
		t.Fatal("route not bound")
	}
	name, ok := c.ErrorMessage(409)
	if !ok || name != "wavefront.gen.v2027_01_01.Problem" {
		t.Fatalf("ErrorMessage(409) = %q, %v; want wavefront.gen.v2027_01_01.Problem, true", name, ok)
	}
	if _, err := b.Message(name); err != nil {
		t.Fatalf("error message %q not in descriptor set: %v", name, err)
	}
}

// A layer with no declared error responses must emit byte-identical
// versions.yaml to the pre-#53 generator: no error_messages block at all.
func TestAddOmitsEmptyErrorBlock(t *testing.T) {
	const plain = `{
  "openapi": "3.0.0",
  "info": {"title": "acme", "version": "2027-02-02"},
  "paths": {"/v3/items/{id}": {"get": {"operationId": "getItem",
    "responses": {"200": {"content": {"application/json": {"schema": {"$ref": "#/components/schemas/Item"}}}}}}}},
  "components": {"schemas": {"Item": {"type": "object", "properties": {"id": {"type": "string"}}}}}
}`
	in := filepath.Join(t.TempDir(), "openapi.json")
	if err := os.WriteFile(in, []byte(plain), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	out := t.TempDir()
	if err := Add(in, out, false); err != nil {
		t.Fatalf("Add: %v", err)
	}
	vy, _ := os.ReadFile(filepath.Join(out, "2027-02-02", "versions.yaml"))
	if strings.Contains(string(vy), "error_messages") {
		t.Errorf("versions.yaml has an error_messages block for an error-free layer:\n%s", vy)
	}
}
