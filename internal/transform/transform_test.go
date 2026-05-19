package transform

import (
	"bytes"
	"testing"
)

func mustDV(t *testing.T, v any) DefaultValue {
	t.Helper()
	d, err := NewDefaultValue(v)
	if err != nil {
		t.Fatalf("NewDefaultValue(%#v): %v", v, err)
	}
	return d
}

func TestNewDefaultValueRejectsNonScalar(t *testing.T) {
	for _, v := range []any{map[string]any{"a": 1}, []any{1, 2}} {
		if _, err := NewDefaultValue(v); err == nil {
			t.Errorf("NewDefaultValue(%#v) should reject non-scalar", v)
		}
	}
	for _, v := range []any{"s", 7, int64(1 << 60), 3.5, true, nil} {
		if _, err := NewDefaultValue(v); err != nil {
			t.Errorf("NewDefaultValue(%#v) should accept scalar: %v", v, err)
		}
	}
}

func TestNoOpsIsByteIdenticalPassthrough(t *testing.T) {
	in := []byte(`{"a":1}` + "\n  trailing")
	out, e := ApplyRequest(nil, in)
	if e != nil {
		t.Fatalf("unexpected error: %v", e)
	}
	if !bytes.Equal(out, in) {
		t.Errorf("no-ops mutated body: got %q want %q", out, in)
	}
}

func TestRenameMovesKey(t *testing.T) {
	out, e := ApplyRequest([]Op{{Kind: "rename", From: "displayName", To: "display_name"}},
		[]byte(`{"displayName":"Ada"}`))
	if e != nil {
		t.Fatalf("err: %v", e)
	}
	if string(out) != `{"display_name":"Ada"}` {
		t.Errorf("got %s", out)
	}
}

func TestRenameAbsentSourceFailsRequest422(t *testing.T) {
	_, e := ApplyRequest([]Op{{Kind: "rename", From: "x", To: "y"}}, []byte(`{}`))
	if e == nil || e.HTTPStatus() != 422 || e.Code() != "transform_failed" {
		t.Fatalf("want 422 transform_failed, got %v", e)
	}
}

func TestRenameAbsentSourceFailsResponse502(t *testing.T) {
	_, e := ApplyResponse([]Op{{Kind: "rename", From: "x", To: "y"}}, []byte(`{}`))
	if e == nil || e.HTTPStatus() != 502 {
		t.Fatalf("want 502, got %v", e)
	}
}

func TestRenameCollisionFails(t *testing.T) {
	_, e := ApplyRequest([]Op{{Kind: "rename", From: "a", To: "b"}}, []byte(`{"a":1,"b":2}`))
	if e == nil {
		t.Fatal("want collision error")
	}
}

func TestOptionalizeBeforeRenameSkips(t *testing.T) {
	out, e := ApplyRequest([]Op{
		{Kind: "optionalize", Field: "displayName"},
		{Kind: "rename", From: "displayName", To: "display_name"},
	}, []byte(`{"keep":1}`))
	if e != nil {
		t.Fatalf("err: %v", e)
	}
	if string(out) != `{"keep":1}` {
		t.Errorf("got %s", out)
	}
}

func TestDefaultFillsWhenAbsentOrNull(t *testing.T) {
	out, e := ApplyRequest([]Op{{Kind: KindDefault, Field: "locale", Value: mustDV(t, "en-US")}},
		[]byte(`{"x":1}`))
	if e != nil || string(out) != `{"locale":"en-US","x":1}` {
		t.Fatalf("got %s err %v", out, e)
	}
	out, _ = ApplyRequest([]Op{{Kind: KindDefault, Field: "locale", Value: mustDV(t, "en-US")}},
		[]byte(`{"locale":"fr"}`))
	if string(out) != `{"locale":"fr"}` {
		t.Errorf("must not overwrite present value: %s", out)
	}
}

func TestCoerce(t *testing.T) {
	out, e := ApplyRequest([]Op{{Kind: "coerce", Field: "id", CoerceTo: "string"}},
		[]byte(`{"id":4291}`))
	if e != nil || string(out) != `{"id":"4291"}` {
		t.Fatalf("number->string: got %s err %v", out, e)
	}
	out, e = ApplyRequest([]Op{{Kind: "coerce", Field: "n", CoerceTo: "number"}},
		[]byte(`{"n":"12"}`))
	if e != nil || string(out) != `{"n":12}` {
		t.Fatalf("string->number: got %s err %v", out, e)
	}
	_, e = ApplyRequest([]Op{{Kind: "coerce", Field: "n", CoerceTo: "number"}},
		[]byte(`{"n":"abc"}`))
	if e == nil {
		t.Error("non-numeric string -> number must fail")
	}
	_, e = ApplyRequest([]Op{{Kind: "coerce", Field: "o", CoerceTo: "string"}},
		[]byte(`{"o":{"k":1}}`))
	if e == nil {
		t.Error("non-scalar coerce must fail")
	}
	out, e = ApplyRequest([]Op{{Kind: "coerce", Field: "id", CoerceTo: "string"}},
		[]byte(`{"id":1234567}`))
	if e != nil || string(out) != `{"id":"1234567"}` {
		t.Fatalf("large integer->string: got %s err %v", out, e)
	}
}

func TestApplyResponseSuccess(t *testing.T) {
	out, e := ApplyResponse([]Op{{Kind: "rename", From: "internalId", To: "id"}},
		[]byte(`{"internalId":1}`))
	if e != nil {
		t.Fatalf("unexpected error: %v", e)
	}
	if string(out) != `{"id":1}` {
		t.Errorf("got %s", out)
	}
}

func TestOptionalizeBeforeCoerceSkips(t *testing.T) {
	out, e := ApplyRequest([]Op{
		{Kind: "optionalize", Field: "n"},
		{Kind: "coerce", Field: "n", CoerceTo: "string"},
	}, []byte(`{"keep":1}`))
	if e != nil {
		t.Fatalf("err: %v", e)
	}
	if string(out) != `{"keep":1}` {
		t.Errorf("got %s", out)
	}
}

func TestNonObjectBodyWithOpsFails(t *testing.T) {
	_, e := ApplyRequest([]Op{{Kind: KindDefault, Field: "a", Value: mustDV(t, 1)}}, []byte(`[1,2]`))
	if e == nil || e.HTTPStatus() != 422 {
		t.Fatalf("array body with ops must 422, got %v", e)
	}
}
