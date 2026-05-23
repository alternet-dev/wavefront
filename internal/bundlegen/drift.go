package bundlegen

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/alternet-dev/wavefront/internal/transform"
)

// Confidence labels how trustworthy a drafted stanza is. Confident stanzas
// follow from unambiguous shape changes; Ambiguous stanzas need operator
// judgment between alternative interpretations.
type Confidence string

const (
	Confident Confidence = "confident"
	Ambiguous Confidence = "ambiguous"
)

// StanzaArgs is the sealed set of typed arguments for the transform verbs
// the drafter can emit. Each implementer corresponds to exactly one
// transform.Kind; a StanzaProposal's Verb selects which implementer is
// expected to populate Args.
type StanzaArgs interface {
	isStanzaArgs()
}

// RenameArgs are the arguments of a `rename` stanza.
type RenameArgs struct {
	From, To string
}

// DefaultArgs are the arguments of a `default` stanza.
type DefaultArgs struct {
	Field string
	Value any
}

// OptionalizeArgs are the arguments of an `optionalize` stanza.
type OptionalizeArgs struct {
	Field string
}

// CoerceArgs are the arguments of a `coerce` stanza.
type CoerceArgs struct {
	Field string
	To    string
}

func (RenameArgs) isStanzaArgs()      {}
func (DefaultArgs) isStanzaArgs()     {}
func (OptionalizeArgs) isStanzaArgs() {}
func (CoerceArgs) isStanzaArgs()      {}

// SignalKind enumerates the structural observations the drafter can
// record about a proposed stanza or candidate pair. A bare kind
// describes a structural fact on its own; a parametric kind carries a
// primitive-type token in the accompanying Signal.Value.
type SignalKind string

const (
	SignalKindPureAdd    SignalKind = "pure-add"
	SignalKindTypeChange SignalKind = "type-change"
	SignalKindFromType   SignalKind = "from"
	SignalKindToType     SignalKind = "to"
	SignalKindCaseRename SignalKind = "case-rename"
	SignalKindSameType   SignalKind = "same-type"
	SignalKindNoName     SignalKind = "no-name-signal"
)

// Signal is one structural observation the drafter recorded about a
// proposed stanza or candidate pair. Bare signals carry no Value;
// parametric signals carry a primitive-type token. Build a Signal
// through one of the constructor functions below — they enforce the
// bare-vs-parametric invariant — so a renderer downstream can rely on
// String() emitting the canonical "kind" or "kind:value" form without
// re-validating.
type Signal struct {
	Kind  SignalKind
	Value string // empty for bare signals
}

// String returns the canonical surfaced form: "kind" for bare signals,
// "kind:value" for parametric ones. The renderer joins these with ", "
// when emitting the explanatory YAML comment alongside a stanza.
func (s Signal) String() string {
	if s.Value == "" {
		return string(s.Kind)
	}
	return string(s.Kind) + ":" + s.Value
}

// PureAddSignal is the bare observation that a field is present in the
// "to" shape only.
func PureAddSignal() Signal { return Signal{Kind: SignalKindPureAdd} }

// TypeChangeSignal is the bare observation that a field's primitive
// type changed; pair it with FromTypeSignal and ToTypeSignal to carry
// the before/after tokens.
func TypeChangeSignal() Signal { return Signal{Kind: SignalKindTypeChange} }

// FromTypeSignal carries the "from" side primitive type of a change.
func FromTypeSignal(t string) Signal { return Signal{Kind: SignalKindFromType, Value: t} }

// ToTypeSignal carries the "to" side primitive type of a change.
func ToTypeSignal(t string) Signal { return Signal{Kind: SignalKindToType, Value: t} }

// CaseRenameSignal is the bare observation that two field names share
// their case-and-separator-normalized form.
func CaseRenameSignal() Signal { return Signal{Kind: SignalKindCaseRename} }

// SameTypeSignal carries the shared primitive type of a rename or
// candidate pair.
func SameTypeSignal(t string) Signal { return Signal{Kind: SignalKindSameType, Value: t} }

// NoNameSignal is the bare observation that a pair shares no name signal
// at all (same type but otherwise unrelated identifiers).
func NoNameSignal() Signal { return Signal{Kind: SignalKindNoName} }

