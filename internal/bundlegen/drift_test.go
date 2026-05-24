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
		{Verb: transform.KindDefault, Args: DefaultArgs{Field: "locale", Value: ""}, Confidence: Confident, Signals: []Signal{PureAddSignal()}},
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
		{Verb: transform.KindDefault, Args: DefaultArgs{Field: "region", Value: ""}, Confidence: Confident, Signals: []Signal{PureAddSignal()}},
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
		{Verb: transform.KindCoerce, Args: CoerceArgs{Field: "id", To: "string"}, Confidence: Confident, Signals: []Signal{TypeChangeSignal(), FromTypeSignal("int32"), ToTypeSignal("string")}},
	}
	if !reflect.DeepEqual(p.Request, wantReq) {
		t.Errorf("request proposals = %+v, want %+v", p.Request, wantReq)
	}
	wantResp := []StanzaProposal{
		{Verb: transform.KindCoerce, Args: CoerceArgs{Field: "count", To: "string"}, Confidence: Confident, Signals: []Signal{TypeChangeSignal(), FromTypeSignal("int64"), ToTypeSignal("string")}},
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
	// Without the ambiguous match, `legacy` and (no peer) would simply note. But
	// the ambiguous pair-matcher will pair `legacy` (string) with no addition
	// because there's none; expect a Note. Same for `user` — kept on both sides.
	// Since there is no addition to pair with, `legacy` falls through as a Note.
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
	// the verb set has no way to strip them from the response. The
	// drafter records a Note for the operator instead of a stanza.
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

func TestDraftShimCaseRenameRequest(t *testing.T) {
	// A removed `userName` paired with an added `user_name` of the same
	// type is a confident case-rename; the drafter emits a `rename`
	// stanza on the request side rather than a separate add+remove.
	from := openAPIDoc(t, "2024-01", `"userName":{"type":"string"}`, `"ok":{"type":"boolean"}`)
	to := openAPIDoc(t, "2024-06", `"user_name":{"type":"string"}`, `"ok":{"type":"boolean"}`)

	p, err := DraftShim(from, to)
	if err != nil {
		t.Fatalf("DraftShim: %v", err)
	}
	want := []StanzaProposal{
		{Verb: transform.KindRename, Args: RenameArgs{From: "userName", To: "user_name"}, Confidence: Confident, Signals: []Signal{CaseRenameSignal(), SameTypeSignal("string")}},
	}
	if !reflect.DeepEqual(p.Request, want) {
		t.Errorf("request proposals = %+v, want %+v", p.Request, want)
	}
	if len(p.Notes) != 0 {
		t.Errorf("expected no notes — rename absorbed the pair; got %v", p.Notes)
	}
}

func TestDraftShimCaseRenameResponse(t *testing.T) {
	// For the response side the transform reshapes backend→old-client, so
	// the rename's `from` is the backend's name (in `to/openapi.json`) and
	// `to` is the old contract's name (in `from/openapi.json`).
	from := openAPIDoc(t, "2024-01", `"user":{"type":"string"}`, `"userName":{"type":"string"}`)
	to := openAPIDoc(t, "2024-06", `"user":{"type":"string"}`, `"user_name":{"type":"string"}`)

	p, err := DraftShim(from, to)
	if err != nil {
		t.Fatalf("DraftShim: %v", err)
	}
	want := []StanzaProposal{
		{Verb: transform.KindRename, Args: RenameArgs{From: "user_name", To: "userName"}, Confidence: Confident, Signals: []Signal{CaseRenameSignal(), SameTypeSignal("string")}},
	}
	if !reflect.DeepEqual(p.Response, want) {
		t.Errorf("response proposals = %+v, want %+v", p.Response, want)
	}
}

func TestDraftShimDifferentTypesNotRenamed(t *testing.T) {
	// Removed `text` (string) and added `count` (integer) cannot be a
	// rename — different types. The pure-diff behavior holds: a Note for
	// the removal, a `default` for the addition.
	from := openAPIDoc(t, "2024-01", `"text":{"type":"string"}`, `"ok":{"type":"boolean"}`)
	to := openAPIDoc(t, "2024-06", `"count":{"type":"integer"}`, `"ok":{"type":"boolean"}`)

	p, err := DraftShim(from, to)
	if err != nil {
		t.Fatalf("DraftShim: %v", err)
	}
	wantReq := []StanzaProposal{
		{Verb: transform.KindDefault, Args: DefaultArgs{Field: "count", Value: 0}, Confidence: Confident, Signals: []Signal{PureAddSignal()}},
	}
	if !reflect.DeepEqual(p.Request, wantReq) {
		t.Errorf("request proposals = %+v, want %+v", p.Request, wantReq)
	}
	if len(p.Notes) != 1 || !strings.Contains(p.Notes[0], `"text"`) {
		t.Errorf("expected one note for removed request field %q; got %v", "text", p.Notes)
	}
}

func TestDraftShimUnrelatedNamesProduceCandidates(t *testing.T) {
	// `text` and `message` are the canonical ambiguous case — same type,
	// no shared structure. The drafter surfaces them as a candidate pair
	// the renderer emits as an `# OPTION A — rename` / `# OPTION B —
	// delete + add` block; no auto-rename, no note.
	from := openAPIDoc(t, "2024-01", `"text":{"type":"string"}`, `"ok":{"type":"boolean"}`)
	to := openAPIDoc(t, "2024-06", `"message":{"type":"string"}`, `"ok":{"type":"boolean"}`)

	p, err := DraftShim(from, to)
	if err != nil {
		t.Fatalf("DraftShim: %v", err)
	}
	for _, s := range p.Request {
		if s.Verb == transform.KindRename {
			t.Errorf("unrelated names must not auto-rename; got %+v", s)
		}
	}
	wantCands := []CandidatePair{
		{From: "text", To: "message", Type: "string", Signals: []Signal{SameTypeSignal("string"), NoNameSignal()}},
	}
	if !reflect.DeepEqual(p.RequestCandidates, wantCands) {
		t.Errorf("request candidates = %+v, want %+v", p.RequestCandidates, wantCands)
	}
	if len(p.Notes) != 0 {
		t.Errorf("expected no notes — the ambiguous pair absorbed both ends; got %v", p.Notes)
	}
}

func TestDraftShimStrictForcesCandidatesForCaseRenames(t *testing.T) {
	// Default mode auto-matches userName ↔ user_name as a confident
	// rename. Strict mode suppresses every confident match and reports
	// the pair as a candidate instead — the operator chooses.
	from := openAPIDoc(t, "2024-01", `"userName":{"type":"string"}`, `"ok":{"type":"boolean"}`)
	to := openAPIDoc(t, "2024-06", `"user_name":{"type":"string"}`, `"ok":{"type":"boolean"}`)

	p, err := DraftShimStrict(from, to)
	if err != nil {
		t.Fatalf("DraftShimStrict: %v", err)
	}
	for _, s := range p.Request {
		if s.Verb == transform.KindRename {
			t.Errorf("strict mode should not auto-rename; got %+v", s)
		}
	}
	wantCands := []CandidatePair{
		{From: "userName", To: "user_name", Type: "string", Signals: []Signal{SameTypeSignal("string"), NoNameSignal()}},
	}
	if !reflect.DeepEqual(p.RequestCandidates, wantCands) {
		t.Errorf("request candidates = %+v, want %+v", p.RequestCandidates, wantCands)
	}
}

func TestRenderYAMLIncludesConfidentStanzas(t *testing.T) {
	from := openAPIDoc(t, "2024-01", `"userName":{"type":"string"}`, `"ok":{"type":"boolean"}`)
	to := openAPIDoc(t, "2024-06", `"user_name":{"type":"string"},"locale":{"type":"string"}`, `"ok":{"type":"boolean"}`)

	p, err := DraftShim(from, to)
	if err != nil {
		t.Fatalf("DraftShim: %v", err)
	}
	out := p.RenderYAML(RenderOptions{Date: "2026-05-23"})

	for _, want := range []string{
		"# Drafted by `bundle draft-shim` on 2026-05-23.",
		`- contract_version: "2024-01"`,
		`target: "2024-06"`,
		"    request:",
		"      # CONFIDENT: case-rename, same-type:string",
		`      - rename: { from: "userName", to: "user_name" }`,
		"      # CONFIDENT: pure-add",
		`      - default: { field: "locale", value: "" }`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered YAML missing %q\n--- output ---\n%s", want, out)
		}
	}
	if strings.Contains(out, "AMBIGUOUS") {
		t.Errorf("no candidates expected; got AMBIGUOUS block:\n%s", out)
	}
}

