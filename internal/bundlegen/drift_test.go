package bundlegen

import (
	"reflect"
	"strings"
	"testing"

	"github.com/alternet-dev/wavefront/internal/transform"
)

// openAPI document fixtures for drift detection. Each helper returns a
// minimal, syntactically-valid OpenAPI 3 document with one operation, used
// to exercise specific structural drifts in isolation.
func openAPIDoc(t *testing.T, version, requestProps, responseProps string) []byte {
	t.Helper()
	return []byte(`{
"openapi":"3.0.0",
"info":{"title":"t","version":"` + version + `"},
"paths":{"/v/echo":{"post":{
  "operationId":"echo",
  "requestBody":{"content":{"application/json":{"schema":{"$ref":"#/components/schemas/Req"}}}},
  "responses":{"200":{"content":{"application/json":{"schema":{"$ref":"#/components/schemas/Resp"}}}}}
}}},
"components":{"schemas":{
  "Req":{"type":"object","properties":{` + requestProps + `}},
  "Resp":{"type":"object","properties":{` + responseProps + `}}
}}}`)
}

func TestDraftShimAddedRequestFieldDraftsDefault(t *testing.T) {
	from := openAPIDoc(t, "2024-01", `"user":{"type":"string"}`, `"ok":{"type":"boolean"}`)
	to := openAPIDoc(t, "2024-06", `"user":{"type":"string"},"locale":{"type":"string"}`, `"ok":{"type":"boolean"}`)

	p, err := DraftShim(from, to)
	if err != nil {
		t.Fatalf("DraftShim: %v", err)
	}
	want := []StanzaProposal{
		{Verb: transform.KindDefault, Args: DefaultArgs{Field: "locale", Value: ""}, Confidence: Confident, Signals: []string{"pure-add"}},
	}
	if !reflect.DeepEqual(p.Request, want) {
		t.Errorf("request proposals = %+v, want %+v", p.Request, want)
	}
	if len(p.Response) != 0 {
		t.Errorf("response proposals = %+v, want empty", p.Response)
	}
	if len(p.Notes) != 0 {
		t.Errorf("notes = %v, want empty", p.Notes)
	}
}

func TestDraftShimRemovedResponseFieldDraftsDefault(t *testing.T) {
	// Backend stopped returning `region`; the old contract still expects it.
	from := openAPIDoc(t, "2024-01", `"user":{"type":"string"}`, `"name":{"type":"string"},"region":{"type":"string"}`)
	to := openAPIDoc(t, "2024-06", `"user":{"type":"string"}`, `"name":{"type":"string"}`)

	p, err := DraftShim(from, to)
	if err != nil {
		t.Fatalf("DraftShim: %v", err)
	}
	want := []StanzaProposal{
		{Verb: transform.KindDefault, Args: DefaultArgs{Field: "region", Value: ""}, Confidence: Confident, Signals: []string{"pure-add"}},
	}
	if !reflect.DeepEqual(p.Response, want) {
		t.Errorf("response proposals = %+v, want %+v", p.Response, want)
	}
}

func TestDraftShimTypeChangeDraftsCoerce(t *testing.T) {
	// On request, `id` changes int → string; on response, `count` changes string → int64.
	from := openAPIDoc(t, "2024-01",
		`"id":{"type":"integer"}`,
		`"count":{"type":"string"}`)
	to := openAPIDoc(t, "2024-06",
		`"id":{"type":"string"}`,
		`"count":{"type":"integer","format":"int64"}`)

	p, err := DraftShim(from, to)
	if err != nil {
		t.Fatalf("DraftShim: %v", err)
	}
	wantReq := []StanzaProposal{
		{Verb: transform.KindCoerce, Args: CoerceArgs{Field: "id", To: "string"}, Confidence: Confident, Signals: []string{"type-change", "from:int32", "to:string"}},
	}
	if !reflect.DeepEqual(p.Request, wantReq) {
		t.Errorf("request proposals = %+v, want %+v", p.Request, wantReq)
	}
	wantResp := []StanzaProposal{
		{Verb: transform.KindCoerce, Args: CoerceArgs{Field: "count", To: "string"}, Confidence: Confident, Signals: []string{"type-change", "from:int64", "to:string"}},
	}
	if !reflect.DeepEqual(p.Response, wantResp) {
		t.Errorf("response proposals = %+v, want %+v", p.Response, wantResp)
	}
}

func TestDraftShimRemovedRequestFieldNotesUnsupported(t *testing.T) {
	// Old client sends `legacy`; backend no longer accepts it. The verb
	// set has no way to drop a known field; this must be surfaced for
	// human attention.
	from := openAPIDoc(t, "2024-01", `"user":{"type":"string"},"legacy":{"type":"string"}`, `"ok":{"type":"boolean"}`)
	to := openAPIDoc(t, "2024-06", `"user":{"type":"string"}`, `"ok":{"type":"boolean"}`)

	p, err := DraftShim(from, to)
	if err != nil {
		t.Fatalf("DraftShim: %v", err)
	}
	if len(p.Request) != 0 {
		t.Errorf("request proposals = %+v, want empty (drop-unknown is unsupported)", p.Request)
	}
	if len(p.Notes) != 1 || !strings.Contains(p.Notes[0], `"legacy"`) || !strings.Contains(p.Notes[0], "request") {
		t.Errorf("expected one note naming request field %q; got %v", "legacy", p.Notes)
	}
}

func TestDraftShimAddedResponseFieldNotesUnsupported(t *testing.T) {
	// Backend now returns `region`; the old contract doesn't declare it.
	// The adapter's strict JSON unmarshal can't see unknown fields, and
	// the verb set has no way to strip them from the response.
	from := openAPIDoc(t, "2024-01", `"user":{"type":"string"}`, `"name":{"type":"string"}`)
	to := openAPIDoc(t, "2024-06", `"user":{"type":"string"}`, `"name":{"type":"string"},"region":{"type":"string"}`)

	p, err := DraftShim(from, to)
	if err != nil {
		t.Fatalf("DraftShim: %v", err)
	}
	if len(p.Response) != 0 {
		t.Errorf("response proposals = %+v, want empty (strip-unknown is unsupported)", p.Response)
	}
	if len(p.Notes) != 1 || !strings.Contains(p.Notes[0], `"region"`) || !strings.Contains(p.Notes[0], "response") {
		t.Errorf("expected one note naming response field %q; got %v", "region", p.Notes)
	}
}

func TestDraftShimVersionsRecorded(t *testing.T) {
	from := openAPIDoc(t, "2024-01", `"user":{"type":"string"}`, `"ok":{"type":"boolean"}`)
	to := openAPIDoc(t, "2024-06", `"user":{"type":"string"}`, `"ok":{"type":"boolean"}`)
	p, err := DraftShim(from, to)
	if err != nil {
		t.Fatalf("DraftShim: %v", err)
	}
	if p.FromVersion != "2024-01" || p.ToVersion != "2024-06" {
		t.Errorf("versions = %q→%q, want 2024-01→2024-06", p.FromVersion, p.ToVersion)
	}
}