// StanzaProposal is one drafted transform stanza, before any YAML
// rendering. Verb is the canonical transform.Kind that selects which
// StanzaArgs implementer is held in Args; Signals records the structural
// observations that produced the proposal so the renderer can surface
// *why* the drafter emitted it.
type StanzaProposal struct {
	Verb       transform.Kind
	Args       StanzaArgs
	Confidence Confidence
	Signals    []Signal
}

// CandidatePair is one ambiguous rename: a removed field and an added
// field that share a primitive type but cannot be auto-renamed without
// guessing operator intent. The renderer emits both interpretations
// side by side in a YAML candidates block so a reviewer commits to one.
type CandidatePair struct {
	From, To string
	Type     string
	Signals  []Signal
}

// ShimProposal is the draft of a single resolution.yaml override entry:
// request and response stanza lists, candidate (ambiguous) pairs, plus
// any notes the drafter could not express mechanically.
type ShimProposal struct {
	FromVersion        string
	ToVersion          string
	Request            []StanzaProposal
	RequestCandidates  []CandidatePair
	Response           []StanzaProposal
	ResponseCandidates []CandidatePair
	Notes              []string
}

// DraftShim compares two OpenAPI documents — the frozen "from" contract
// and the drifted "to" contract — and proposes a transform-shim override
// that bridges them. It detects four change classes at the request and
// response message's top-level fields:
//
//   - a field in the "to" shape that the "from" shape lacks (a pure
//     addition) becomes a `default` stanza filling the new field with a
//     type-appropriate placeholder
//   - a field present in both but with a changed primitive type becomes
//     a `coerce` stanza
//   - a removed/added pair whose case-normalized names match becomes a
//     confident `rename` stanza
//   - a removed/added pair sharing a primitive type but with
//     differently-normalized names becomes an ambiguous candidate the
//     renderer surfaces as both `rename` and `delete + add` alternatives
//
// A field in the "from" shape that has no peer left after pair matching
// becomes a Note — the transform verb set has no way to drop a known
// field; the operator must reconcile by hand. Top-level object
// properties only; nested objects, arrays, and oneOf/allOf are not yet
// supported.
func DraftShim(fromOpenAPI, toOpenAPI []byte) (*ShimProposal, error) {
	return draftShim(fromOpenAPI, toOpenAPI, false)
}

// DraftShimStrict is the operator escape hatch: it suppresses every
// confident case-rename match and reports each removed/added pair as a
// candidate instead. Use when the drafter's confidence is itself the
// thing in question — for example when reviewing a high-stakes contract
// drift and an auto-rename would short-circuit a decision the operator
// wants to make.
func DraftShimStrict(fromOpenAPI, toOpenAPI []byte) (*ShimProposal, error) {
	return draftShim(fromOpenAPI, toOpenAPI, true)
}

func draftShim(fromOpenAPI, toOpenAPI []byte, strict bool) (*ShimProposal, error) {
	fromDoc, err := parseOpenAPIDoc(fromOpenAPI)
	if err != nil {
		return nil, fmt.Errorf("parse from: %w", err)
	}
	toDoc, err := parseOpenAPIDoc(toOpenAPI)
	if err != nil {
		return nil, fmt.Errorf("parse to: %w", err)
	}

	fromReq, fromResp, err := operationSchemas(fromDoc)
	if err != nil {
		return nil, fmt.Errorf("resolve from schemas: %w", err)
	}
	toReq, toResp, err := operationSchemas(toDoc)
	if err != nil {
		return nil, fmt.Errorf("resolve to schemas: %w", err)
	}

	p := &ShimProposal{
		FromVersion: fromDoc.Info.Version,
		ToVersion:   toDoc.Info.Version,
	}

	// Request direction: old client speaks `fromReq`, backend expects `toReq`.
	// The transform reshapes the body from the "from" side to the "to" side.
	reqProps, reqCands, reqNotes := draftDirection(fromReq, toReq, "request", strict)
	p.Request = reqProps
	p.RequestCandidates = reqCands
	p.Notes = append(p.Notes, reqNotes...)

	// Response direction: backend returns `toResp`, old client expects
	// `fromResp`. The transform reshapes from `toResp` to `fromResp`.
	respProps, respCands, respNotes := draftDirection(toResp, fromResp, "response", strict)
	p.Response = respProps
	p.ResponseCandidates = respCands
	p.Notes = append(p.Notes, respNotes...)

	return p, nil
}

