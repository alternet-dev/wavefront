package bundlegen

import (
	"encoding/json"
	"fmt"
	"sort"

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

// StanzaProposal is one drafted transform stanza, before any YAML
// rendering. Verb is the canonical transform.Kind that selects which
// StanzaArgs implementer is held in Args; Signals records the structural
// observations that produced the proposal so the renderer can surface
// *why* the drafter emitted it.
type StanzaProposal struct {
	Verb       transform.Kind
	Args       StanzaArgs
	Confidence Confidence
	Signals    []string
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
// that bridges them. It detects three confident change classes at the
// request and response message's top-level fields:
//
//   - a field in the "to" shape that the "from" shape lacks (a pure
//     addition) becomes a `default` stanza filling the new field with a
//     type-appropriate placeholder
//   - a field present in both but with a changed primitive type becomes
//     a `coerce` stanza
//   - a field in the "from" shape that the "to" shape lacks (a pure
//     removal) is surfaced as a note rather than a stanza, because the
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
func draftDirection(from, to *schema, side string) ([]StanzaProposal, []string) {
	fromFields := topLevelProperties(from)
	toFields := topLevelProperties(to)

	fromNames := sortedKeys(fromFields)
	toNames := sortedKeys(toFields)
	inTo := makeSet(toNames)
	inFrom := makeSet(fromNames)

	var props []StanzaProposal
	var notes []string

	// Fields added (in `to`, not in `from`) — fill via `default`.
	for _, name := range toNames {
		if inFrom[name] {
			continue
		}
		props = append(props, StanzaProposal{
			Verb:       transform.KindDefault,
			Args:       DefaultArgs{Field: name, Value: zeroForSchema(toFields[name])},
			Confidence: Confident,
			Signals:    []string{"pure-add"},
		})
	}

	// Type changes for fields present in both.
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
			Signals:    []string{"type-change", "from:" + fromT, "to:" + toT},
		})
	}

	// Fields removed (in `from`, not in `to`) — the verb set has no way
	// to drop a known field on this side; surface as a note so the human
	// can decide.
	for _, name := range fromNames {
		if inTo[name] {
			continue
		}
		notes = append(notes, fmt.Sprintf(
			"%s field %q is present in the from shape but absent from the to shape; the transform verb set has no way to drop a known field, so the operator must reconcile this manually",
			side, name))
	}

	return props, notes
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
