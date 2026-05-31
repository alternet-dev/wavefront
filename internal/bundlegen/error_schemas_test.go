package bundlegen

import "testing"

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