// parseOpenAPIDoc decodes a raw OpenAPI document into the local openAPI
// shape the rest of bundlegen operates on.
func parseOpenAPIDoc(data []byte) (openAPI, error) {
	var doc openAPI
	if err := json.Unmarshal(data, &doc); err != nil {
		return doc, err
	}
	return doc, nil
}

// operationSchemas resolves the request and response component schemas of
// an OpenAPI document with exactly one operation (the same shape
// `bundlegen.Add` already requires).
func operationSchemas(doc openAPI) (req, resp *schema, err error) {
	_, _, op, err := singleOperation(doc)
	if err != nil {
		return nil, nil, err
	}
	reqName, err := refSchemaName(op, true)
	if err != nil {
		return nil, nil, fmt.Errorf("request: %w", err)
	}
	respName, err := refSchemaName(op, false)
	if err != nil {
		return nil, nil, fmt.Errorf("response: %w", err)
	}
	schemas, err := collectSchemas(doc, []string{reqName, respName})
	if err != nil {
		return nil, nil, err
	}
	req, ok := schemas[reqName]
	if !ok || req == nil {
		return nil, nil, fmt.Errorf("request schema %q not found", reqName)
	}
	resp, ok = schemas[respName]
	if !ok || resp == nil {
		return nil, nil, fmt.Errorf("response schema %q not found", respName)
	}
	return req, resp, nil
}

// draftDirection diffs one direction of a transform: a `from` shape that
// arrives and a `to` shape that must leave. It emits stanzas for the
// changes the transform verbs cover, candidate pairs for the ambiguous
// ones, and notes for everything it cannot mechanically express.
//
// When `strict` is set, the confident case-rename match is suppressed —
// every removed/added primitive-type pair is reported as a candidate
// instead of an auto-rename. The operator escape hatch when the
// drafter's confidence is itself the thing in question.
func draftDirection(from, to *schema, side string, strict bool) ([]StanzaProposal, []CandidatePair, []string) {
	fromFields := topLevelProperties(from)
	toFields := topLevelProperties(to)

	fromNames := sortedKeys(fromFields)
	toNames := sortedKeys(toFields)
	inTo := makeSet(toNames)
	inFrom := makeSet(fromNames)

	var removed, added []string
	for _, n := range fromNames {
		if !inTo[n] {
			removed = append(removed, n)
		}
	}
	for _, n := range toNames {
		if !inFrom[n] {
			added = append(added, n)
		}
	}

	var (
		renames                          []renamePair
		unmatchedRemoved, unmatchedAdded []string
	)
	if strict {
		// No confident matches; everything is fair game for the
		// ambiguous pass below.
		unmatchedRemoved = append(unmatchedRemoved, removed...)
		unmatchedAdded = append(unmatchedAdded, added...)
	} else {
		renames, unmatchedRemoved, unmatchedAdded = matchCaseRenames(removed, added, fromFields, toFields)
	}

	// Of the still-unmatched removals/additions, pair any with a shared
	// primitive type into an ambiguous candidate.
	candidates, unmatchedRemoved, unmatchedAdded := matchAmbiguousPairs(unmatchedRemoved, unmatchedAdded, fromFields, toFields)

	var props []StanzaProposal
	var notes []string

	// Confident renames take precedence over the underlying add/remove.
	for _, r := range renames {
		props = append(props, StanzaProposal{
			Verb:       transform.KindRename,
			Args:       RenameArgs{From: r.from, To: r.to},
			Confidence: Confident,
			Signals:    []Signal{CaseRenameSignal(), SameTypeSignal(primitiveType(fromFields[r.from]))},
		})
	}

	// Unmatched additions still need a `default` so an old client's
	// request body carries the new contract's required field.
	for _, name := range unmatchedAdded {
		props = append(props, StanzaProposal{
			Verb:       transform.KindDefault,
			Args:       DefaultArgs{Field: name, Value: zeroForSchema(toFields[name])},
			Confidence: Confident,
			Signals:    []Signal{PureAddSignal()},
		})
	}

	// Type changes for fields present in both — orthogonal to rename
	// matching, since both endpoints carry the same name.
	for _, name := range fromNames {
		if !inTo[name] {
			continue
		}
		fromT := primitiveType(fromFields[name])
		toT := primitiveType(toFields[name])
		if fromT == "" || toT == "" || fromT == toT {
			continue
		}
		props = append(props, StanzaProposal{
			Verb:       transform.KindCoerce,
			Args:       CoerceArgs{Field: name, To: toT},
			Confidence: Confident,
			Signals:    []Signal{TypeChangeSignal(), FromTypeSignal(fromT), ToTypeSignal(toT)},
		})
	}

	// Unmatched removals: the verb set has no way to drop a known field
	// on this side; surface as a note so the human can decide.
	for _, name := range unmatchedRemoved {
		notes = append(notes, fmt.Sprintf(
			"%s field %q is present in the from shape but absent from the to shape; the transform verb set has no way to drop a known field, so the operator must reconcile this manually",
			side, name))
	}

	return props, candidates, notes
}

