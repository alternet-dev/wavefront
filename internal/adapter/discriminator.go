package adapter

import (
	"encoding/json"
	"fmt"

	"google.golang.org/protobuf/reflect/protoreflect"
)

// DiscriminatorResolver is the optional capability the adapter type-asserts the
// resolver against. For a mapped-discriminator wrapper message it returns the
// discriminator property name (the OpenAPI "propertyName" — the JSON key that
// names the active member on the flattened wire) and the on-wire value →
// oneof-field-name mapping (#132). It is defined with scalar returns rather
// than a struct type so bundle.Bundle satisfies it structurally, the same
// convention bundle.Contract uses for Binding.
type DiscriminatorResolver interface {
	Discriminator(messageFullName string) (propertyName string, mapping map[string]string, ok bool)
}

// discInfo is the adapter's internal view of what DiscriminatorResolver
// returns. Keeping it local avoids coupling the adapter package to the
// bundle's public type and keeps the walker's call sites tidy.
type discInfo struct {
	propertyName string
	mapping      map[string]string
}

// transformWrappedToFlat rewrites protojson's output JSON for md so each mapped
// discriminator wrapper carries the flattened OpenAPI wire shape (discriminator
// property + member fields at the top level). It is a no-op when resolver does
// not implement DiscriminatorResolver, so adapters without typed-union support
// stay byte-identical.
func transformWrappedToFlat(md protoreflect.MessageDescriptor, resolver MessageResolver, in []byte) ([]byte, error) {
	disc, ok := resolver.(DiscriminatorResolver)
	if !ok {
		return in, nil
	}
	return transformJSON(md, disc, in, dirToFlat)
}

// transformFlatToWrapped rewrites an upstream JSON body for md so each mapped
// discriminator position is wrapped under the matched oneof member key, which
// is what protojson will accept against the generated schema. No-op when
// resolver does not implement DiscriminatorResolver.
func transformFlatToWrapped(md protoreflect.MessageDescriptor, resolver MessageResolver, in []byte) ([]byte, error) {
	disc, ok := resolver.(DiscriminatorResolver)
	if !ok {
		return in, nil
	}
	return transformJSON(md, disc, in, dirToWrapped)
}

type transformDir int

const (
	dirToFlat    transformDir = iota // wrapped -> flat (proto -> upstream)
	dirToWrapped                     // flat -> wrapped (upstream -> proto)
)

// transformJSON is the shared JSON walk: decode in, visit it against md,
// apply the discriminator transform at each wrapper position, re-encode.
// An input that is not a JSON object at the root (null, scalar, array) is
// returned verbatim — a message body in this adapter is always a JSON object.
func transformJSON(md protoreflect.MessageDescriptor, resolver DiscriminatorResolver, in []byte, dir transformDir) ([]byte, error) {
	if len(in) == 0 {
		return in, nil
	}
	var v any
	if err := json.Unmarshal(in, &v); err != nil {
		return nil, err
	}
	obj, ok := v.(map[string]any)
	if !ok {
		return in, nil
	}
	out, err := walkMessage(md, resolver, obj, dir)
	if err != nil {
		return nil, err
	}
	return json.Marshal(out)
}

// walkMessage visits one JSON object positioned at md. If md is itself a
// mapped-discriminator wrapper, the object is flattened or wrapped per dir.
// Otherwise each submessage field is recursed into (singular, repeated, or
// map<string, msg>) so nested wrappers deeper in the tree are handled too.
func walkMessage(md protoreflect.MessageDescriptor, resolver DiscriminatorResolver, obj map[string]any, dir transformDir) (map[string]any, error) {
	if propName, mapping, ok := resolver.Discriminator(string(md.FullName())); ok {
		info := discInfo{propertyName: propName, mapping: mapping}
		switch dir {
		case dirToFlat:
			return flattenWrapper(md, resolver, obj, info)
		case dirToWrapped:
			return wrapFlat(md, resolver, obj, info)
		}
	}
	// Non-wrapper message: recurse into message-typed fields so nested
	// discriminator wrappers get transformed too.
	out := make(map[string]any, len(obj))
	for k, v := range obj {
		field := lookupField(md, k)
		if field == nil || field.Message() == nil {
			out[k] = v
			continue
		}
		transformed, err := walkFieldValue(field, resolver, v, dir)
		if err != nil {
			return nil, err
		}
		out[k] = transformed
	}
	return out, nil
}

// walkFieldValue applies the transform to one field's JSON value, handling
// singular / repeated / map<string, msg>. The field is known to have a message
// element type.
func walkFieldValue(field protoreflect.FieldDescriptor, resolver DiscriminatorResolver, v any, dir transformDir) (any, error) {
	msgDesc := field.Message()
	switch {
	case field.IsMap():
		// map<string, V>: a map-entry message whose "value" field carries V.
		// Only the message-valued form is interesting here; proto3 forbids a
		// repeated map value, so each entry's value is at most one message.
		valField := field.MapValue()
		if valField == nil || valField.Message() == nil {
			return v, nil
		}
		m, ok := v.(map[string]any)
		if !ok {
			return v, nil
		}
		out := make(map[string]any, len(m))
		for k, item := range m {
			subObj, ok := item.(map[string]any)
			if !ok {
				out[k] = item
				continue
			}
			sub, err := walkMessage(valField.Message(), resolver, subObj, dir)
			if err != nil {
				return nil, err
			}
			out[k] = sub
		}
		return out, nil
	case field.IsList():
		arr, ok := v.([]any)
		if !ok {
			return v, nil
		}
		out := make([]any, 0, len(arr))
		for _, item := range arr {
			subObj, ok := item.(map[string]any)
			if !ok {
				out = append(out, item)
				continue
			}
			sub, err := walkMessage(msgDesc, resolver, subObj, dir)
			if err != nil {
				return nil, err
			}
			out = append(out, sub)
		}
		return out, nil
	default:
		subObj, ok := v.(map[string]any)
		if !ok {
			return v, nil
		}
		return walkMessage(msgDesc, resolver, subObj, dir)
	}
}

