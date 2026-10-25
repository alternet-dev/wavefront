package bundlegen

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/alternet-dev/wavefront/internal/transform"
	"gopkg.in/yaml.v3"
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
	SignalKindSimilarity SignalKind = "name-similarity"
	SignalKindIntent     SignalKind = "operator-intent"
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

// NameSimilaritySignal records a non-exact normalized-name match that met the
// caller's confidence threshold. The value is rounded only for presentation;
// matching itself retains the full score.
func NameSimilaritySignal(score float64) Signal {
	return Signal{Kind: SignalKindSimilarity, Value: fmt.Sprintf("%.3f", score)}
}

// OperatorIntentSignal records that an explicit operator-authored mapping,
// rather than a heuristic, selected a rename.
func OperatorIntentSignal() Signal { return Signal{Kind: SignalKindIntent} }

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

// RenameIntent is one operator-authored old-to-new top-level field mapping.
// A mapping applies to any request or response direction in the compared
// OpenAPI pair where both fields exist and have the same primitive type.
type RenameIntent struct {
	From string `yaml:"from"`
	To   string `yaml:"to"`
}

// DraftOptions controls heuristic rename detection. RenameConfidence is a
// normalized edit-similarity threshold from 0 to 1; 1 preserves the historic
// exact case/separator-normalized behavior. Strict suppresses heuristics but
// still permits explicit RenameIntents.
type DraftOptions struct {
	RenameConfidence float64
	Strict           bool
	RenameIntents    []RenameIntent
}

// LoadRenameIntents decodes a versioned operator intent file. Intent files are
// deliberately invocation input, not bundle state: they apply only to the
// OpenAPI pair the caller supplies to DraftShimWithOptions.
func LoadRenameIntents(data []byte) ([]RenameIntent, error) {
	var file struct {
		Version int            `yaml:"version"`
		Renames []RenameIntent `yaml:"renames"`
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&file); err != nil {
		return nil, fmt.Errorf("parse rename intent: %w", err)
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, fmt.Errorf("parse rename intent: multiple YAML documents are not supported")
		}
		return nil, fmt.Errorf("parse rename intent: %w", err)
	}
	if file.Version != 1 {
		return nil, fmt.Errorf("rename intent has unsupported version %d (want 1)", file.Version)
	}
	if len(file.Renames) == 0 {
		return nil, fmt.Errorf("rename intent has no renames")
	}
	froms := make(map[string]bool, len(file.Renames))
	tos := make(map[string]bool, len(file.Renames))
	for _, rename := range file.Renames {
		if rename.From == "" || rename.To == "" {
			return nil, fmt.Errorf("rename intent entries require non-empty from and to")
		}
		if froms[rename.From] || tos[rename.To] {
			return nil, fmt.Errorf("rename intent entries must map each from and to field at most once")
		}
		froms[rename.From] = true
		tos[rename.To] = true
	}
	return file.Renames, nil
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
	return DraftShimWithOptions(fromOpenAPI, toOpenAPI, DraftOptions{RenameConfidence: 1})
}

// DraftShimStrict is the operator escape hatch: it suppresses every heuristic
// rename and reports each non-intent removed/added pair as a candidate instead.
// Use when the drafter's confidence is itself the thing in question — for
// example when reviewing a high-stakes contract drift and an auto-rename would
// short-circuit a decision the operator wants to make.
func DraftShimStrict(fromOpenAPI, toOpenAPI []byte) (*ShimProposal, error) {
	return DraftShimWithOptions(fromOpenAPI, toOpenAPI, DraftOptions{RenameConfidence: 1, Strict: true})
}

