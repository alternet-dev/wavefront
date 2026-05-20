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

func mustPath(t *testing.T, s string) Path {
	t.Helper()
	p, err := ParsePath(s)
	if err != nil {
		t.Fatalf("ParsePath(%q): %v", s, err)
	}
	return p
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
	out, e := ApplyRequest([]Op{{Kind: "rename", From: mustPath(t, "displayName"), To: mustPath(t, "display_name")}},
		[]byte(`{"displayName":"Ada"}`))
	if e != nil {
		t.Fatalf("err: %v", e)
	}
	if string(out) != `{"display_name":"Ada"}` {
		t.Errorf("got %s", out)
	}
}

func TestRenameAbsentSourceFailsRequest422(t *testing.T) {
	_, e := ApplyRequest([]Op{{Kind: "rename", From: mustPath(t, "x"), To: mustPath(t, "y")}}, []byte(`{}`))
	if e == nil || e.HTTPStatus() != 422 || e.Code() != "transform_failed" {
		t.Fatalf("want 422 transform_failed, got %v", e)
	}
}

func TestRenameAbsentSourceFailsResponse502(t *testing.T) {
	_, e := ApplyResponse([]Op{{Kind: "rename", From: mustPath(t, "x"), To: mustPath(t, "y")}}, []byte(`{}`))
	if e == nil || e.HTTPStatus() != 502 {
		t.Fatalf("want 502, got %v", e)
	}
}

func TestRenameCollisionFails(t *testing.T) {
	_, e := ApplyRequest([]Op{{Kind: "rename", From: mustPath(t, "a"), To: mustPath(t, "b")}}, []byte(`{"a":1,"b":2}`))
	if e == nil {
		t.Fatal("want collision error")
	}
}

func TestOptionalizeBeforeRenameSkips(t *testing.T) {
	out, e := ApplyRequest([]Op{
		{Kind: "optionalize", Field: mustPath(t, "displayName")},
		{Kind: "rename", From: mustPath(t, "displayName"), To: mustPath(t, "display_name")},
	}, []byte(`{"keep":1}`))
	if e != nil {
		t.Fatalf("err: %v", e)
	}
	if string(out) != `{"keep":1}` {
		t.Errorf("got %s", out)
	}
}

func TestDefaultFillsWhenAbsentOrNull(t *testing.T) {
	out, e := ApplyRequest([]Op{{Kind: KindDefault, Field: mustPath(t, "locale"), Value: mustDV(t, "en-US")}},
		[]byte(`{"x":1}`))
	if e != nil || string(out) != `{"locale":"en-US","x":1}` {
		t.Fatalf("got %s err %v", out, e)
	}
	out, _ = ApplyRequest([]Op{{Kind: KindDefault, Field: mustPath(t, "locale"), Value: mustDV(t, "en-US")}},
		[]byte(`{"locale":"fr"}`))
	if string(out) != `{"locale":"fr"}` {
		t.Errorf("must not overwrite present value: %s", out)
	}
}

func TestCoerce(t *testing.T) {
	out, e := ApplyRequest([]Op{{Kind: "coerce", Field: mustPath(t, "id"), CoerceTo: "string"}},
		[]byte(`{"id":4291}`))
	if e != nil || string(out) != `{"id":"4291"}` {
		t.Fatalf("number->string: got %s err %v", out, e)
	}
	out, e = ApplyRequest([]Op{{Kind: "coerce", Field: mustPath(t, "n"), CoerceTo: "number"}},
		[]byte(`{"n":"12"}`))
	if e != nil || string(out) != `{"n":12}` {
		t.Fatalf("string->number: got %s err %v", out, e)
	}
	_, e = ApplyRequest([]Op{{Kind: "coerce", Field: mustPath(t, "n"), CoerceTo: "number"}},
		[]byte(`{"n":"abc"}`))
	if e == nil {
		t.Error("non-numeric string -> number must fail")
	}
	_, e = ApplyRequest([]Op{{Kind: "coerce", Field: mustPath(t, "o"), CoerceTo: "string"}},
		[]byte(`{"o":{"k":1}}`))
	if e == nil {
		t.Error("non-scalar coerce must fail")
	}
	out, e = ApplyRequest([]Op{{Kind: "coerce", Field: mustPath(t, "id"), CoerceTo: "string"}},
		[]byte(`{"id":1234567}`))
	if e != nil || string(out) != `{"id":"1234567"}` {
		t.Fatalf("large integer->string: got %s err %v", out, e)
	}
}

