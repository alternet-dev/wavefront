package transform

import (
	"errors"
	"fmt"
	"strings"
)

// Segment is one component of a Path. Name is the object key; when Array is
// true the value at Name must be a []any and the path iterates over its
// elements (subsequent segments apply to each element).
type Segment struct {
	Name  string
	Array bool
}

// Path is a parsed reference into a JSON value tree, used by every verb's
// addressing. A bare-name slice-1 path parses to a one-segment Path with
// Array=false; the parser rejects any other shape that lacks a Name leaf.
type Path []Segment

// ParsePath parses a path string. Grammar:
//
//	path    = segment ( "." segment )*
//	segment = name | name "[]"
//	name    = one or more UTF-8 bytes excluding ".", "[", and "]"
//
// The LAST segment must NOT be an array segment — paths must end on a Name
// leaf (otherwise no verb has a leaf to act on). Returns an error for any
// grammar violation; bundle.toOps surfaces these as ValidationError.
func ParsePath(s string) (Path, error) {
	if s == "" {
		return nil, errors.New("empty path")
	}
	if strings.HasPrefix(s, ".") {
		return nil, fmt.Errorf("leading dot in path %q", s)
	}
	if strings.HasSuffix(s, ".") {
		return nil, fmt.Errorf("trailing dot in path %q", s)
	}
	segs := strings.Split(s, ".")
	out := make(Path, 0, len(segs))
	for i, seg := range segs {
		if seg == "" {
			return nil, fmt.Errorf("empty segment in path %q", s)
		}
		array := strings.HasSuffix(seg, "[]")
		if array {
			seg = seg[:len(seg)-2]
		}
		if seg == "" {
			return nil, fmt.Errorf("empty name before [] in path %q", s)
		}
		if strings.ContainsAny(seg, ".[]") {
			return nil, fmt.Errorf("invalid character in segment %q of path %q", seg, s)
		}
		if array && i == len(segs)-1 {
			return nil, fmt.Errorf("path %q ends with [] (must end with a Name leaf)", s)
		}
		out = append(out, Segment{Name: seg, Array: array})
	}
	return out, nil
}

// String returns the canonical text form of p (the inverse of ParsePath).
func (p Path) String() string {
	parts := make([]string, len(p))
	for i, s := range p {
		if s.Array {
			parts[i] = s.Name + "[]"
		} else {
			parts[i] = s.Name
		}
	}
	return strings.Join(parts, ".")
}

// Equal reports whether p and q are the same path (both Name and Array
// equal at every segment).
func (p Path) Equal(q Path) bool {
	if len(p) != len(q) {
		return false
	}
	for i := range p {
		if p[i] != q[i] {
			return false
		}
	}
	return true
}

// HasPrefixByName reports whether p starts with prefix's segments compared
// by Name only (Array flag ignored). Used by optionalize's prefix coverage:
// "optionalize {data}" covers "data.x" AND "data[].y" — the "absent"
// thing's children are absent regardless of how the missing thing would
// have been read (object vs array).
func (p Path) HasPrefixByName(prefix Path) bool {
	if len(prefix) > len(p) {
		return false
	}
	for i := range prefix {
		if prefix[i].Name != p[i].Name {
			return false
		}
	}
	return true
}
