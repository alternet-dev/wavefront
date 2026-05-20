// Package bundletest is shared test scaffolding: it builds a self-contained
// FileDescriptorSet and writes a valid on-disk bundle so negotiate/server/e2e
// can exercise the real bundle.Load path without protoc. It is imported only
// by tests (never by cmd), so it does not enter the binary.
package bundletest

import (
	"os"
	"path/filepath"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// ValidOpenAPI is a minimal well-formed OpenAPI document.
const ValidOpenAPI = `{"openapi":"3.0.0","info":{"title":"test","version":"1"},"paths":{}}`

// ValidVersions binds contract "2024-11" → POST /v3/echo, acme.v1.Ping →
// acme.v1.Pong.
const ValidVersions = `version: 1
contracts:
  - contract_version: "2024-11"
    route: /v3/echo
    method: POST
    request_message: acme.v1.Ping
    response_message: acme.v1.Pong
`

// FDSBytes returns the wire bytes of a self-contained FileDescriptorSet:
// google.protobuf.Timestamp plus acme.v1 {Ping{string text=1; int32 n=2},
// Pong{string text=1; google.protobuf.Timestamp at=2}}. It also includes
// Item{int32 id=1; string text=2}, Meta{string locale=1}, and
// PingV2{repeated Item items=1; optional Meta meta=2} for nested/array
// transform coverage.
func FDSBytes(t testing.TB) []byte {
	t.Helper()
	tsFDP := protodesc.ToFileDescriptorProto(timestamppb.File_google_protobuf_timestamp_proto)

	str := func(name string, n int32) *descriptorpb.FieldDescriptorProto {
		return &descriptorpb.FieldDescriptorProto{
			Name: proto.String(name), Number: proto.Int32(n),
			Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
			Type:     descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(),
			JsonName: proto.String(name),
		}
	}
	i32 := func(name string, n int32) *descriptorpb.FieldDescriptorProto {
		return &descriptorpb.FieldDescriptorProto{
			Name: proto.String(name), Number: proto.Int32(n),
			Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
			Type:     descriptorpb.FieldDescriptorProto_TYPE_INT32.Enum(),
			JsonName: proto.String(name),
		}
	}
	ts := &descriptorpb.FieldDescriptorProto{
		Name: proto.String("at"), Number: proto.Int32(2),
		Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
		Type:     descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum(),
		TypeName: proto.String(".google.protobuf.Timestamp"),
		JsonName: proto.String("at"),
	}
	itemMsg := &descriptorpb.DescriptorProto{
		Name:  proto.String("Item"),
		Field: []*descriptorpb.FieldDescriptorProto{i32("id", 1), str("text", 2)},
	}
	metaMsg := &descriptorpb.DescriptorProto{
		Name:  proto.String("Meta"),
		Field: []*descriptorpb.FieldDescriptorProto{str("locale", 1)},
	}
	pingV2Items := &descriptorpb.FieldDescriptorProto{
		Name:     proto.String("items"),
		Number:   proto.Int32(1),
		Label:    descriptorpb.FieldDescriptorProto_LABEL_REPEATED.Enum(),
		Type:     descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum(),
		TypeName: proto.String(".acme.v1.Item"),
		JsonName: proto.String("items"),
	}
	pingV2Meta := &descriptorpb.FieldDescriptorProto{
		Name:     proto.String("meta"),
		Number:   proto.Int32(2),
		Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
		Type:     descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum(),
		TypeName: proto.String(".acme.v1.Meta"),
		JsonName: proto.String("meta"),
	}
	pingV2 := &descriptorpb.DescriptorProto{
		Name:  proto.String("PingV2"),
		Field: []*descriptorpb.FieldDescriptorProto{pingV2Items, pingV2Meta},
	}
	acme := &descriptorpb.FileDescriptorProto{
		Name:       proto.String("acme/v1/types.proto"),
		Package:    proto.String("acme.v1"),
		Syntax:     proto.String("proto3"),
		Dependency: []string{"google/protobuf/timestamp.proto"},
		MessageType: []*descriptorpb.DescriptorProto{
			{Name: proto.String("Ping"), Field: []*descriptorpb.FieldDescriptorProto{str("text", 1), i32("n", 2)}},
			{Name: proto.String("Pong"), Field: []*descriptorpb.FieldDescriptorProto{str("text", 1), ts}},
			itemMsg,
			metaMsg,
			pingV2,
		},
	}
	b, err := proto.Marshal(&descriptorpb.FileDescriptorSet{
		File: []*descriptorpb.FileDescriptorProto{tsFDP, acme},
	})
	if err != nil {
		t.Fatalf("marshal fds: %v", err)
	}
	return b
}

// Dir writes a bundle into a fresh temp dir and returns its path. If versions
// is empty, ValidVersions is used.
func Dir(t testing.TB, versions string) string {
	t.Helper()
	if versions == "" {
		versions = ValidVersions
	}
	dir := t.TempDir()
	write := func(name string, b []byte) {
		if err := os.WriteFile(filepath.Join(dir, name), b, 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	write("descriptors.binpb", FDSBytes(t))
	write("openapi.json", []byte(ValidOpenAPI))
	write("versions.yaml", []byte(versions))
	return dir
}
