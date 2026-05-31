package wireerror

import (
	"strings"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"
)

// errorMD is the descriptor for the fixed, version-independent
// `wavefront.v0.Error { string code = 1; string message = 2; }`. It is built
// in-code (no .proto, no protoc, no codegen) for consistency with the bundle
// generator's in-code descriptor construction, and because the type never
// varies — it must encode even when contract negotiation failed.
var errorMD protoreflect.MessageDescriptor

func init() {
	fdp := &descriptorpb.FileDescriptorProto{
		Name:    proto.String("wavefront/v0/error.proto"),
		Package: proto.String("wavefront.v0"),
		Syntax:  proto.String("proto3"),
		MessageType: []*descriptorpb.DescriptorProto{{
			Name: proto.String("Error"),
			Field: []*descriptorpb.FieldDescriptorProto{
				{
					Name:     proto.String("code"),
					Number:   proto.Int32(1),
					Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
					Type:     descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(),
					JsonName: proto.String("code"),
				},
				{
					Name:     proto.String("message"),
					Number:   proto.Int32(2),
					Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
					Type:     descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(),
					JsonName: proto.String("message"),
				},
			},
		}},
	}
	fd, err := protodesc.NewFile(fdp, nil)
	if err != nil {
		panic("wireerror: building wavefront.v0.Error descriptor: " + err.Error())
	}
	errorMD = fd.Messages().Get(0)
}

func marshalError(code, message string) []byte {
	m := dynamicpb.NewMessage(errorMD)
	m.Set(errorMD.Fields().ByName("code"), protoreflect.ValueOfString(code))
	// The message may carry a relayed upstream body (aid envelope) whose bytes
	// are not guaranteed valid UTF-8; proto3 string fields must be. Coerce
	// invalid sequences to U+FFFD so the envelope never fails to marshal.
	m.Set(errorMD.Fields().ByName("message"), protoreflect.ValueOfString(strings.ToValidUTF8(message, "�")))
	b, err := proto.Marshal(m)
	if err != nil {
		panic("wireerror: marshaling wavefront.v0.Error: " + err.Error())
	}
	return b
}