// matchAmbiguousPairs pairs leftover removed and added fields by shared
// primitive type alone — no name signal. Each pair becomes a candidate
// the renderer surfaces as `# OPTION A — rename` vs
// `# OPTION B — delete + add`, leaving the choice to the operator.
//
// Like matchCaseRenames, this is one-to-one and order-stable: each
// removed field is considered once against the still-unconsumed
// additions in declared order. Pairing two fields with the same type
// but no other signal is intentionally weak — that's the *point*; the
// renderer flags it as ambiguous instead of guessing.
func matchAmbiguousPairs(removed, added []string, fromSchemas, toSchemas map[string]*schema) ([]CandidatePair, []string, []string) {
	usedAdded := make(map[string]bool)
	var pairs []CandidatePair
	var unmatchedRemoved []string

	for _, r := range removed {
		fromT := primitiveType(fromSchemas[r])
		if fromT == "" {
			unmatchedRemoved = append(unmatchedRemoved, r)
			continue
		}
		matched := ""
		for _, a := range added {
			if usedAdded[a] {
				continue
			}
			toT := primitiveType(toSchemas[a])
			if toT == "" || toT != fromT {
				continue
			}
			matched = a
			break
		}
		if matched == "" {
			unmatchedRemoved = append(unmatchedRemoved, r)
			continue
		}
		pairs = append(pairs, CandidatePair{
			From:    r,
			To:      matched,
			Type:    fromT,
			Signals: []Signal{SameTypeSignal(fromT), NoNameSignal()},
		})
		usedAdded[matched] = true
	}

	var unmatchedAdded []string
	for _, a := range added {
		if !usedAdded[a] {
			unmatchedAdded = append(unmatchedAdded, a)
		}
	}
	return pairs, unmatchedRemoved, unmatchedAdded
}

// renamePair is a single confident rename: the field name in the `from`
// shape and the field name it should take in the `to` shape.
type renamePair struct{ from, to string }

// matchCaseRenames pairs a removed field with an added field when their
// case-normalized names are identical and they share a primitive type.
// Anything else is left unmatched for the add/note behavior.
//
// The matching is one-to-one and order-stable: each removed field is
// considered once, against the still-unconsumed additions, in declared
// order. The current scoring is binary — a pair either matches both
// signals (normalized name + primitive type) or it doesn't.
func matchCaseRenames(removed, added []string, fromSchemas, toSchemas map[string]*schema) ([]renamePair, []string, []string) {
	usedAdded := make(map[string]bool)
	var pairs []renamePair
	var unmatchedRemoved []string

	for _, r := range removed {
		normR := normalizeFieldName(r)
		fromT := primitiveType(fromSchemas[r])
		if normR == "" || fromT == "" {
			unmatchedRemoved = append(unmatchedRemoved, r)
			continue
		}
		matched := ""
		for _, a := range added {
			if usedAdded[a] {
				continue
			}
			if normalizeFieldName(a) != normR {
				continue
			}
			toT := primitiveType(toSchemas[a])
			if toT == "" || toT != fromT {
				continue
			}
			matched = a
			break
		}
		if matched == "" {
			unmatchedRemoved = append(unmatchedRemoved, r)
			continue
		}
		pairs = append(pairs, renamePair{from: r, to: matched})
		usedAdded[matched] = true
	}

	var unmatchedAdded []string
	for _, a := range added {
		if !usedAdded[a] {
			unmatchedAdded = append(unmatchedAdded, a)
		}
	}
	return pairs, unmatchedRemoved, unmatchedAdded
}

