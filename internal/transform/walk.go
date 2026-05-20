package transform

import "github.com/alternet-dev/wavefront/internal/wireerror"

// failFn produces a transform_failed error in the direction (422 request /
// 502 response) of the verb-applying call site.
type failFn func(reason string) *wireerror.Error

// optionalizedSet tracks active optionalize paths in segment-order.
// coversByName reports whether p is a name-wise extension of (or equal to)
// any registered optionalize stanza.
type optionalizedSet struct{ paths []Path }

func newOptionalizedSet() *optionalizedSet { return &optionalizedSet{} }

func (s *optionalizedSet) add(p Path) { s.paths = append(s.paths, p) }

func (s *optionalizedSet) coversByName(p Path) bool {
	if s == nil {
		return false
	}
	for _, opt := range s.paths {
		if p.HasPrefixByName(opt) {
			return true
		}
	}
	return false
}

// walkLeaves walks obj along p (read-mode), invoking fn for each leaf
// instance discovered (parent map + leaf name). Missing intermediate or
// type-mismatch returns a transform_failed via fail, UNLESS opts covers p
// by name (in which case the affected leaf is silently skipped). Empty
// array at an iterating segment is a silent no-op (zero leaves).
//
// Invariants enforced by ParsePath: len(p) >= 1; the final segment is
// non-array (p[len(p)-1].Array == false).
func walkLeaves(obj map[string]any, p Path, opts *optionalizedSet, fail failFn, fn func(parent map[string]any, leaf string) *wireerror.Error) *wireerror.Error {
	if len(p) == 0 {
		return fail("internal: empty path")
	}
	if len(p) == 1 {
		return fn(obj, p[0].Name)
	}
	seg := p[0]
	rest := p[1:]
	val, present := obj[seg.Name]
	if !present || val == nil {
		if opts.coversByName(p) {
			return nil
		}
		return fail("path " + p.String() + ": segment " + seg.Name + " is absent")
	}
	if seg.Array {
		arr, ok := val.([]any)
		if !ok {
			return fail("path " + p.String() + ": segment " + seg.Name + " is not an array")
		}
		for _, el := range arr {
			sub, ok := el.(map[string]any)
			if !ok {
				return fail("path " + p.String() + ": array element at " + seg.Name + " is not an object")
			}
			if e := walkLeaves(sub, rest, opts, fail, fn); e != nil {
				return e
			}
		}
		return nil
	}
	sub, ok := val.(map[string]any)
	if !ok {
		return fail("path " + p.String() + ": intermediate " + seg.Name + " is not an object")
	}
	return walkLeaves(sub, rest, opts, fail, fn)
}

// walkLeavesEnsureObjects is the write-mode walker used by `default`. It
// auto-creates a missing-or-null object intermediate on the way down. It
// FAILS on:
//   - an existing intermediate that is a non-object (scalar/array when an
//     object is expected) — that's a violation, not something we silently
//     overwrite.
//   - a missing-or-null array-iteration segment — auto-creating an array
//     of unknown length is nonsensical (default populates EXISTING
//     elements; it can't materialize them).
func walkLeavesEnsureObjects(obj map[string]any, p Path, fail failFn, fn func(parent map[string]any, leaf string) *wireerror.Error) *wireerror.Error {
	if len(p) == 0 {
		return fail("internal: empty path")
	}
	if len(p) == 1 {
		return fn(obj, p[0].Name)
	}
	seg := p[0]
	rest := p[1:]
	val, present := obj[seg.Name]
	if seg.Array {
		if !present || val == nil {
			return fail("path " + p.String() + ": array segment " + seg.Name + " is absent and cannot be auto-created")
		}
		arr, ok := val.([]any)
		if !ok {
			return fail("path " + p.String() + ": segment " + seg.Name + " is not an array")
		}
		for _, el := range arr {
			sub, ok := el.(map[string]any)
			if !ok {
				return fail("path " + p.String() + ": array element at " + seg.Name + " is not an object")
			}
			if e := walkLeavesEnsureObjects(sub, rest, fail, fn); e != nil {
				return e
			}
		}
		return nil
	}
	if !present || val == nil {
		sub := map[string]any{}
		obj[seg.Name] = sub
		return walkLeavesEnsureObjects(sub, rest, fail, fn)
	}
	sub, ok := val.(map[string]any)
	if !ok {
		return fail("path " + p.String() + ": intermediate " + seg.Name + " exists as a non-object (cannot auto-create over it)")
	}
	return walkLeavesEnsureObjects(sub, rest, fail, fn)
}
