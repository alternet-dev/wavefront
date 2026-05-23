package bundlegen

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

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

// ShimProposal is the draft of a single resolution.yaml override entry:
// request and response stanza lists plus any notes the drafter could not
// express mechanically.
type ShimProposal struct {
	FromVersion string
	ToVersion   string
	Request     []StanzaProposal
	Response    []StanzaProposal
	Notes       []string
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
//   - a removed/added pair whose names case-normalize to the same string
//     and share a primitive type becomes a confident `rename` stanza
//   - a field in the "from" shape that has no peer left after pair
//     matching is surfaced as a note rather than a stanza, because the
//     transform verb set has no way to drop a known field — the operator
//     must reconcile the gap by hand
//
// Top-level object properties only; nested objects, arrays, and
// oneOf/allOf are not yet supported.
func DraftShim(fromOpenAPI, toOpenAPI []byte) (*ShimProposal, error) {
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
	reqProps, reqNotes := draftDirection(fromReq, toReq, "request")
	p.Request = reqProps
	p.Notes = append(p.Notes, reqNotes...)

	// Response direction: backend returns `toResp`, old client expects
	// `fromResp`. The transform reshapes from `toResp` to `fromResp`.
	respProps, respNotes := draftDirection(toResp, fromResp, "response")
	p.Response = respProps
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
// changes the transform verbs cover; everything it cannot mechanically
// express is appended to notes for human review.
//
// Confident pair-matching: a removed field and an added field whose names
// case-normalize to the same string and share a primitive type are paired
// into a `rename` stanza instead of a separate add and remove.
// Lower-confidence pairings (different normalized names, non-primitive
// types) are deliberately left unmatched here — those need human
// disambiguation rather than a guess.
func draftDirection(from, to *schema, side string) ([]StanzaProposal, []string) {
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

	renames, unmatchedRemoved, unmatchedAdded := matchCaseRenames(removed, added, fromFields, toFields)

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

	return props, notes
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