// normalizeFieldName folds a field name to its case- and separator-free
// form. `userName`, `user_name`, `user-name`, and `USERNAME` all collapse
// to `username` — the signal the drafter uses to declare a confident
// rename.
func normalizeFieldName(s string) string {
	s = strings.ToLower(s)
	s = strings.ReplaceAll(s, "_", "")
	s = strings.ReplaceAll(s, "-", "")
	return s
}

// topLevelProperties returns the properties map of an object schema, or
// nil for a non-object / nil schema. The drafter limits its scope to a
// top-level object's properties.
func topLevelProperties(s *schema) map[string]*schema {
	if s == nil || s.Properties == nil {
		return nil
	}
	return s.Properties
}

func sortedKeys(m map[string]*schema) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func makeSet(xs []string) map[string]bool {
	out := make(map[string]bool, len(xs))
	for _, x := range xs {
		out[x] = true
	}
	return out
}

// primitiveType maps an OpenAPI schema to a transform-verb type token.
// Only primitives are recognised; arrays, objects, and $ref schemas
// return "".
func primitiveType(s *schema) string {
	if s == nil || s.Ref != "" {
		return ""
	}
	switch s.Type {
	case "string", "boolean":
		return s.Type
	case "integer":
		if s.Format == "int64" {
			return "int64"
		}
		return "int32"
	case "number":
		if s.Format == "float" {
			return "float"
		}
		return "double"
	}
	return ""
}

// zeroForSchema picks a JSON-appropriate zero value to seed a drafted
// `default` stanza. The operator is expected to revise the placeholder
// before committing the override.
func zeroForSchema(s *schema) any {
	if s == nil {
		return ""
	}
	switch s.Type {
	case "string":
		return ""
	case "boolean":
		return false
	case "integer", "number":
		return 0
	case "array":
		return []any{}
	case "object":
		return map[string]any{}
	}
	return ""
}

// RenderOptions tunes ShimProposal.RenderYAML output. Date sets the
// header comment's drafted-on stamp; an empty value resolves to today
// (UTC, YYYY-MM-DD).
type RenderOptions struct {
	Date string
}

// RenderYAML emits the shim proposal as a YAML list element ready to
// paste into a `resolution.yaml`'s `overrides:` block. Confident stanzas
// land as fully-formed YAML entries; ambiguous pairs appear as
// commented-out OPTION A / OPTION B blocks so the operator picks
// intent. Notes are rendered as a top comment block; the renderer never
// modifies a layer, never edits an existing resolution.yaml, and is
// idempotent — a no-op proposal still emits a header and an empty
// override skeleton so a reviewer sees the drafter ran.
func (p *ShimProposal) RenderYAML(opts RenderOptions) string {
	date := opts.Date
	if date == "" {
		date = time.Now().UTC().Format("2006-01-02")
	}

	var b strings.Builder
	fmt.Fprintf(&b, "# Drafted by `bundle draft-shim` on %s.\n", date)
	b.WriteString("# Review every block, edit as needed, then paste into bundle/resolution.yaml's `overrides:` list.\n")
	if len(p.Notes) > 0 {
		b.WriteString("#\n")
		b.WriteString("# Notes from the drafter (these need human reconciliation):\n")
		for _, n := range p.Notes {
			fmt.Fprintf(&b, "# - %s\n", n)
		}
	}
	b.WriteString("#\n")
	fmt.Fprintf(&b, "- contract_version: %q\n", p.FromVersion)
	b.WriteString("  transform:\n")
	fmt.Fprintf(&b, "    target: %q\n", p.ToVersion)
	renderDirection(&b, "    request", p.Request, p.RequestCandidates, "request")
	renderDirection(&b, "    response", p.Response, p.ResponseCandidates, "response")
	return b.String()
}

