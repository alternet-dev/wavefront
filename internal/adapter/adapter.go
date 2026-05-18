// Package adapter is the codec boundary. It is interaction-model-agnostic: the
// core passes a Binding (never positional proto names) and the adapter never
// sees the inbound transport — DecodeRequest's product is the upstream intent.
// A future GraphQL adapter (single endpoint, request-defined response shape)
// implements the same interface without touching server/negotiate. v0.1 ships
// only the protobuf↔JSON adapter.
package adapter

import (
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"

	"github.com/alternet-dev/wavefront/internal/wireerror"
)

// Binding is what the adapter needs to express upstream intent. bundle.Contract
// satisfies it structurally (no import cycle).
type Binding interface {
	Method() string
	Route() string
	RequestMessage() string
	ResponseMessage() string
}

// UpstreamCall is the adapter's product: the intent of one upstream call. The
// adapter sets Method/Path/Body/ContentType from the binding + codec; it does
// not set RawQuery — the verbatim inbound query is transport the server owns.
type UpstreamCall struct {
	Method      string
	Path        string
	RawQuery    string
	Body        []byte
	ContentType string
}

// Adapter converts between an external codec and the internal JSON backend.
type Adapter interface {
	DecodeRequest(b Binding, in []byte) (UpstreamCall, *wireerror.Error)
	EncodeResponse(b Binding, upstreamJSON []byte) (out []byte, contentType string, err *wireerror.Error)
}

// MessageResolver resolves a fully-qualified proto name to its descriptor.
// bundle.Bundle satisfies it structurally.
type MessageResolver interface {
	Message(fullName string) (protoreflect.MessageDescriptor, error)
}

// ProtoJSON is the reference adapter: protobuf in, JSON to the upstream, and
// back. It owns no transport and no routing policy.
type ProtoJSON struct {
	resolver MessageResolver
}

func NewProtoJSON(r MessageResolver) *ProtoJSON { return &ProtoJSON{resolver: r} }

func (p *ProtoJSON) DecodeRequest(b Binding, in []byte) (UpstreamCall, *wireerror.Error) {
	md, err := p.resolver.Message(b.RequestMessage())
	if err != nil {
		return UpstreamCall{}, wireerror.DecodeFailed("request_message " + b.RequestMessage() + " is not resolvable")
	}
	msg := dynamicpb.NewMessage(md)
	if err := proto.Unmarshal(in, msg); err != nil {
		return UpstreamCall{}, wireerror.DecodeFailed("request body is not valid protobuf for " + b.RequestMessage())
	}
	j, err := protojson.Marshal(msg)
	if err != nil {
		return UpstreamCall{}, wireerror.DecodeFailed("could not re-encode request as JSON")
	}
	return UpstreamCall{
		Method:      b.Method(),
		Path:        b.Route(),
		Body:        j,
		ContentType: "application/json",
	}, nil
}

func (p *ProtoJSON) EncodeResponse(b Binding, upstreamJSON []byte) ([]byte, string, *wireerror.Error) {
	md, err := p.resolver.Message(b.ResponseMessage())
	if err != nil {
		return nil, "", wireerror.UpstreamError("response_message " + b.ResponseMessage() + " is not resolvable")
	}
	msg := dynamicpb.NewMessage(md)
	if err := protojson.Unmarshal(upstreamJSON, msg); err != nil {
		return nil, "", wireerror.UpstreamError("upstream response did not match " + b.ResponseMessage())
	}
	out, err := proto.Marshal(msg)
	if err != nil {
		return nil, "", wireerror.UpstreamError("could not encode upstream response")
	}
	return out, "application/protobuf", nil
}