// DraftShimWithOptions compares two OpenAPI documents and applies the supplied
// heuristic and operator-intent controls. Explicit intents always take
// precedence over heuristic matching.
func DraftShimWithOptions(fromOpenAPI, toOpenAPI []byte, opts DraftOptions) (*ShimProposal, error) {
	if math.IsNaN(opts.RenameConfidence) || math.IsInf(opts.RenameConfidence, 0) || opts.RenameConfidence < 0 || opts.RenameConfidence > 1 {
		return nil, fmt.Errorf("rename confidence must be between 0 and 1")
	}
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
	if err := validateRenameIntents(fromReq, toReq, fromResp, toResp, opts.RenameIntents); err != nil {
		return nil, err
	}

	p := &ShimProposal{
		FromVersion: fromDoc.Info.Version,
		ToVersion:   toDoc.Info.Version,
	}

	// Request direction: old client speaks `fromReq`, backend expects `toReq`.
	// The transform reshapes the body from the "from" side to the "to" side.
	reqProps, reqCands, reqNotes, err := draftDirection(fromReq, toReq, "request", opts)
	if err != nil {
		return nil, err
	}
	p.Request = reqProps
	p.RequestCandidates = reqCands
	p.Notes = append(p.Notes, reqNotes...)

	// Response direction: backend returns `toResp`, old client expects
	// `fromResp`. The transform reshapes from `toResp` to `fromResp`.
	respOpts := opts
	respOpts.RenameIntents = invertRenameIntents(opts.RenameIntents)
	respProps, respCands, respNotes, err := draftDirection(toResp, fromResp, "response", respOpts)
	if err != nil {
		return nil, err
	}
	p.Response = respProps
	p.ResponseCandidates = respCands
	p.Notes = append(p.Notes, respNotes...)

	return p, nil
}

// validateRenameIntents checks the old-to-new mapping in its natural request
// direction and its reversed response direction. An entry may apply to one
// direction only, but it may not silently name a field on only one side of a
// direction or bridge different primitive types.
func validateRenameIntents(fromReq, toReq, fromResp, toResp *schema, intents []RenameIntent) error {
	for _, intent := range intents {
		requestApplies, err := validateRenameIntentDirection("request", intent.From, intent.To, topLevelProperties(fromReq), topLevelProperties(toReq))
		if err != nil {
			return err
		}
		responseApplies, err := validateRenameIntentDirection("response", intent.To, intent.From, topLevelProperties(toResp), topLevelProperties(fromResp))
		if err != nil {
			return err
		}
		if !requestApplies && !responseApplies {
			return fmt.Errorf("rename intent %q -> %q does not match a request or response direction", intent.From, intent.To)
		}
	}
	return nil
}

func validateRenameIntentDirection(side, fromName, toName string, fromFields, toFields map[string]*schema) (bool, error) {
	fromField, hasFrom := fromFields[fromName]
	toField, hasTo := toFields[toName]
	if !hasFrom && !hasTo {
		return false, nil
	}
	if !hasFrom || !hasTo {
		return false, fmt.Errorf("rename intent %q -> %q names a %s field on only one side", fromName, toName, side)
	}
	fromType, toType := primitiveType(fromField), primitiveType(toField)
	if fromType == "" || toType == "" || fromType != toType {
		return false, fmt.Errorf("rename intent %q -> %q must map same primitive types on the %s", fromName, toName, side)
	}
	return true, nil
}