// renderDirection writes the stanzas + candidate blocks for one side of
// the transform. An empty direction omits the key entirely — a
// resolution.yaml with no stanzas on a side is treated as no-op anyway.
func renderDirection(b *strings.Builder, key string, props []StanzaProposal, cands []CandidatePair, side string) {
	if len(props) == 0 && len(cands) == 0 {
		return
	}
	fmt.Fprintf(b, "%s:\n", key)
	for _, s := range props {
		fmt.Fprintf(b, "      # CONFIDENT: %s\n", joinSignals(s.Signals))
		fmt.Fprintf(b, "      - %s: %s\n", s.Verb, renderArgs(s.Args))
	}
	for _, c := range cands {
		fmt.Fprintf(b, "      # AMBIGUOUS pair %q ↔ %q (%s).\n",
			c.From, c.To, joinSignals(c.Signals))
		b.WriteString("      # Pick exactly one option, edit, and delete the other comment block:\n")
		b.WriteString("      #\n")
		b.WriteString("      # OPTION A — interpret as a rename:\n")
		fmt.Fprintf(b, "      # - rename: %s\n", renderArgs(RenameArgs{From: c.From, To: c.To}))
		b.WriteString("      #\n")
		b.WriteString("      # OPTION B — interpret as separate add + remove:\n")
		fmt.Fprintf(b, "      # - default: %s\n", renderArgs(DefaultArgs{Field: c.To, Value: zeroForType(c.Type)}))
		fmt.Fprintf(b, "      # (note: dropping the obsolete %s-side %q is not expressible in the verb set; the operator must reconcile manually if OPTION B is chosen)\n", side, c.From)
	}
}

// joinSignals concatenates a list of Signal values via their canonical
// String() form. Centralised so the renderer never reaches inside the
// Signal contract.
func joinSignals(signals []Signal) string {
	parts := make([]string, len(signals))
	for i, s := range signals {
		parts[i] = s.String()
	}
	return strings.Join(parts, ", ")
}

// renderArgs writes a StanzaArgs value as compact YAML inline-flow
// ({key: val, ...}). The arg-key order is fixed per verb (e.g.
// `from`/`to` for rename, `field`/`value` for default) so the rendered
// YAML is deterministic across runs.
func renderArgs(args StanzaArgs) string {
	switch a := args.(type) {
	case RenameArgs:
		return fmt.Sprintf("{ from: %s, to: %s }", formatYAMLValue(a.From), formatYAMLValue(a.To))
	case DefaultArgs:
		return fmt.Sprintf("{ field: %s, value: %s }", formatYAMLValue(a.Field), formatYAMLValue(a.Value))
	case OptionalizeArgs:
		return fmt.Sprintf("{ field: %s }", formatYAMLValue(a.Field))
	case CoerceArgs:
		return fmt.Sprintf("{ field: %s, to: %s }", formatYAMLValue(a.Field), formatYAMLValue(a.To))
	default:
		return fmt.Sprintf("{ /* unsupported args type %T */ }", args)
	}
}

// formatYAMLValue emits a single arg value as a YAML scalar. Strings are
// quoted; numbers, booleans, and nil pass through directly. Other types
// fall back to %v — drafter args today are primitives only.
func formatYAMLValue(v any) string {
	switch t := v.(type) {
	case string:
		return fmt.Sprintf("%q", t)
	case bool:
		return fmt.Sprintf("%t", t)
	case int, int32, int64:
		return fmt.Sprintf("%d", t)
	case float32, float64:
		return fmt.Sprintf("%v", t)
	case nil:
		return "null"
	default:
		return fmt.Sprintf("%v", v)
	}
}

// zeroForType returns a JSON-appropriate zero for a primitive type token
// (the value coming out of primitiveType). Used by the candidates-block
// renderer to seed the OPTION B `default` stanza's value.
func zeroForType(t string) any {
	switch t {
	case "string":
		return ""
	case "boolean":
		return false
	case "int32", "int64":
		return 0
	case "float", "double":
		return 0.0
	}
	return ""
}