func TestRenderYAMLEmitsCandidatesBlock(t *testing.T) {
	from := openAPIDoc(t, "2024-01", `"text":{"type":"string"}`, `"ok":{"type":"boolean"}`)
	to := openAPIDoc(t, "2024-06", `"message":{"type":"string"}`, `"ok":{"type":"boolean"}`)

	p, err := DraftShim(from, to)
	if err != nil {
		t.Fatalf("DraftShim: %v", err)
	}
	out := p.RenderYAML(RenderOptions{Date: "2026-05-23"})

	for _, want := range []string{
		`# AMBIGUOUS pair "text" ↔ "message"`,
		"# Pick exactly one option, edit, and delete the other comment block:",
		"# OPTION A — interpret as a rename:",
		`# - rename: { from: "text", to: "message" }`,
		"# OPTION B — interpret as separate add + remove:",
		`# - default: { field: "message", value: "" }`,
		"# (note: dropping the obsolete request-side \"text\"",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered YAML missing %q\n--- output ---\n%s", want, out)
		}
	}
	// The candidate block must remain inside the comment lane — a
	// reviewer pasting the file unedited gets a no-op override, never an
	// accidental auto-rename.
	for _, line := range strings.Split(out, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "- rename:") && !strings.HasPrefix(trimmed, "# - rename:") {
			t.Errorf("candidate rename leaked outside the comment lane: %q", line)
		}
	}
}