func invertRenameIntents(intents []RenameIntent) []RenameIntent {
	inverted := make([]RenameIntent, len(intents))
	for i, intent := range intents {
		inverted[i] = RenameIntent{From: intent.To, To: intent.From}
	}
	return inverted
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
// an OpenAPI document with exactly one operation. draft-shim diffs a
// single from/to pair; multi-route inputs would make the diff ambiguous
// (which route's request schema is being compared?). This is a draft-shim
// constraint, not a general bundlegen one.
func operationSchemas(doc openAPI) (req, resp *schema, err error) {
	ops, err := allOperations(doc)
	if err != nil {
		return nil, nil, err
	}
	if len(ops) != 1 {
		return nil, nil, fmt.Errorf("draft-shim requires an OpenAPI document with exactly one operation, found %d", len(ops))
	}
	op := ops[0].op
	reqName, err := refSchemaName(op, true)
	if err != nil {
		return nil, nil, fmt.Errorf("request: %w", err)
	}
	respName, err := refSchemaName(op, false)
	if err != nil {
		return nil, nil, fmt.Errorf("response: %w", err)
	}
	// The synthetic Empty message has no OpenAPI component to walk; a
	// bodyless side diffs as an object with no properties.
	realRoots, _ := partitionEmpty([]string{reqName, respName})
	schemas, err := collectSchemas(doc, realRoots)
	if err != nil {
		return nil, nil, err
	}
	req, err = resolveOperationSchema(schemas, reqName)
	if err != nil {
		return nil, nil, fmt.Errorf("request %w", err)
	}
	resp, err = resolveOperationSchema(schemas, respName)
	if err != nil {
		return nil, nil, fmt.Errorf("response %w", err)
	}
	return req, resp, nil
}

// resolveOperationSchema returns the collected schema for name, or a fresh
// empty object for the synthetic Empty message — which collectSchemas does
// not produce because it has no OpenAPI component to walk from.
func resolveOperationSchema(schemas map[string]*schema, name string) (*schema, error) {
	if name == emptyMessageName {
		return &schema{Type: "object", Properties: map[string]*schema{}}, nil
	}
	s, ok := schemas[name]
	if !ok || s == nil {
		return nil, fmt.Errorf("schema %q not found", name)
	}
	return s, nil
}

// draftDirection diffs one direction of a transform: a `from` shape that
// arrives and a `to` shape that must leave. It emits stanzas for the
// changes the transform verbs cover, candidate pairs for the ambiguous
// ones, and notes for everything it cannot mechanically express.
//
// Strict mode suppresses heuristic matching, while explicit operator intent
// still selects its declared pairs.
func draftDirection(from, to *schema, side string, opts DraftOptions) ([]StanzaProposal, []CandidatePair, []string, error) {
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

	renames, unmatchedRemoved, unmatchedAdded, err := matchIntentRenames(removed, added, fromFields, toFields, opts.RenameIntents)
	if err != nil {
		return nil, nil, nil, err
	}
	if !opts.Strict {
		var heuristicRenames []renamePair
		heuristicRenames, unmatchedRemoved, unmatchedAdded = matchHeuristicRenames(unmatchedRemoved, unmatchedAdded, fromFields, toFields, opts.RenameConfidence)
		renames = append(renames, heuristicRenames...)
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
			Signals:    r.signals,
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

	return props, candidates, notes, nil
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

// renamePair is a single confident rename and the evidence that selected it.
type renamePair struct {
	from, to string
	signals  []Signal
}

// matchIntentRenames honors explicit, validated mappings before any heuristic
// can consume their fields. An intent must correspond to a removed/added pair
// on the direction where it applies; otherwise it would generate a transform
// that does not describe this diff.
func matchIntentRenames(removed, added []string, fromSchemas, toSchemas map[string]*schema, intents []RenameIntent) ([]renamePair, []string, []string, error) {
	removedSet := makeSet(removed)
	addedSet := makeSet(added)
	usedRemoved := make(map[string]bool)
	usedAdded := make(map[string]bool)
	var pairs []renamePair
	for _, intent := range intents {
		_, hasFrom := fromSchemas[intent.From]
		_, hasTo := toSchemas[intent.To]
		if !hasFrom && !hasTo {
			continue
		}
		if !hasFrom || !hasTo {
			return nil, nil, nil, fmt.Errorf("rename intent %q -> %q does not describe a removed/added pair", intent.From, intent.To)
		}
		if !removedSet[intent.From] && !addedSet[intent.To] {
			// This direction contains both names but has no rename-shaped
			// change. The same intent can still apply to the other direction.
			continue
		}
		if !removedSet[intent.From] || !addedSet[intent.To] {
			return nil, nil, nil, fmt.Errorf("rename intent %q -> %q does not describe a removed/added pair", intent.From, intent.To)
		}
		if usedRemoved[intent.From] || usedAdded[intent.To] {
			return nil, nil, nil, fmt.Errorf("rename intent %q -> %q conflicts with another mapping", intent.From, intent.To)
		}
		pairs = append(pairs, renamePair{
			from: intent.From,
			to:   intent.To,
			signals: []Signal{
				OperatorIntentSignal(),
				SameTypeSignal(primitiveType(fromSchemas[intent.From])),
			},
		})
		usedRemoved[intent.From] = true
		usedAdded[intent.To] = true
	}
	var unmatchedRemoved, unmatchedAdded []string
	for _, name := range removed {
		if !usedRemoved[name] {
			unmatchedRemoved = append(unmatchedRemoved, name)
		}
	}
	for _, name := range added {
		if !usedAdded[name] {
			unmatchedAdded = append(unmatchedAdded, name)
		}
	}
	return pairs, unmatchedRemoved, unmatchedAdded, nil
}

// matchHeuristicRenames pairs same-type fields whose normalized edit
// similarity reaches threshold. At the default threshold of 1, this is the
// historic case- and separator-normalized exact match. Each source field
// chooses its highest-scoring still-available target, with source order and
// target order breaking ties deterministically.
func matchHeuristicRenames(removed, added []string, fromSchemas, toSchemas map[string]*schema, threshold float64) ([]renamePair, []string, []string) {
	usedAdded := make(map[string]bool)
	var pairs []renamePair
	var unmatchedRemoved []string

	for _, r := range removed {
		fromT := primitiveType(fromSchemas[r])
		if fromT == "" {
			unmatchedRemoved = append(unmatchedRemoved, r)
			continue
		}
		matched := ""
		bestScore := -1.0
		for _, a := range added {
			if usedAdded[a] {
				continue
			}
			toT := primitiveType(toSchemas[a])
			if toT == "" || toT != fromT {
				continue
			}
			score := nameSimilarity(r, a)
			if score >= threshold && score > bestScore {
				matched = a
				bestScore = score
			}
		}
		if matched == "" {
			unmatchedRemoved = append(unmatchedRemoved, r)
			continue
		}
		signals := []Signal{SameTypeSignal(fromT)}
		if normalizeFieldName(r) == normalizeFieldName(matched) {
			signals = append([]Signal{CaseRenameSignal()}, signals...)
		} else {
			signals = append([]Signal{NameSimilaritySignal(bestScore)}, signals...)
		}
		pairs = append(pairs, renamePair{from: r, to: matched, signals: signals})
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

// nameSimilarity returns the normalized Levenshtein similarity of two field
// names after case and separator normalization. It returns 1 for equal names
// and 0 for maximally dissimilar names of the same maximum length.
func nameSimilarity(a, b string) float64 {
	aRunes := []rune(normalizeFieldName(a))
	bRunes := []rune(normalizeFieldName(b))
	maxLen := len(aRunes)
	if len(bRunes) > maxLen {
		maxLen = len(bRunes)
	}
	if maxLen == 0 {
		return 1
	}
	previous := make([]int, len(bRunes)+1)
	for j := range previous {
		previous[j] = j
	}
	for i, ar := range aRunes {
		current := make([]int, len(bRunes)+1)
		current[0] = i + 1
		for j, br := range bRunes {
			cost := 0
			if ar != br {
				cost = 1
			}
			current[j+1] = minInt(previous[j+1]+1, current[j]+1, previous[j]+cost)
		}
		previous = current
	}
	return float64(maxLen-previous[len(bRunes)]) / float64(maxLen)
}

func minInt(values ...int) int {
	smallest := values[0]
	for _, value := range values[1:] {
		if value < smallest {
			smallest = value
		}
	}
	return smallest
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
