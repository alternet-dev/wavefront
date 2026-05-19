// Package transform is the v0.2 mechanical transform runtime. It is a pure,
// codec-agnostic unit: a closed set of four hardcoded verbs over the
// top-level fields of a JSON object, applied in listed order, strict. It
// imports only wireerror + encoding/json (never bundle/adapter — the server
// passes ops + bytes), so it has no transport or codec knowledge and a
// future GraphQL/gRPC adapter reuses it untouched.
package transform

import (
	"encoding/json"
	"errors"
	"strconv"

	"github.com/alternet-dev/wavefront/internal/wireerror"
)

// The closed set of mechanical verbs. Op.Kind is always one of these; the
// generator/bundle loader and this interpreter share them as the single
// source of truth (no scattered string literals).
const (
	KindRename      = "rename"
	KindDefault     = "default"
	KindOptionalize = "optionalize"
	KindCoerce      = "coerce"
)

// Op is one parsed, statically-validated transform stanza. Exactly one verb
// is represented by Kind; bundle.Load validates shape before constructing it,
// so the runtime only ever meets data-dependent failures.
type Op struct {
	Kind  string // one of the Kind* constants
	From  string // rename
	To    string // rename
	Field string // default | optionalize | coerce
	// Value is the literal a `default` injects when the field is absent. It
	// is intentionally `any`: a default is an arbitrary JSON scalar
	// (string/number/bool/null) decoded from the bundle YAML and written
	// through verbatim. A narrower Go type would drop numeric/bool defaults;
	// the scalar-only contract is enforced at bundle load, not by this type.
	Value    any
	CoerceTo string // coerce: string | number | bool
}

// ApplyRequest runs request ops (external→internal); failures are 422.
func ApplyRequest(ops []Op, body []byte) ([]byte, *wireerror.Error) {
	return apply(ops, body, true)
}

// ApplyResponse runs response ops (internal→external); failures are 502.
func ApplyResponse(ops []Op, body []byte) ([]byte, *wireerror.Error) {
	return apply(ops, body, false)
}

func apply(ops []Op, body []byte, request bool) ([]byte, *wireerror.Error) {
	fail := func(reason string) *wireerror.Error {
		if request {
			return wireerror.TransformFailedRequest(reason)
		}
		return wireerror.TransformFailedResponse(reason)
	}
	if len(ops) == 0 {
		return body, nil // byte-identical passthrough; no parse
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil || obj == nil {
		return nil, fail("transform target is not a JSON object")
	}
	optional := map[string]bool{}
	for _, op := range ops {
		switch op.Kind {
		case KindOptionalize:
			optional[op.Field] = true
		case KindRename:
			v, ok := obj[op.From]
			if !ok {
				if optional[op.From] {
					continue
				}
				return nil, fail("rename source field " + op.From + " is absent")
			}
			if _, exists := obj[op.To]; exists {
				return nil, fail("rename target field " + op.To + " already present")
			}
			delete(obj, op.From)
			obj[op.To] = v
		case KindDefault:
			if cur, ok := obj[op.Field]; !ok || cur == nil {
				obj[op.Field] = op.Value
			}
		case KindCoerce:
			v, ok := obj[op.Field]
			if !ok {
				if optional[op.Field] {
					continue
				}
				return nil, fail("coerce field " + op.Field + " is absent")
			}
			cv, cerr := coerce(v, op.CoerceTo)
			if cerr != nil {
				return nil, fail("coerce field " + op.Field + ": " + cerr.Error())
			}
			obj[op.Field] = cv
		}
	}
	out, err := json.Marshal(obj)
	if err != nil {
		return nil, fail("could not re-encode transformed body")
	}
	return out, nil
}

// coerce converts a scalar JSON value (encoding/json: float64/string/bool)
// to to ∈ {string, number, bool}. Mechanical only — never date/units/locale.
func coerce(v any, to string) (any, error) {
	switch to {
	case "string":
		switch x := v.(type) {
		case string:
			return x, nil
		case float64:
			return strconv.FormatFloat(x, 'f', -1, 64), nil
		case bool:
			return strconv.FormatBool(x), nil
		}
		return nil, errors.New("value is not a scalar")
	case "number":
		switch x := v.(type) {
		case float64:
			return x, nil
		case string:
			n, err := strconv.ParseFloat(x, 64)
			if err != nil {
				return nil, errors.New("string is not numeric")
			}
			return n, nil
		}
		return nil, errors.New("value cannot become a number")
	case "bool":
		switch x := v.(type) {
		case bool:
			return x, nil
		case string:
			if x == "true" {
				return true, nil
			}
			if x == "false" {
				return false, nil
			}
			return nil, errors.New("string is not true/false")
		}
		return nil, errors.New("value cannot become a bool")
	}
	return nil, errors.New("unknown coerce target " + to)
}