func TestApplyResponseSuccess(t *testing.T) {
	out, e := ApplyResponse([]Op{{Kind: "rename", From: mustPath(t, "internalId"), To: mustPath(t, "id")}},
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
		{Kind: "optionalize", Field: mustPath(t, "n")},
		{Kind: "coerce", Field: mustPath(t, "n"), CoerceTo: "string"},
	}, []byte(`{"keep":1}`))
	if e != nil {
		t.Fatalf("err: %v", e)
	}
	if string(out) != `{"keep":1}` {
		t.Errorf("got %s", out)
	}
}

func TestNonObjectBodyWithOpsFails(t *testing.T) {
	_, e := ApplyRequest([]Op{{Kind: KindDefault, Field: mustPath(t, "a"), Value: mustDV(t, 1)}}, []byte(`[1,2]`))
	if e == nil || e.HTTPStatus() != 422 {
		t.Fatalf("array body with ops must 422, got %v", e)
	}
}

func TestRenameInsideArrayElement(t *testing.T) {
	body := []byte(`{"items":[{"old":1},{"old":2}]}`)
	ops := []Op{{Kind: KindRename, From: mustPath(t, "items[].old"), To: mustPath(t, "items[].new")}}
	out, e := ApplyRequest(ops, body)
	if e != nil {
		t.Fatalf("err: %v", e)
	}
	if string(out) != `{"items":[{"new":1},{"new":2}]}` {
		t.Errorf("got %s", out)
	}
}

func TestDefaultAutoCreatesNestedObject(t *testing.T) {
	body := []byte(`{}`)
	ops := []Op{{Kind: KindDefault, Field: mustPath(t, "meta.locale"), Value: mustDV(t, "en-US")}}
	out, e := ApplyRequest(ops, body)
	if e != nil {
		t.Fatalf("err: %v", e)
	}
	if string(out) != `{"meta":{"locale":"en-US"}}` {
		t.Errorf("got %s", out)
	}
}

func TestDefaultFailsOnNonObjectIntermediate(t *testing.T) {
	body := []byte(`{"meta":"scalar"}`)
	ops := []Op{{Kind: KindDefault, Field: mustPath(t, "meta.locale"), Value: mustDV(t, "en-US")}}
	_, e := ApplyRequest(ops, body)
	if e == nil {
		t.Fatalf("non-object intermediate is a violation; must fail")
	}
}

func TestCoerceInsideArrayElement(t *testing.T) {
	body := []byte(`{"items":[{"id":1},{"id":2}]}`)
	ops := []Op{{Kind: KindCoerce, Field: mustPath(t, "items[].id"), CoerceTo: "string"}}
	out, e := ApplyRequest(ops, body)
	if e != nil {
		t.Fatalf("err: %v", e)
	}
	if string(out) != `{"items":[{"id":"1"},{"id":"2"}]}` {
		t.Errorf("got %s", out)
	}
}

func TestOptionalizePrefixCoversChildren(t *testing.T) {
	body := []byte(`{}`)
	ops := []Op{
		{Kind: KindOptionalize, Field: mustPath(t, "data")},
		{Kind: KindRename, From: mustPath(t, "data[].x"), To: mustPath(t, "data[].y")},
	}
	out, e := ApplyRequest(ops, body)
	if e != nil {
		t.Fatalf("prefix-optionalized missing 'data' should skip: %v", e)
	}
	if string(out) != `{}` {
		t.Errorf("got %s", out)
	}
}

func TestRenameMissingFromInsideArrayFailsUnlessOptionalized(t *testing.T) {
	body := []byte(`{"items":[{"x":1},{}]}`)
	ops := []Op{{Kind: KindRename, From: mustPath(t, "items[].x"), To: mustPath(t, "items[].y")}}
	_, e := ApplyRequest(ops, body)
	if e == nil {
		t.Fatalf("mixed-presence rename must fail per-element unless prefix-optionalized")
	}
	ops = []Op{
		{Kind: KindOptionalize, Field: mustPath(t, "items")},
		{Kind: KindRename, From: mustPath(t, "items[].x"), To: mustPath(t, "items[].y")},
	}
	out, e := ApplyRequest(ops, body)
	if e != nil {
		t.Fatalf("prefix-optionalized mixed-presence should skip: %v", e)
	}
	if string(out) != `{"items":[{"y":1},{}]}` {
		t.Errorf("got %s", out)
	}
}
