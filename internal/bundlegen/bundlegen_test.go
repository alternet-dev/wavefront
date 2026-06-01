package bundlegen_test

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"
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

// protoName converts a plain string to a protoreflect.Name; a string variable
// does not implicitly convert at a Fields().ByName call site (only an untyped
// string literal does).
func protoName(s string) protoreflect.Name { return protoreflect.Name(s) }

// fullName reports the fully-qualified message name a field references, or a
// placeholder if the field is nil or not a message field — for failure messages.
func fullName(f protoreflect.FieldDescriptor) string {
	if f != nil && f.Message() != nil {
		return string(f.Message().FullName())
	}
	return "<non-message>"
}

func TestAddFromURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(sampleOpenAPI))
	}))
	defer srv.Close()

	out := t.TempDir()
	if err := bundlegen.Add(srv.URL+"/openapi.json", out, false); err != nil {
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
	if err := bundlegen.Add(srv.URL, t.TempDir(), false); err == nil {
		t.Fatal("expected a hard error on non-200 OpenAPI fetch, got nil")
	}
}

func TestAddProducesLoadableLayer(t *testing.T) {
	in := writeOpenAPI(t, sampleOpenAPI)
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
	// The untagged-union fixture embeds google/protobuf/struct.proto, so it
	// guards reproducibility of the WKT-injection path alongside the plain one —
	// the consumer's buf-breaking gate relies on byte-identical output run to run.
	for name, doc := range map[string]string{"plain": sampleOpenAPI, "with struct.proto": untaggedUnionOpenAPI} {
		t.Run(name, func(t *testing.T) {
			in := writeOpenAPI(t, doc)
			o1, o2 := t.TempDir(), t.TempDir()
			if err := bundlegen.Add(in, o1, false); err != nil {
				t.Fatalf("add1: %v", err)
			}
			if err := bundlegen.Add(in, o2, false); err != nil {
				t.Fatalf("add2: %v", err)
			}
			a, _ := os.ReadFile(filepath.Join(o1, "2026-05-17", "descriptors.binpb"))
			b, _ := os.ReadFile(filepath.Join(o2, "2026-05-17", "descriptors.binpb"))
			if string(a) != string(b) {
				t.Error("descriptors.binpb is not byte-reproducible across runs")
			}
		})
	}
}