func TestRenderYAMLOmitsEmptyDirection(t *testing.T) {
	// Only the request side has a change; the response direction has no
	// stanzas. The renderer should skip the `response:` key entirely
	// (matching the bundle's convention of treating absent keys as no-op).
	from := openAPIDoc(t, "2024-01", `"user":{"type":"string"}`, `"ok":{"type":"boolean"}`)
	to := openAPIDoc(t, "2024-06", `"user":{"type":"string"},"locale":{"type":"string"}`, `"ok":{"type":"boolean"}`)

	p, err := DraftShim(from, to)
	if err != nil {
		t.Fatalf("DraftShim: %v", err)
	}
	out := p.RenderYAML(RenderOptions{Date: "2026-05-23"})
	if strings.Contains(out, "    response:") {
		t.Errorf("rendered YAML should omit empty response direction:\n%s", out)
	}
	if !strings.Contains(out, "    request:") {
		t.Errorf("rendered YAML should keep the populated request direction:\n%s", out)
	}
}

func TestRenderYAMLIncludesNotes(t *testing.T) {
	// A removed-from-new request field becomes a Note (the verb set has
	// no drop verb). The renderer surfaces every Note in a top comment
	// block.
	from := openAPIDoc(t, "2024-01", `"user":{"type":"string"},"legacy":{"type":"object","properties":{}}`, `"ok":{"type":"boolean"}`)
	to := openAPIDoc(t, "2024-06", `"user":{"type":"string"}`, `"ok":{"type":"boolean"}`)

	p, err := DraftShim(from, to)
	if err != nil {
		t.Fatalf("DraftShim: %v", err)
	}
	if len(p.Notes) == 0 {
		t.Fatal("expected at least one note (non-primitive remove)")
	}
	out := p.RenderYAML(RenderOptions{Date: "2026-05-23"})
	if !strings.Contains(out, "# Notes from the drafter (these need human reconciliation):") {
		t.Errorf("rendered YAML missing Notes header:\n%s", out)
	}
	if !strings.Contains(out, `"legacy"`) {
		t.Errorf("rendered YAML missing legacy-field note content:\n%s", out)
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
