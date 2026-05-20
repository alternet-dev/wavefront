package transform

import (
	"net/http"
	"testing"

	"github.com/alternet-dev/wavefront/internal/wireerror"
)

func failReq(reason string) *wireerror.Error { return wireerror.TransformFailedRequest(reason) }

func TestWalkLeavesTopLevel(t *testing.T) {
	obj := map[string]any{"text": "hi"}
	p, _ := ParsePath("text")
	var seen []string
	werr := walkLeaves(obj, p, nil, failReq, func(parent map[string]any, leaf string) *wireerror.Error {
		seen = append(seen, leaf)
		return nil
	})
	if werr != nil {
		t.Fatalf("unexpected: %v", werr)
	}
	if len(seen) != 1 || seen[0] != "text" {
		t.Errorf("got %v want [text]", seen)
	}
}

func TestWalkLeavesNestedObject(t *testing.T) {
	obj := map[string]any{"data": map[string]any{"user": map[string]any{"email": "a@b"}}}
	p, _ := ParsePath("data.user.email")
	var seenVal any
	werr := walkLeaves(obj, p, nil, failReq, func(parent map[string]any, leaf string) *wireerror.Error {
		seenVal = parent[leaf]
		return nil
	})
	if werr != nil || seenVal != "a@b" {
		t.Errorf("seenVal=%v werr=%v", seenVal, werr)
	}
}

func TestWalkLeavesArrayIteration(t *testing.T) {
	obj := map[string]any{"data": []any{
		map[string]any{"id": 1.0},
		map[string]any{"id": 2.0},
		map[string]any{"id": 3.0},
	}}
	p, _ := ParsePath("data[].id")
	var ids []any
	werr := walkLeaves(obj, p, nil, failReq, func(parent map[string]any, leaf string) *wireerror.Error {
		ids = append(ids, parent[leaf])
		return nil
	})
	if werr != nil {
		t.Fatalf("err: %v", werr)
	}
	if len(ids) != 3 || ids[0] != 1.0 || ids[2] != 3.0 {
		t.Errorf("got %v", ids)
	}
}

func TestWalkLeavesEmptyArrayIsSilent(t *testing.T) {
	obj := map[string]any{"data": []any{}}
	p, _ := ParsePath("data[].id")
	called := 0
	werr := walkLeaves(obj, p, nil, failReq, func(map[string]any, string) *wireerror.Error {
		called++
		return nil
	})
	if werr != nil || called != 0 {
		t.Errorf("empty array should be silent no-op; called=%d werr=%v", called, werr)
	}
}

func TestWalkLeavesMissingIntermediateFailsUnlessOptionalized(t *testing.T) {
	obj := map[string]any{}
	p, _ := ParsePath("data.user.email")
	werr := walkLeaves(obj, p, nil, failReq, func(map[string]any, string) *wireerror.Error { return nil })
	if werr == nil || werr.HTTPStatus() != http.StatusUnprocessableEntity {
		t.Fatalf("missing intermediate must 422-fail; got %v", werr)
	}
	// With optionalize covering "data" (prefix), skip silently.
	opts := newOptionalizedSet()
	dataP, _ := ParsePath("data")
	opts.add(dataP)
	werr = walkLeaves(obj, p, opts, failReq, func(map[string]any, string) *wireerror.Error { return nil })
	if werr != nil {
		t.Errorf("prefix-optionalized missing intermediate should skip silently; got %v", werr)
	}
}

func TestWalkLeavesNonObjectIntermediateFails(t *testing.T) {
	obj := map[string]any{"data": "scalar-not-object"}
	p, _ := ParsePath("data.user")
	werr := walkLeaves(obj, p, nil, failReq, func(map[string]any, string) *wireerror.Error { return nil })
	if werr == nil {
		t.Errorf("non-object intermediate must fail")
	}
}

func TestWalkLeavesNonArrayAtArraySegmentFails(t *testing.T) {
	obj := map[string]any{"data": map[string]any{"x": 1}}
	p, _ := ParsePath("data[].x")
	werr := walkLeaves(obj, p, nil, failReq, func(map[string]any, string) *wireerror.Error { return nil })
	if werr == nil {
		t.Errorf("data is not an array; data[] must fail")
	}
}

func TestWalkLeavesEnsureObjectsAutoCreates(t *testing.T) {
	obj := map[string]any{}
	p, _ := ParsePath("a.b.c")
	var setLeaf string
	werr := walkLeavesEnsureObjects(obj, p, failReq, func(parent map[string]any, leaf string) *wireerror.Error {
		setLeaf = leaf
		parent[leaf] = "v"
		return nil
	})
	if werr != nil {
		t.Fatalf("err: %v", werr)
	}
	if setLeaf != "c" {
		t.Errorf("leaf=%q", setLeaf)
	}
	a, _ := obj["a"].(map[string]any)
	b, _ := a["b"].(map[string]any)
	if b["c"] != "v" {
		t.Errorf("auto-create chain failed: %v", obj)
	}
}

func TestWalkLeavesEnsureObjectsFailsOnNonObjectIntermediate(t *testing.T) {
	obj := map[string]any{"a": "scalar"}
	p, _ := ParsePath("a.b")
	werr := walkLeavesEnsureObjects(obj, p, failReq, func(map[string]any, string) *wireerror.Error { return nil })
	if werr == nil {
		t.Errorf("non-object existing intermediate is a violation; must fail")
	}
}

func TestWalkLeavesEnsureObjectsFailsOnMissingArrayIntermediate(t *testing.T) {
	obj := map[string]any{}
	p, _ := ParsePath("data[].x")
	werr := walkLeavesEnsureObjects(obj, p, failReq, func(map[string]any, string) *wireerror.Error { return nil })
	if werr == nil {
		t.Errorf("missing array intermediate cannot be auto-created; must fail")
	}
}