// flattenWrapper lowers one wrapper object from the protojson wrapped shape
// ({"<member_field>": {...}}) to the flattened OpenAPI shape (member fields
// at the top level, discriminator property restored). An unset oneof is
// forwarded verbatim as the empty JSON object — the upstream can decide.
func flattenWrapper(md protoreflect.MessageDescriptor, resolver DiscriminatorResolver, obj map[string]any, info discInfo) (map[string]any, error) {
	if len(obj) == 0 {
		return obj, nil
	}
	// field-name -> discriminator value, built once from info.mapping.
	fieldToValue := make(map[string]string, len(info.mapping))
	for value, fieldName := range info.mapping {
		fieldToValue[fieldName] = value
	}
	// Exactly one key is expected — the active member field. More than one
	// means protojson saw a set-twice oneof, which cannot happen here.
	var activeField string
	var activeValue any
	for k, v := range obj {
		activeField = k
		activeValue = v
	}
	discValue, ok := fieldToValue[activeField]
	if !ok {
		return nil, fmt.Errorf("discriminator wrapper %s: unexpected field %q (not a mapped member)", md.FullName(), activeField)
	}
	member, ok := activeValue.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("discriminator wrapper %s: member %q value is not an object", md.FullName(), activeField)
	}
	// Recurse into the member's own fields so nested discriminators within the
	// variant message are flattened too.
	memberDesc := memberFieldDescriptor(md, activeField)
	if memberDesc != nil && memberDesc.Message() != nil {
		recursed, err := walkMessage(memberDesc.Message(), resolver, member, dirToFlat)
		if err != nil {
			return nil, err
		}
		member = recursed
	}
	out := make(map[string]any, len(member)+1)
	for k, v := range member {
		out[k] = v
	}
	// Restore the discriminator property. The member's own schema includes it
	// (FastAPI emits it on every variant), but the user may have omitted it in
	// a hand-authored message; writing it unconditionally keeps the upstream
	// contract honoured.
	out[info.propertyName] = discValue
	return out, nil
}

// wrapFlat rewrites one flattened upstream object ({"petType": "cat", ...})
// into protojson's wrapped shape ({"cat": {...}}), looking up the active
// member via info.mapping. An object with no discriminator property is a
// contract violation and is reported.
func wrapFlat(md protoreflect.MessageDescriptor, resolver DiscriminatorResolver, obj map[string]any, info discInfo) (map[string]any, error) {
	rawValue, ok := obj[info.propertyName]
	if !ok {
		return nil, fmt.Errorf("discriminator wrapper %s: missing discriminator property %q on upstream object", md.FullName(), info.propertyName)
	}
	discValue, ok := rawValue.(string)
	if !ok {
		return nil, fmt.Errorf("discriminator wrapper %s: discriminator property %q is not a string", md.FullName(), info.propertyName)
	}
	fieldName, ok := info.mapping[discValue]
	if !ok {
		return nil, fmt.Errorf("discriminator wrapper %s: discriminator value %q is not in the mapping", md.FullName(), discValue)
	}
	// Build the member object (everything from obj except — optionally —
	// the discriminator; preserve it so the member's own schema stays filled).
	member := make(map[string]any, len(obj))
	for k, v := range obj {
		member[k] = v
	}
	// Recurse into the member's descriptor so nested discriminators inside
	// the variant message are wrapped too.
	memberDesc := memberFieldDescriptor(md, fieldName)
	if memberDesc != nil && memberDesc.Message() != nil {
		recursed, err := walkMessage(memberDesc.Message(), resolver, member, dirToWrapped)
		if err != nil {
			return nil, err
		}
		member = recursed
	}
	return map[string]any{fieldName: member}, nil
}

// lookupField resolves one JSON key to its FieldDescriptor against md. Both
// the JSON name (what protojson writes) and the proto field name are checked —
// the generator always sets json_name to the original OpenAPI property, so
// upstream JSON keys hit by json_name while proto field names are the fallback.
func lookupField(md protoreflect.MessageDescriptor, key string) protoreflect.FieldDescriptor {
	if f := md.Fields().ByJSONName(key); f != nil {
		return f
	}
	return md.Fields().ByName(protoreflect.Name(key))
}

// memberFieldDescriptor returns the descriptor for the oneof member named by
// fieldName within a wrapper message. The wrapper's fields are the oneof's
// members by construction.
func memberFieldDescriptor(md protoreflect.MessageDescriptor, fieldName string) protoreflect.FieldDescriptor {
	return md.Fields().ByName(protoreflect.Name(fieldName))
}