func TestAddHardErrors(t *testing.T) {
	cases := map[string]string{
		"allOf": `{"openapi":"3.0.0","info":{"version":"1"},"paths":{"/x":{"post":{
			"requestBody":{"content":{"application/json":{"schema":{"$ref":"#/components/schemas/A"}}}},
			"responses":{"200":{"content":{"application/json":{"schema":{"$ref":"#/components/schemas/A"}}}}}}}},
			"components":{"schemas":{"A":{"type":"object","properties":{"x":{"allOf":[{"type":"string"}]}}}}}}`,
		// additionalProperties:true alongside declared properties is the mixed
		// form — an object with known fields PLUS arbitrary extras. A pure open
		// object (#121) lowers to google.protobuf.Struct, but the mixed form has
		// no clean proto lowering and stays a hard error.
		"additionalProperties true mixed with properties": `{"openapi":"3.0.0","info":{"version":"1"},"paths":{"/x":{"post":{
			"requestBody":{"content":{"application/json":{"schema":{"$ref":"#/components/schemas/A"}}}},
			"responses":{"200":{"content":{"application/json":{"schema":{"$ref":"#/components/schemas/A"}}}}}}}},
			"components":{"schemas":{"A":{"type":"object","additionalProperties":true,"properties":{"x":{"type":"string"}}}}}}`,
		"additionalProperties typed dict mixed with properties": `{"openapi":"3.0.0","info":{"version":"1"},"paths":{"/x":{"post":{
			"requestBody":{"content":{"application/json":{"schema":{"$ref":"#/components/schemas/A"}}}},
			"responses":{"200":{"content":{"application/json":{"schema":{"$ref":"#/components/schemas/A"}}}}}}}},
			"components":{"schemas":{"A":{"type":"object","additionalProperties":{"type":"string"},"properties":{"x":{"type":"string"}}}}}}`,
		"inline non-ref schema": `{"openapi":"3.0.0","info":{"version":"1"},"paths":{"/x":{"post":{
			"requestBody":{"content":{"application/json":{"schema":{"type":"object","properties":{"x":{"type":"string"}}}}}},
			"responses":{"200":{"content":{"application/json":{"schema":{"$ref":"#/components/schemas/A"}}}}}}}},
			"components":{"schemas":{"A":{"type":"object","properties":{"x":{"type":"string"}}}}}}`,
		// A property carrying a keyword but no type and no $ref (here format) is
		// not the unconstrained {} that #121 lowers to google.protobuf.Value —
		// it is a malformed leaf and stays a hard error.
		"typeless non-empty property": `{"openapi":"3.0.0","info":{"version":"1"},"paths":{"/x":{"post":{
			"requestBody":{"content":{"application/json":{"schema":{"$ref":"#/components/schemas/A"}}}},
			"responses":{"200":{"content":{"application/json":{"schema":{"$ref":"#/components/schemas/A"}}}}}}}},
			"components":{"schemas":{"A":{"type":"object","properties":{"x":{"format":"uuid"}}}}}}`,
		"missing info.version": `{"openapi":"3.0.0","info":{"title":"x"},"paths":{"/x":{"post":{
			"requestBody":{"content":{"application/json":{"schema":{"$ref":"#/components/schemas/A"}}}},
			"responses":{"200":{"content":{"application/json":{"schema":{"$ref":"#/components/schemas/A"}}}}}}}},
			"components":{"schemas":{"A":{"type":"object","properties":{"x":{"type":"string"}}}}}}`,
		// #121 lowers an open object or untagged union to a struct.proto well-known
		// type when it is a property or array item. A component bound *directly* as
		// a request/response body is a different binding — request_message would
		// have to reference google.protobuf.{Struct,Value} instead of a generated
		// message — and stays rejected by the object-with-properties guard.
		"open-object component bound as body": `{"openapi":"3.0.0","info":{"version":"1"},"paths":{"/x":{"post":{
			"requestBody":{"content":{"application/json":{"schema":{"$ref":"#/components/schemas/Open"}}}},
			"responses":{"200":{"content":{"application/json":{"schema":{"$ref":"#/components/schemas/Open"}}}}}}}},
			"components":{"schemas":{"Open":{"type":"object","additionalProperties":true}}}}`,
		"untagged-union component bound as body": `{"openapi":"3.1.0","info":{"version":"1"},"paths":{"/x":{"post":{
			"requestBody":{"content":{"application/json":{"schema":{"$ref":"#/components/schemas/Poly"}}}},
			"responses":{"200":{"content":{"application/json":{"schema":{"$ref":"#/components/schemas/Poly"}}}}}}}},
			"components":{"schemas":{"Poly":{"anyOf":[{"type":"string"},{"type":"integer"}]}}}}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			in := writeOpenAPI(t, body)
			if err := bundlegen.Add(in, t.TempDir(), false); err == nil {
				t.Fatalf("%s: expected a hard error, got nil", name)
			}
		})
	}
}

// additionalPropsFalseOpenAPI carries additionalProperties: false on its
// component schema — the Pydantic v2 extra='forbid' default. A proto
// message is closed by construction, so the constraint is already the
// target representation and must be ignored. additionalPropsAbsentOpenAPI
// is the control with the key removed; the two must emit byte-identical
// descriptors. (#113)
const additionalPropsFalseOpenAPI = `{
  "openapi": "3.0.0",
  "info": {"title": "acme", "version": "2026-05-17"},
  "paths": {
    "/v3/widget": {
      "post": {
        "operationId": "makeWidget",
        "requestBody": {"content": {"application/json": {"schema": {"$ref": "#/components/schemas/Widget"}}}},
        "responses": {"200": {"content": {"application/json": {"schema": {"$ref": "#/components/schemas/Widget"}}}}}
      }
    }
  },
  "components": {
    "schemas": {
      "Widget": {"type": "object", "additionalProperties": false, "properties": {
        "name": {"type": "string"}
      }}
    }
  }
}`

// nullableAnyOfOpenAPI exercises #112: the OpenAPI 3.1 nullable idiom
// anyOf:[T, {type: null}]. `label` is a nullable scalar and `meta` is a
// nullable $ref to a sibling component. Both must lower to proto3 optional
// (the same lowering as 3.0's `nullable: true`); the nullable $ref must
// still pull its target (Meta) into the descriptor set via the schema walk.
const nullableAnyOfOpenAPI = `{
  "openapi": "3.1.0",
  "info": {"title": "acme", "version": "2026-05-17"},
  "paths": {
    "/v3/node": {
      "post": {
        "operationId": "makeNode",
        "requestBody": {"content": {"application/json": {"schema": {"$ref": "#/components/schemas/Node"}}}},
        "responses": {"200": {"content": {"application/json": {"schema": {"$ref": "#/components/schemas/Node"}}}}}
      }
    }
  },
  "components": {
    "schemas": {
      "Node": {"type": "object", "properties": {
        "id": {"type": "string"},
        "label": {"anyOf": [{"type": "string"}, {"type": "null"}]},
        "meta": {"anyOf": [{"$ref": "#/components/schemas/Meta"}, {"type": "null"}]}
      }},
      "Meta": {"type": "object", "properties": {
        "key": {"type": "string"}
      }}
    }
  }
}`

const additionalPropsAbsentOpenAPI = `{
  "openapi": "3.0.0",
  "info": {"title": "acme", "version": "2026-05-17"},
  "paths": {
    "/v3/widget": {
      "post": {
        "operationId": "makeWidget",
        "requestBody": {"content": {"application/json": {"schema": {"$ref": "#/components/schemas/Widget"}}}},
        "responses": {"200": {"content": {"application/json": {"schema": {"$ref": "#/components/schemas/Widget"}}}}}
      }
    }
  },
  "components": {
    "schemas": {
      "Widget": {"type": "object", "properties": {
        "name": {"type": "string"}
      }}
    }
  }
}`

// mapAdditionalPropsOpenAPI exercises #119: a Pydantic Dict[str, V] surfaces
// as a property whose schema is {type:object, additionalProperties:<V>} with
// no declared properties of its own. `labels` is Dict[str, str] and must
// lower to map<string,string>; `items` is Dict[str, Item] and must lower to
// map<string,Item>, pulling the value $ref's target (Item) into the
// descriptor set via the schema walk.
const mapAdditionalPropsOpenAPI = `{
  "openapi": "3.0.0",
  "info": {"title": "acme", "version": "2026-05-17"},
  "paths": {
    "/v3/bag": {
      "post": {
        "operationId": "makeBag",
        "requestBody": {"content": {"application/json": {"schema": {"$ref": "#/components/schemas/Bag"}}}},
        "responses": {"200": {"content": {"application/json": {"schema": {"$ref": "#/components/schemas/Bag"}}}}}
      }
    }
  },
  "components": {
    "schemas": {
      "Bag": {"type": "object", "properties": {
        "labels": {"type": "object", "additionalProperties": {"type": "string"}},
        "items": {"type": "object", "additionalProperties": {"$ref": "#/components/schemas/Item"}}
      }},
      "Item": {"type": "object", "properties": {
        "sku": {"type": "string"}
      }}
    }
  }
}`

// TestAddAdditionalPropertiesFalseIsANoOp: a schema with
// additionalProperties: false bundles cleanly and emits descriptors
// byte-identical to the same schema with the key absent — the key is fully
// ignored, not merely tolerated. (#113)
func TestAddAdditionalPropertiesFalseIsANoOp(t *testing.T) {
	falseDir := t.TempDir()
	if err := bundlegen.Add(writeOpenAPI(t, additionalPropsFalseOpenAPI), falseDir, false); err != nil {
		t.Fatalf("Add with additionalProperties:false: %v", err)
	}
	b, err := bundle.Load(falseDir)
	if err != nil {
		t.Fatalf("bundle did not load: %v", err)
	}
	m, err := b.Message("wavefront.gen.v2026_05_17.Widget")
	if err != nil {
		t.Fatalf("resolve Widget: %v", err)
	}
	if m.Fields().ByName("name") == nil {
		t.Error("Widget missing declared field name")
	}

	absentDir := t.TempDir()
	if err := bundlegen.Add(writeOpenAPI(t, additionalPropsAbsentOpenAPI), absentDir, false); err != nil {
		t.Fatalf("Add with key absent: %v", err)
	}
	fa, _ := os.ReadFile(filepath.Join(falseDir, "2026-05-17", "descriptors.binpb"))
	fb, _ := os.ReadFile(filepath.Join(absentDir, "2026-05-17", "descriptors.binpb"))
	if string(fa) != string(fb) {
		t.Error("additionalProperties:false produced different descriptors than the key being absent")
	}
}

// TestAddTypedAdditionalPropertiesBecomesMap: a property whose schema is a
// pure typed dict ({type:object, additionalProperties:<scalar|$ref>} with no
// declared properties) lowers to a proto3 map field. The scalar value form
// becomes map<string,string>; the $ref value form becomes map<string,Item>
// and pulls Item into the descriptor set. (#119)
func TestAddTypedAdditionalPropertiesBecomesMap(t *testing.T) {
	in := writeOpenAPI(t, mapAdditionalPropsOpenAPI)
	out := t.TempDir()
	if err := bundlegen.Add(in, out, false); err != nil {
		t.Fatalf("Add: %v", err)
	}
	b, err := bundle.Load(out)
	if err != nil {
		t.Fatalf("generated bundle did not load: %v", err)
	}

	bag, err := b.Message("wavefront.gen.v2026_05_17.Bag")
	if err != nil {
		t.Fatalf("resolve Bag: %v", err)
	}

	labels := bag.Fields().ByName("labels")
	if labels == nil || !labels.IsMap() {
		t.Fatal("Bag.labels (Dict[str,str]) should be a map field")
	}
	if k := labels.MapKey().Kind().String(); k != "string" {
		t.Errorf("Bag.labels map key kind = %q, want string", k)
	}
	if v := labels.MapValue().Kind().String(); v != "string" {
		t.Errorf("Bag.labels map value kind = %q, want string", v)
	}

	items := bag.Fields().ByName("items")
	if items == nil || !items.IsMap() {
		t.Fatal("Bag.items (Dict[str,Item]) should be a map field")
	}
	if k := items.MapKey().Kind().String(); k != "string" {
		t.Errorf("Bag.items map key kind = %q, want string", k)
	}
	if v := items.MapValue().Kind().String(); v != "message" {
		t.Fatalf("Bag.items map value kind = %q, want message", v)
	}
	if mv := items.MapValue().Message(); mv == nil || !strings.HasSuffix(string(mv.FullName()), ".Item") {
		t.Error("Bag.items map value should reference the Item message")
	}

	// The value $ref's target was walked into the descriptor set.
	if _, err := b.Message("wavefront.gen.v2026_05_17.Item"); err != nil {
		t.Errorf("map value $ref target Item not collected: %v", err)
	}
}

// TestAddNullableAnyOfLowersToProto3Optional: a 3.1 anyOf:[T,{type:null}]
// property lowers to a proto3 optional field, for both a scalar leaf and a
// $ref. The $ref target is collected into the descriptor set. (#112)
func TestAddNullableAnyOfLowersToProto3Optional(t *testing.T) {
	in := writeOpenAPI(t, nullableAnyOfOpenAPI)
	out := t.TempDir()
	if err := bundlegen.Add(in, out, false); err != nil {
		t.Fatalf("Add: %v", err)
	}
	b, err := bundle.Load(out)
	if err != nil {
		t.Fatalf("generated bundle did not load: %v", err)
	}

	node, err := b.Message("wavefront.gen.v2026_05_17.Node")
	if err != nil {
		t.Fatalf("resolve Node: %v", err)
	}

	// A plain proto3 scalar has no presence; the nullable scalar does.
	if id := node.Fields().ByName("id"); id == nil || id.HasPresence() {
		t.Error("Node.id should be a plain scalar with no presence")
	}
	if label := node.Fields().ByName("label"); label == nil || !label.HasPresence() {
		t.Error("Node.label (nullable anyOf scalar) should be a proto3 optional with presence")
	}

	// The nullable $ref is a message field referencing Meta, with presence.
	meta := node.Fields().ByName("meta")
	if meta == nil || meta.Message() == nil || !strings.HasSuffix(string(meta.Message().FullName()), ".Meta") {
		t.Fatal("Node.meta (nullable anyOf $ref) should be a message field referencing Meta")
	}
	if !meta.HasPresence() {
		t.Error("Node.meta (nullable anyOf $ref) should have presence")
	}

	// The nullable $ref's target was walked into the descriptor set.
	if _, err := b.Message("wavefront.gen.v2026_05_17.Meta"); err != nil {
		t.Errorf("nullable $ref target Meta not collected: %v", err)
	}
}

// untaggedUnionOpenAPI exercises #121's untagged-union and unconstrained-schema
// lowering. None of these shapes carries a discriminator, so each is a bare JSON
// value on the wire that proto3 oneof (a tagged union) cannot decode; the
// correct target is google.protobuf.Value:
//   - loc: an array whose items are an untagged anyOf of scalars (the FastAPI
//     ValidationError.loc shape) → repeated google.protobuf.Value;
//   - code: a property-level untagged oneOf → google.protobuf.Value;
//   - either: a property-level untagged anyOf of $refs → google.protobuf.Value
//     (the member messages A/B are absorbed into Value, not collected);
//   - extra: an unconstrained {} → google.protobuf.Value.
const untaggedUnionOpenAPI = `{
  "openapi": "3.1.0",
  "info": {"title": "acme", "version": "2026-05-17"},
  "paths": {
    "/v3/poly": {
      "post": {
        "operationId": "poly",
        "requestBody": {"content": {"application/json": {"schema": {"$ref": "#/components/schemas/Poly"}}}},
        "responses": {"200": {"content": {"application/json": {"schema": {"$ref": "#/components/schemas/Poly"}}}}}
      }
    }
  },
  "components": {
    "schemas": {
      "Poly": {"type": "object", "properties": {
        "loc": {"type": "array", "items": {"anyOf": [{"type": "string"}, {"type": "integer"}]}},
        "code": {"oneOf": [{"type": "string"}, {"type": "integer"}]},
        "either": {"anyOf": [{"$ref": "#/components/schemas/A"}, {"$ref": "#/components/schemas/B"}]},
        "extra": {}
      }},
      "A": {"type": "object", "properties": {"a": {"type": "string"}}},
      "B": {"type": "object", "properties": {"b": {"type": "string"}}}
    }
  }
}`

// TestAddUntaggedUnionAndUnconstrainedLowerToValue: untagged anyOf/oneOf (no
// discriminator) and the unconstrained {} schema lower to google.protobuf.Value,
// singular at the property level and repeated as array items. The bundle loads,
// which proves the synthetic google/protobuf/struct.proto dependency resolves. (#121)
func TestAddUntaggedUnionAndUnconstrainedLowerToValue(t *testing.T) {
	in := writeOpenAPI(t, untaggedUnionOpenAPI)
	out := t.TempDir()
	if err := bundlegen.Add(in, out, false); err != nil {
		t.Fatalf("Add: %v", err)
	}
	b, err := bundle.Load(out)
	if err != nil {
		t.Fatalf("generated bundle did not load: %v", err)
	}
	poly, err := b.Message("wavefront.gen.v2026_05_17.Poly")
	if err != nil {
		t.Fatalf("resolve Poly: %v", err)
	}

	loc := poly.Fields().ByName("loc")
	if loc == nil || !loc.IsList() {
		t.Fatal("Poly.loc (array of untagged anyOf) should be a repeated field")
	}
	if loc.Message() == nil || string(loc.Message().FullName()) != "google.protobuf.Value" {
		t.Errorf("Poly.loc element type = %v, want google.protobuf.Value", fullName(loc))
	}

	for _, name := range []string{"code", "either", "extra"} {
		f := poly.Fields().ByName(protoName(name))
		if f == nil || f.IsList() {
			t.Errorf("Poly.%s should be a singular message field", name)
			continue
		}
		if f.Message() == nil || string(f.Message().FullName()) != "google.protobuf.Value" {
			t.Errorf("Poly.%s type = %v, want google.protobuf.Value", name, fullName(f))
		}
	}
}

// openObjectOpenAPI exercises #121's open-object lowering: an object property
// with additionalProperties:true or additionalProperties:{} and no declared
// properties of its own is dict[str, Any] and lowers to google.protobuf.Struct.
const openObjectOpenAPI = `{
  "openapi": "3.0.0",
  "info": {"title": "acme", "version": "2026-05-17"},
  "paths": {
    "/v3/open": {
      "post": {
        "operationId": "open",
        "requestBody": {"content": {"application/json": {"schema": {"$ref": "#/components/schemas/Open"}}}},
        "responses": {"200": {"content": {"application/json": {"schema": {"$ref": "#/components/schemas/Open"}}}}}
      }
    }
  },
  "components": {
    "schemas": {
      "Open": {"type": "object", "properties": {
        "meta": {"type": "object", "additionalProperties": true},
        "bag": {"type": "object", "additionalProperties": {}}
      }}
    }
  }
}`

// TestAddOpenObjectLowersToStruct: an open object (additionalProperties:true or
// {}, no declared properties) lowers to google.protobuf.Struct rather than a
// proto3 map; a Struct decodes a bare JSON object directly. (#121)
func TestAddOpenObjectLowersToStruct(t *testing.T) {
	in := writeOpenAPI(t, openObjectOpenAPI)
	out := t.TempDir()
	if err := bundlegen.Add(in, out, false); err != nil {
		t.Fatalf("Add: %v", err)
	}
	b, err := bundle.Load(out)
	if err != nil {
		t.Fatalf("generated bundle did not load: %v", err)
	}
	open, err := b.Message("wavefront.gen.v2026_05_17.Open")
	if err != nil {
		t.Fatalf("resolve Open: %v", err)
	}
	for _, name := range []string{"meta", "bag"} {
		f := open.Fields().ByName(protoName(name))
		if f == nil || f.IsMap() || f.IsList() {
			t.Errorf("Open.%s (open object) should be a singular message field, not a map/list", name)
			continue
		}
		if f.Message() == nil || string(f.Message().FullName()) != "google.protobuf.Struct" {
			t.Errorf("Open.%s type = %v, want google.protobuf.Struct", name, fullName(f))
		}
	}
}

// discriminatedUnionOpenAPI exercises #129's discriminated-union lowering. A
// tagged anyOf/oneOf (one carrying a sibling discriminator) is always a JSON
// object, but its wire shape is flattened — the discriminator is a sibling
// property and the member's fields sit at the top level. A proto3 oneof encodes
// the other way (wrapped under the member key, {"cat":{...}}), so protojson
// cannot bridge the two; the faithful lowering is google.protobuf.Struct, which
// relays the object verbatim. pet is a property-level discriminated anyOf; pets
// is an array whose items are a discriminated oneOf.
const discriminatedUnionOpenAPI = `{
  "openapi": "3.1.0",
  "info": {"title": "acme", "version": "2026-05-17"},
  "paths": {
    "/v3/pet": {
      "post": {
        "operationId": "pet",
        "requestBody": {"content": {"application/json": {"schema": {"$ref": "#/components/schemas/PetEnvelope"}}}},
        "responses": {"200": {"content": {"application/json": {"schema": {"$ref": "#/components/schemas/PetEnvelope"}}}}}
      }
    }
  },
  "components": {
    "schemas": {
      "PetEnvelope": {"type": "object", "properties": {
        "pet": {
          "anyOf": [{"$ref": "#/components/schemas/Cat"}, {"$ref": "#/components/schemas/Dog"}],
          "discriminator": {"propertyName": "petType", "mapping": {"cat": "#/components/schemas/Cat", "dog": "#/components/schemas/Dog"}}
        },
        "pets": {"type": "array", "items": {
          "oneOf": [{"$ref": "#/components/schemas/Cat"}, {"$ref": "#/components/schemas/Dog"}],
          "discriminator": {"propertyName": "petType", "mapping": {"cat": "#/components/schemas/Cat", "dog": "#/components/schemas/Dog"}}
        }}
      }},
      "Cat": {"type": "object", "properties": {"petType": {"type": "string"}, "meow": {"type": "string"}}},
      "Dog": {"type": "object", "properties": {"petType": {"type": "string"}, "woof": {"type": "string"}}}
    }
  }
}`

// TestAddDiscriminatedUnionLowersToStruct: a discriminated anyOf/oneOf lowers to
// google.protobuf.Struct (singular at the property level, repeated as array
// items) — the same WKT-fallback open objects get, because proto3 oneof's wire
// shape is incompatible with OpenAPI's flattened discriminator. The union
// members are absorbed into Struct, not collected as messages. (#129)
func TestAddDiscriminatedUnionLowersToStruct(t *testing.T) {
	in := writeOpenAPI(t, discriminatedUnionOpenAPI)
	out := t.TempDir()
	if err := bundlegen.Add(in, out, false); err != nil {
		t.Fatalf("Add: %v", err)
	}
	b, err := bundle.Load(out)
	if err != nil {
		t.Fatalf("generated bundle did not load: %v", err)
	}
	env, err := b.Message("wavefront.gen.v2026_05_17.PetEnvelope")
	if err != nil {
		t.Fatalf("resolve PetEnvelope: %v", err)
	}

	pet := env.Fields().ByName("pet")
	if pet == nil || pet.IsList() {
		t.Fatal("PetEnvelope.pet (discriminated anyOf) should be a singular message field")
	}
	if pet.Message() == nil || string(pet.Message().FullName()) != "google.protobuf.Struct" {
		t.Errorf("PetEnvelope.pet type = %v, want google.protobuf.Struct", fullName(pet))
	}

	pets := env.Fields().ByName("pets")
	if pets == nil || !pets.IsList() {
		t.Fatal("PetEnvelope.pets (array of discriminated oneOf) should be a repeated field")
	}
	if pets.Message() == nil || string(pets.Message().FullName()) != "google.protobuf.Struct" {
		t.Errorf("PetEnvelope.pets element type = %v, want google.protobuf.Struct", fullName(pets))
	}

	// The union members are absorbed into Struct, so they are not collected.
	for _, name := range []string{"Cat", "Dog"} {
		if _, err := b.Message("wavefront.gen.v2026_05_17." + name); err == nil {
			t.Errorf("union member %s should not be collected (absorbed into Struct)", name)
		}
	}
}

// TestGeneratedDiscriminatedUnionDecodesFlattenedUpstream is the end-to-end proof
// for #129: a generated bundle whose discriminated-union fields are Struct
// actually decodes a realistic flattened upstream body — discriminator as a
// sibling property, member fields at the top level — through the same dynamicpb +
// protojson machinery the adapter uses. A proto3 oneof would hard-error on this
// shape (unknown field "petType"); Struct relays it verbatim. (#129)
func TestGeneratedDiscriminatedUnionDecodesFlattenedUpstream(t *testing.T) {
	in := writeOpenAPI(t, discriminatedUnionOpenAPI)
	out := t.TempDir()
	if err := bundlegen.Add(in, out, false); err != nil {
		t.Fatalf("Add: %v", err)
	}
	b, err := bundle.Load(out)
	if err != nil {
		t.Fatalf("generated bundle did not load: %v", err)
	}
	md, err := b.Message("wavefront.gen.v2026_05_17.PetEnvelope")
	if err != nil {
		t.Fatalf("resolve PetEnvelope: %v", err)
	}

	const body = `{"pet":{"petType":"cat","meow":"purr"},"pets":[{"petType":"dog","woof":"bark"}]}`
	msg := dynamicpb.NewMessage(md)
	if err := protojson.Unmarshal([]byte(body), msg); err != nil {
		t.Fatalf("decode flattened discriminated-union body: %v", err)
	}

	// Re-encode and confirm both members survived the round-trip verbatim.
	reEncoded, err := protojson.Marshal(msg)
	if err != nil {
		t.Fatalf("re-encode: %v", err)
	}
	for _, want := range []string{`"petType"`, `"cat"`, `"meow"`, `"purr"`, `"dog"`, `"woof"`, `"bark"`} {
		if !strings.Contains(string(reEncoded), want) {
			t.Errorf("re-encoded body %s missing %q", reEncoded, want)
		}
	}
}

// validationErrorOpenAPI is the FastAPI auto-emitted 422 surface: an operation
// whose 422 binds HTTPValidationError, whose detail is a list of ValidationError
// whose loc is an untagged anyOf of string|integer. This is the reference
// consumer's blocker — the shape #121 exists to type.
const validationErrorOpenAPI = `{
  "openapi": "3.1.0",
  "info": {"title": "acme", "version": "2026-05-17"},
  "paths": {
    "/v3/items": {
      "post": {
        "operationId": "createItem",
        "requestBody": {"content": {"application/json": {"schema": {"$ref": "#/components/schemas/Item"}}}},
        "responses": {
          "200": {"content": {"application/json": {"schema": {"$ref": "#/components/schemas/Item"}}}},
          "422": {"content": {"application/json": {"schema": {"$ref": "#/components/schemas/HTTPValidationError"}}}}
        }
      }
    }
  },
  "components": {
    "schemas": {
      "Item": {"type": "object", "properties": {"name": {"type": "string"}}},
      "HTTPValidationError": {"type": "object", "properties": {
        "detail": {"type": "array", "items": {"$ref": "#/components/schemas/ValidationError"}}
      }},
      "ValidationError": {"type": "object", "properties": {
        "loc": {"type": "array", "items": {"anyOf": [{"type": "string"}, {"type": "integer"}]}},
        "msg": {"type": "string"},
        "type": {"type": "string"}
      }}
    }
  }
}`

// TestGeneratedValidationErrorBundleDecodesRealistic422 is the end-to-end proof
// for #121: a generated bundle whose loc field is repeated google.protobuf.Value
// actually decodes a realistic FastAPI 422 body through the same dynamicpb +
// protojson machinery the adapter uses. Before #121 the untagged union lowered
// to a proto3 oneof and this Unmarshal failed (the transform_failed/502 the
// consumer hit). The mixed string/integer loc array is the crux. (#121)
func TestGeneratedValidationErrorBundleDecodesRealistic422(t *testing.T) {
	in := writeOpenAPI(t, validationErrorOpenAPI)
	out := t.TempDir()
	if err := bundlegen.Add(in, out, false); err != nil {
		t.Fatalf("Add: %v", err)
	}
	b, err := bundle.Load(out)
	if err != nil {
		t.Fatalf("generated bundle did not load: %v", err)
	}
	md, err := b.Message("wavefront.gen.v2026_05_17.HTTPValidationError")
	if err != nil {
		t.Fatalf("resolve HTTPValidationError: %v", err)
	}

	const body = `{"detail":[{"loc":["body","items",0],"msg":"field required","type":"missing"}]}`
	msg := dynamicpb.NewMessage(md)
	if err := protojson.Unmarshal([]byte(body), msg); err != nil {
		t.Fatalf("decode realistic 422 body: %v", err)
	}

	detailFD := md.Fields().ByName("detail")
	detail := msg.Get(detailFD).List()
	if detail.Len() != 1 {
		t.Fatalf("detail length = %d, want 1", detail.Len())
	}
	ve := detail.Get(0).Message()
	locFD := detailFD.Message().Fields().ByName("loc")
	loc := ve.Get(locFD).List()
	if loc.Len() != 3 {
		t.Fatalf("loc length = %d, want 3 (mixed string/integer)", loc.Len())
	}

	// Re-encode and confirm the mixed scalars survived the round-trip.
	reEncoded, err := protojson.Marshal(msg)
	if err != nil {
		t.Fatalf("re-encode: %v", err)
	}
	for _, want := range []string{`"body"`, `"items"`, `0`} {
		if !strings.Contains(string(reEncoded), want) {
			t.Errorf("re-encoded body %s missing %q", reEncoded, want)
		}
	}
}

// emptyObjectOpenAPI carries a component that is an explicitly-empty object —
// {"type":"object","properties":{}} — the shape FastAPI emits for a model with
// no fields (a bare acknowledgement). It must bundle as a zero-field proto
// message rather than being rejected. (#118)
const emptyObjectOpenAPI = `{
  "openapi": "3.0.0",
  "info": {"title": "acme", "version": "2026-05-17"},
  "paths": {
    "/v3/ack": {
      "post": {
        "operationId": "doAck",
        "requestBody": {"content": {"application/json": {"schema": {"$ref": "#/components/schemas/AckRequest"}}}},
        "responses": {"200": {"content": {"application/json": {"schema": {"$ref": "#/components/schemas/Ack"}}}}}
      }
    }
  },
  "components": {
    "schemas": {
      "AckRequest": {"type": "object", "properties": {"id": {"type": "string"}}},
      "Ack": {"type": "object", "properties": {}}
    }
  }
}`

// TestAddAcceptsEmptyObjectSchema: an explicitly-empty object schema
// ({"type":"object","properties":{}}) bundles as a zero-field proto message; a
// bare {"type":"object"} with no properties key stays a hard error — the
// open/untyped-object boundary. (#118)
func TestAddAcceptsEmptyObjectSchema(t *testing.T) {
	in := writeOpenAPI(t, emptyObjectOpenAPI)
	out := t.TempDir()
	if err := bundlegen.Add(in, out, false); err != nil {
		t.Fatalf("Add explicit-empty object: %v", err)
	}
	b, err := bundle.Load(out)
	if err != nil {
		t.Fatalf("generated bundle did not load: %v", err)
	}
	ack, err := b.Message("wavefront.gen.v2026_05_17.Ack")
	if err != nil {
		t.Fatalf("resolve Ack: %v", err)
	}
	if n := ack.Fields().Len(); n != 0 {
		t.Errorf("Ack should be a zero-field message, has %d field(s)", n)
	}

	// A bare {"type":"object"} with no properties key is an open/untyped object
	// and stays rejected, so the acceptance above is a deliberate boundary.
	const noPropsKeyOpenAPI = `{
  "openapi": "3.0.0",
  "info": {"title": "acme", "version": "2026-05-17"},
  "paths": {
    "/v3/ack": {
      "post": {
        "requestBody": {"content": {"application/json": {"schema": {"$ref": "#/components/schemas/Open"}}}},
        "responses": {"200": {"content": {"application/json": {"schema": {"$ref": "#/components/schemas/Open"}}}}}
      }
    }
  },
  "components": {"schemas": {"Open": {"type": "object"}}}
}`
	if err := bundlegen.Add(writeOpenAPI(t, noPropsKeyOpenAPI), t.TempDir(), false); err == nil {
		t.Error("a bare {type:object} with no properties key should still be a hard error")
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
	if err := bundlegen.Add(in, bundleDir, false); err != nil {
		t.Fatalf("first Add: %v", err)
	}
	if err := bundlegen.Add(in, bundleDir, false); err == nil {
		t.Fatal("second Add of the same version: expected a hard error, got nil")
	}
}

// twoRouteOpenAPI is a one-route variant of the multi-route fixture; the
// force-overwrite tests need a second OpenAPI shape that resolves to the
// same info.version so a re-add with --force visibly replaces the layer
// contents.
const sampleOpenAPIExtraRoute = `{
  "openapi": "3.0.0",
  "info": {"title": "acme", "version": "2026-05-17"},
  "paths": {
    "/v3/echo": {
      "post": {
        "operationId": "echo",
        "requestBody": {"content": {"application/json": {"schema": {"$ref": "#/components/schemas/EchoRequest"}}}},
        "responses": {"200": {"content": {"application/json": {"schema": {"$ref": "#/components/schemas/EchoReply"}}}}}
      }
    },
    "/v3/ping": {
      "post": {
        "operationId": "ping",
        "requestBody": {"content": {"application/json": {"schema": {"$ref": "#/components/schemas/EchoRequest"}}}},
        "responses": {"200": {"content": {"application/json": {"schema": {"$ref": "#/components/schemas/EchoReply"}}}}}
      }
    }
  },
  "components": {
    "schemas": {
      "EchoRequest": {"type": "object", "properties": {
        "text": {"type": "string"}
      }},
      "EchoReply": {"type": "object", "properties": {
        "text": {"type": "string"}
      }}
    }
  }
}`

// TestAddForceOverwritesExistingVersion is the inverse of
// TestAddRefusesToOverwriteAnExistingVersion: with force=true, a second
// Add against the same info.version replaces the existing layer dir,
// and the new content is what's on disk.
func TestAddForceOverwritesExistingVersion(t *testing.T) {
	in1 := writeOpenAPI(t, sampleOpenAPI)
	in2 := writeOpenAPI(t, sampleOpenAPIExtraRoute)
	bundleDir := t.TempDir()
	if err := bundlegen.Add(in1, bundleDir, false); err != nil {
		t.Fatalf("first Add: %v", err)
	}
	// Pre-condition: the single-route version is on disk.
	versionsPath := filepath.Join(bundleDir, "2026-05-17", "versions.yaml")
	pre, err := os.ReadFile(versionsPath)
	if err != nil {
		t.Fatalf("read versions.yaml after first add: %v", err)
	}
	if strings.Contains(string(pre), "/v3/ping") {
		t.Fatalf("pre-condition violated: first add already contains /v3/ping:\n%s", pre)
	}

	// Force-overwrite with the two-route OpenAPI.
	if err := bundlegen.Add(in2, bundleDir, true); err != nil {
		t.Fatalf("second Add with force=true: %v", err)
	}
	post, err := os.ReadFile(versionsPath)
	if err != nil {
		t.Fatalf("read versions.yaml after force re-add: %v", err)
	}
	if !strings.Contains(string(post), "/v3/ping") {
		t.Errorf("force re-add did not replace the layer; /v3/ping missing from versions.yaml:\n%s", post)
	}
	if !strings.Contains(string(post), "/v3/echo") {
		t.Errorf("force re-add lost the original route /v3/echo:\n%s", post)
	}

	// The bundle must still load cleanly after a force overwrite.
	if _, err := bundle.Load(bundleDir); err != nil {
		t.Errorf("bundle did not load after force re-add: %v", err)
	}
}

// TestAddForceIsANoOpWhenLayerAbsent confirms force=true on a fresh
// bundle (no existing layer) behaves like a regular add: the force is
// a no-op when there's nothing to overwrite.
func TestAddForceIsANoOpWhenLayerAbsent(t *testing.T) {
	in := writeOpenAPI(t, sampleOpenAPI)
	bundleDir := t.TempDir()
	if err := bundlegen.Add(in, bundleDir, true); err != nil {
		t.Fatalf("Add with force=true on a fresh bundle: %v", err)
	}
	if _, err := os.Stat(filepath.Join(bundleDir, "2026-05-17", "versions.yaml")); err != nil {
		t.Fatalf("force-add on a fresh bundle did not produce a layer: %v", err)
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

// bareObjectOpenAPI carries a bare {"type":"object"} — type present, no
// properties, no additionalProperties — as a singular property (ctx) and an
// array item (extras). It is semantically identical to additionalProperties:
// true ("an object with arbitrary fields") and is the exact shape FastAPI emits
// for ValidationError.ctx. v0.7.0 rejected it; it must lower to
// google.protobuf.Struct like any open object. (#134)
const bareObjectOpenAPI = `{
  "openapi": "3.1.0",
  "info": {"title": "t", "version": "0"},
  "paths": {"/x": {"get": {"operationId": "x", "responses": {"200": {"description": "ok",
    "content": {"application/json": {"schema": {"$ref": "#/components/schemas/Out"}}}}}}}},
  "components": {"schemas": {
    "Out": {"type": "object", "properties": {
      "ctx": {"type": "object"},
      "extras": {"type": "array", "items": {"type": "object"}}
    }}
  }}
}`

// TestAddBareObjectLowersToStruct: a bare {"type":"object"} lowers to
// google.protobuf.Struct — singular as a property, repeated as an array item —
// exactly like additionalProperties:true. (#134)
func TestAddBareObjectLowersToStruct(t *testing.T) {
	in := writeOpenAPI(t, bareObjectOpenAPI)
	out := t.TempDir()
	if err := bundlegen.Add(in, out, false); err != nil {
		t.Fatalf("Add: %v", err)
	}
	b, err := bundle.Load(out)
	if err != nil {
		t.Fatalf("generated bundle did not load: %v", err)
	}
	outMsg, err := b.Message("wavefront.gen.v0.Out")
	if err != nil {
		t.Fatalf("resolve Out: %v", err)
	}

	ctx := outMsg.Fields().ByName(protoName("ctx"))
	if ctx == nil || ctx.IsList() || ctx.IsMap() {
		t.Fatal(`Out.ctx (bare {"type":"object"}) should be a singular message field`)
	}
	if ctx.Message() == nil || string(ctx.Message().FullName()) != "google.protobuf.Struct" {
		t.Errorf("Out.ctx type = %v, want google.protobuf.Struct", fullName(ctx))
	}

	extras := outMsg.Fields().ByName(protoName("extras"))
	if extras == nil || !extras.IsList() {
		t.Fatal("Out.extras (array of bare objects) should be a repeated field")
	}
	if extras.Message() == nil || string(extras.Message().FullName()) != "google.protobuf.Struct" {
		t.Errorf("Out.extras element type = %v, want google.protobuf.Struct", fullName(extras))
	}

	// A Struct decodes an arbitrary object verbatim, like additionalProperties:true.
	const body = `{"ctx":{"limit":5,"why":"too big"},"extras":[{"k":"v"}]}`
	msg := dynamicpb.NewMessage(outMsg)
	if err := protojson.Unmarshal([]byte(body), msg); err != nil {
		t.Fatalf("decode bare-object body: %v", err)
	}
}
