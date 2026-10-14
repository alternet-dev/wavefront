package bundlegen

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// discriminatorPayload is the shape of an OpenAPI `discriminator` object this
// generator reads: a required propertyName (the sibling JSON property that
// names the active member) plus an explicit mapping from discriminator value
// to component $ref. The implicit name-based mapping is not accepted —
// across per-version proto packages it is ambiguous (#129).
type discriminatorPayload struct {
	PropertyName string            `json:"propertyName"`
	Mapping      map[string]string `json:"mapping"`
}

// typedDiscriminator holds the parsed metadata for a mapped discriminated
// union: the discriminator property name plus the ordered mapping-key list
// (sorted deterministically) and each key's resolved target component.
type typedDiscriminator struct {
	propertyName string
	// entries is ordered by discriminator value (sorted ascending) so the
	// emitted oneof field numbers are deterministic across runs.
	entries []discEntry
}

type discEntry struct {
	value      string // discriminator value as it appears on the wire
	targetName string // component schema name (last segment of $ref)
	fieldName  string // sanitized proto field name inside the oneof
}

// parseDiscriminator returns the mapped-discriminator metadata for sc if sc is
// an anyOf/oneOf carrying a discriminator with an explicit mapping of value →
// $ref. It returns ok=false for every other shape: no discriminator, no
// mapping, an empty mapping, or a mapping value that is not a #/components
// $ref. A discriminator without a mapping stays with the previous Struct
// lowering (#129's note that the implicit name-based mapping is ambiguous
// across per-version proto packages).
func parseDiscriminator(sc *schema) (typedDiscriminator, bool) {
	if sc == nil {
		return typedDiscriminator{}, false
	}
	if len(sc.OneOf) == 0 && len(sc.AnyOf) == 0 {
		return typedDiscriminator{}, false
	}
	if !hasDiscriminator(sc) {
		return typedDiscriminator{}, false
	}
	var p discriminatorPayload
	if err := json.Unmarshal(sc.Discriminator, &p); err != nil {
		return typedDiscriminator{}, false
	}
	if strings.TrimSpace(p.PropertyName) == "" {
		return typedDiscriminator{}, false
	}
	if len(p.Mapping) == 0 {
		return typedDiscriminator{}, false
	}
	values := make([]string, 0, len(p.Mapping))
	for v := range p.Mapping {
		values = append(values, v)
	}
	sort.Strings(values)
	entries := make([]discEntry, 0, len(values))
	used := make(map[string]bool, len(values))
	for _, v := range values {
		ref := p.Mapping[v]
		if !strings.HasPrefix(ref, "#/components/schemas/") {
			return typedDiscriminator{}, false
		}
		entries = append(entries, discEntry{
			value:      v,
			targetName: refName(ref),
			fieldName:  uniqueIdent(v, used),
		})
	}
	return typedDiscriminator{
		propertyName: strings.TrimSpace(p.PropertyName),
		entries:      entries,
	}, true
}

// discriminatorMetadata is the per-wrapper metadata the generator persists into
// discriminators.yaml and the loader exposes to the adapter: for one wrapper
// message (a nested type carrying a single oneof), it records the discriminator
// property name and the value → oneof-field-name mapping. The adapter uses
// this to transform between the flattened OpenAPI wire shape and the wrapped
// protojson wire shape.
type discriminatorMetadata struct {
	Message      string            // fully-qualified wrapper message name
	PropertyName string            // the OpenAPI discriminator property (e.g. "petType")
	Mapping      map[string]string // discriminator value -> proto field name (within the oneof)
}

// renderDiscriminatorsYAML emits the layer's discriminators.yaml body from a
// list of wrapper-message entries, sorted by (Message) for byte-reproducibility.
// Returns ("", nil) when entries is empty — a layer with no mapped
// discriminated unions carries no sidecar at all, so bundles that don't use
// the construct stay byte-identical to pre-#132 output.
func renderDiscriminatorsYAML(entries []discriminatorMetadata) string {
	if len(entries) == 0 {
		return ""
	}
	sorted := make([]discriminatorMetadata, len(entries))
	copy(sorted, entries)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Message < sorted[j].Message })
	var b strings.Builder
	b.WriteString("version: 1\noneofs:\n")
	for _, e := range sorted {
		fmt.Fprintf(&b, "  - message: %s\n", e.Message)
		fmt.Fprintf(&b, "    property_name: %q\n", e.PropertyName)
		b.WriteString("    mapping:\n")
		keys := make([]string, 0, len(e.Mapping))
		for k := range e.Mapping {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Fprintf(&b, "      %q: %s\n", k, e.Mapping[k])
		}
	}
	return b.String()
}
