package adapter

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/alternet-dev/wavefront/internal/wireerror"
)

// testFiles builds a self-contained registry: the well-known Timestamp file
// plus acme.v1 {Ping{string text=1; int32 n=2}, Pong{string text=1;
// google.protobuf.Timestamp at=2}}. Exercises scalars + a WKT through
// protojson, the same path the generated bundle will use.
func testFiles(t *testing.T) *protoregistry.Files {
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
	tsField := &descriptorpb.FieldDescriptorProto{
		Name: proto.String("at"), Number: proto.Int32(2),
		Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
		Type:     descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum(),
		TypeName: proto.String(".google.protobuf.Timestamp"),
		JsonName: proto.String("at"),
	}

	acme := &descriptorpb.FileDescriptorProto{
		Name:       proto.String("acme/v1/types.proto"),
		Package:    proto.String("acme.v1"),
		Syntax:     proto.String("proto3"),
		Dependency: []string{"google/protobuf/timestamp.proto"},
		MessageType: []*descriptorpb.DescriptorProto{
			{Name: proto.String("Ping"), Field: []*descriptorpb.FieldDescriptorProto{str("text", 1), i32("n", 2)}},
			{Name: proto.String("Pong"), Field: []*descriptorpb.FieldDescriptorProto{str("text", 1), tsField}},
			// Empty mirrors the generator's synthetic zero-field message bound
			// to bodyless operations: no fields, used to detect the bodyless case.
			{Name: proto.String("Empty")},
		},
	}
	fds := &descriptorpb.FileDescriptorSet{File: []*descriptorpb.FileDescriptorProto{tsFDP, acme}}
	files, err := protodesc.NewFiles(fds)
	if err != nil {
		t.Fatalf("build files: %v", err)
	}
	return files
}

type testResolver struct{ files *protoregistry.Files }

func (r testResolver) Message(name string) (protoreflect.MessageDescriptor, error) {
	d, err := r.files.FindDescriptorByName(protoreflect.FullName(name))
	if err != nil {
		return nil, err
	}
	return d.(protoreflect.MessageDescriptor), nil
}

type tb struct {
	method, route, req, resp string
	errs                     map[int]string
}

func (b tb) Method() string          { return b.method }
func (b tb) Route() string           { return b.route }
func (b tb) RequestMessage() string  { return b.req }
func (b tb) ResponseMessage() string { return b.resp }
func (b tb) ErrorMessage(status int) (string, bool) {
	name, ok := b.errs[status]
	return name, ok
}

func msgOf(t *testing.T, files *protoregistry.Files, name string) protoreflect.MessageDescriptor {
	t.Helper()
	d, err := files.FindDescriptorByName(protoreflect.FullName(name))
	if err != nil {
		t.Fatalf("find %s: %v", name, err)
	}
	return d.(protoreflect.MessageDescriptor)
}

var _ Adapter = (*ProtoJSON)(nil)

func TestDecodeRequestProtoToJSON(t *testing.T) {
	files := testFiles(t)
	a := NewProtoJSON(testResolver{files})

	ping := dynamicpb.NewMessage(msgOf(t, files, "acme.v1.Ping"))
	ping.Set(ping.Descriptor().Fields().ByName("text"), protoreflect.ValueOfString("hi"))
	ping.Set(ping.Descriptor().Fields().ByName("n"), protoreflect.ValueOfInt32(7))
	in, err := proto.Marshal(ping)
	if err != nil {
		t.Fatalf("marshal ping: %v", err)
	}

	call, werr := a.DecodeRequest(tb{method: "POST", route: "/v3/x", req: "acme.v1.Ping", resp: "acme.v1.Pong"}, in)
	if werr != nil {
		t.Fatalf("DecodeRequest: %v", werr)
	}
	if call.Method != "POST" || call.Path != "/v3/x" {
		t.Errorf("method/path = %q %q", call.Method, call.Path)
	}
	if call.ContentType != "application/json" {
		t.Errorf("ContentType = %q", call.ContentType)
	}
	back := dynamicpb.NewMessage(msgOf(t, files, "acme.v1.Ping"))
	if err := protojson.Unmarshal(call.Body, back); err != nil {
		t.Fatalf("body not valid json for Ping: %v (body=%s)", err, call.Body)
	}
	if got := back.Get(back.Descriptor().Fields().ByName("text")).String(); got != "hi" {
		t.Errorf("decoded text = %q", got)
	}
	if got := back.Get(back.Descriptor().Fields().ByName("n")).Int(); got != 7 {
		t.Errorf("decoded n = %d", got)
	}
}

func TestDecodeRequestInvalidProtobuf(t *testing.T) {
	a := NewProtoJSON(testResolver{testFiles(t)})
	_, werr := a.DecodeRequest(tb{method: "POST", route: "/x", req: "acme.v1.Ping", resp: "acme.v1.Pong"}, []byte("\xde\xad\xbe\xef not proto"))
	if werr == nil || werr.Code() != "decode_failed" {
		t.Fatalf("want decode_failed, got %v", werr)
	}
}

// TestDecodeRequestEmptyMessageSendsNoBody: a zero-field request_message is
// the generator's synthetic Empty bound to a bodyless operation. The upstream
// call must carry no body and no Content-Type — the inbound bytes are not
// decoded, so the upstream GET/path-only POST is clean (never `{}`).
func TestDecodeRequestEmptyMessageSendsNoBody(t *testing.T) {
	a := NewProtoJSON(testResolver{testFiles(t)})

	call, werr := a.DecodeRequest(tb{method: "GET", route: "/v3/items/{id}", req: "acme.v1.Empty", resp: "acme.v1.Pong"}, nil)
	if werr != nil {
		t.Fatalf("DecodeRequest: %v", werr)
	}
	if call.Method != "GET" || call.Path != "/v3/items/{id}" {
		t.Errorf("method/path = %q %q", call.Method, call.Path)
	}
	if len(call.Body) != 0 {
		t.Errorf("Body = %q, want empty (bodyless upstream call must not send {})", call.Body)
	}
	if call.ContentType != "" {
		t.Errorf("ContentType = %q, want empty (no body declares no envelope)", call.ContentType)
	}
}

// TestEncodeResponseEmptyMessageReturnsNoBody: a zero-field response_message is
// the synthetic Empty (e.g. an operation whose only declared response is 204).
// EncodeResponse must not attempt to unmarshal the absent upstream body; it
// returns no body.
func TestEncodeResponseEmptyMessageReturnsNoBody(t *testing.T) {
	a := NewProtoJSON(testResolver{testFiles(t)})

	out, ct, werr := a.EncodeResponse(tb{method: "DELETE", route: "/v3/items/{id}", req: "acme.v1.Empty", resp: "acme.v1.Empty"}, nil)
	if werr != nil {
		t.Fatalf("EncodeResponse: %v", werr)
	}
	if len(out) != 0 {
		t.Errorf("out = %q, want empty", out)
	}
	if ct != "application/protobuf" {
		t.Errorf("contentType = %q, want application/protobuf", ct)
	}
}

func TestEncodeResponseJSONToProtoWithWKT(t *testing.T) {
	files := testFiles(t)
	a := NewProtoJSON(testResolver{files})

	upstream := []byte(`{"text":"ok","at":"2026-05-17T00:00:00Z"}`)
	out, ct, werr := a.EncodeResponse(tb{method: "POST", route: "/x", req: "acme.v1.Ping", resp: "acme.v1.Pong"}, upstream)
	if werr != nil {
		t.Fatalf("EncodeResponse: %v", werr)
	}
	if ct != "application/protobuf" {
		t.Errorf("contentType = %q", ct)
	}
	pong := dynamicpb.NewMessage(msgOf(t, files, "acme.v1.Pong"))
	if err := proto.Unmarshal(out, pong); err != nil {
		t.Fatalf("out not valid protobuf for Pong: %v", err)
	}
	if got := pong.Get(pong.Descriptor().Fields().ByName("text")).String(); got != "ok" {
		t.Errorf("decoded text = %q", got)
	}
	atv := pong.Get(pong.Descriptor().Fields().ByName("at")).Message()
	secs := atv.Get(atv.Descriptor().Fields().ByName("seconds")).Int()
	want, _ := time.Parse(time.RFC3339, "2026-05-17T00:00:00Z")
	if secs != want.Unix() {
		t.Errorf("decoded timestamp seconds = %d, want %d", secs, want.Unix())
	}
}

func TestEncodeResponseInvalidJSON(t *testing.T) {
	a := NewProtoJSON(testResolver{testFiles(t)})
	_, _, werr := a.EncodeResponse(tb{method: "POST", route: "/x", req: "acme.v1.Ping", resp: "acme.v1.Pong"}, []byte("definitely not json"))
	if werr == nil || werr.Code() != "upstream_error" {
		t.Fatalf("want upstream_error, got %v", werr)
	}
}

func TestUnresolvableBindingIsTypedError(t *testing.T) {
	a := NewProtoJSON(testResolver{testFiles(t)})
	_, werr := a.DecodeRequest(tb{method: "POST", route: "/x", req: "acme.v1.Ghost", resp: "acme.v1.Pong"}, []byte{})
	var we *wireerror.Error
	if !errors.As(error(werr), &we) {
		t.Fatalf("want *wireerror.Error, got %v", werr)
	}
}

func TestEncodeErrorTypesDeclaredStatus(t *testing.T) {
	files := testFiles(t)
	a := NewProtoJSON(testResolver{files})
	// Use acme.v1.Ping (int32 n=2) as the declared error message type.
	b := tb{method: "POST", route: "/v3/echo", req: "acme.v1.Ping", resp: "acme.v1.Pong",
		errs: map[int]string{409: "acme.v1.Ping"}}

	out, ct, werr := a.EncodeError(b, 409, []byte(`{"n":42,"text":"conflict"}`))
	if werr != nil {
		t.Fatalf("EncodeError: %v", werr)
	}
	if ct != "application/protobuf" {
		t.Errorf("content-type = %q, want application/protobuf", ct)
	}
	msg := dynamicpb.NewMessage(msgOf(t, files, "acme.v1.Ping"))
	if err := proto.Unmarshal(out, msg); err != nil {
		t.Fatalf("output is not acme.v1.Ping: %v", err)
	}
	if got := msg.Get(msgOf(t, files, "acme.v1.Ping").Fields().ByName("n")).Int(); got != 42 {
		t.Errorf("decoded n = %d, want 42", got)
	}
}

func TestEncodeErrorUnboundStatusIsUpstreamError(t *testing.T) {
	files := testFiles(t)
	a := NewProtoJSON(testResolver{files})
	b := tb{method: "POST", route: "/v3/echo", resp: "acme.v1.Pong", errs: nil}
	if _, _, werr := a.EncodeError(b, 409, []byte(`{}`)); werr == nil {
		t.Fatal("EncodeError on an unbound status returned nil error; want upstream_error")
	}
}

// discFiles builds a self-contained registry with a nested discriminator
// wrapper: Env{Pet pet = 1} and Env.Pet{oneof variant {Cat cat; Dog dog}},
// where Cat has fields petType + meow, Dog has petType + woof. This is the
// shape bundlegen emits for a mapped discriminated union property (#132).
func discFiles(t *testing.T) *protoregistry.Files {
	t.Helper()
	str := func(name string, n int32) *descriptorpb.FieldDescriptorProto {
		return &descriptorpb.FieldDescriptorProto{
			Name: proto.String(name), Number: proto.Int32(n),
			Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
			Type:     descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(),
			JsonName: proto.String(name),
		}
	}
	member := func(name string, n int32, typeName string) *descriptorpb.FieldDescriptorProto {
		return &descriptorpb.FieldDescriptorProto{
			Name: proto.String(name), Number: proto.Int32(n),
			Label:      descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
			Type:       descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum(),
			TypeName:   proto.String(typeName),
			JsonName:   proto.String(name),
			OneofIndex: proto.Int32(0),
		}
	}
	cat := &descriptorpb.DescriptorProto{Name: proto.String("Cat"), Field: []*descriptorpb.FieldDescriptorProto{str("petType", 1), str("meow", 2)}}
	dog := &descriptorpb.DescriptorProto{Name: proto.String("Dog"), Field: []*descriptorpb.FieldDescriptorProto{str("petType", 1), str("woof", 2)}}
	petVariant := &descriptorpb.DescriptorProto{
		Name:      proto.String("PetVariant"),
		OneofDecl: []*descriptorpb.OneofDescriptorProto{{Name: proto.String("variant")}},
		Field: []*descriptorpb.FieldDescriptorProto{
			member("cat", 1, ".acme.v1.Cat"),
			member("dog", 2, ".acme.v1.Dog"),
		},
	}
	env := &descriptorpb.DescriptorProto{
		Name: proto.String("Env"),
		Field: []*descriptorpb.FieldDescriptorProto{
			{
				Name:     proto.String("pet"),
				Number:   proto.Int32(1),
				Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
				Type:     descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum(),
				TypeName: proto.String(".acme.v1.Env.PetVariant"),
				JsonName: proto.String("pet"),
			},
			{
				Name:     proto.String("pets"),
				Number:   proto.Int32(2),
				Label:    descriptorpb.FieldDescriptorProto_LABEL_REPEATED.Enum(),
				Type:     descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum(),
				TypeName: proto.String(".acme.v1.Env.PetVariant"),
				JsonName: proto.String("pets"),
			},
		},
		NestedType: []*descriptorpb.DescriptorProto{petVariant},
	}
	acme := &descriptorpb.FileDescriptorProto{
		Name:        proto.String("acme/v1/pet.proto"),
		Package:     proto.String("acme.v1"),
		Syntax:      proto.String("proto3"),
		MessageType: []*descriptorpb.DescriptorProto{cat, dog, env},
	}
	fds := &descriptorpb.FileDescriptorSet{File: []*descriptorpb.FileDescriptorProto{acme}}
	files, err := protodesc.NewFiles(fds)
	if err != nil {
		t.Fatalf("build files: %v", err)
	}
	return files
}

type discResolver struct {
	files  *protoregistry.Files
	oneofs map[string]struct {
		prop    string
		mapping map[string]string
	}
}

func (r discResolver) Message(name string) (protoreflect.MessageDescriptor, error) {
	d, err := r.files.FindDescriptorByName(protoreflect.FullName(name))
	if err != nil {
		return nil, err
	}
	return d.(protoreflect.MessageDescriptor), nil
}

func (r discResolver) Discriminator(name string) (string, map[string]string, bool) {
	e, ok := r.oneofs[name]
	if !ok {
		return "", nil, false
	}
	return e.prop, e.mapping, true
}

func newDiscResolver(t *testing.T) discResolver {
	return discResolver{
		files: discFiles(t),
		oneofs: map[string]struct {
			prop    string
			mapping map[string]string
		}{
			"acme.v1.Env.PetVariant": {prop: "petType", mapping: map[string]string{"cat": "cat", "dog": "dog"}},
		},
	}
}

// TestEncodeResponseFlattensFlatUpstreamToTypedOneof: the adapter wraps the
// flattened upstream body under the matched oneof member key so protojson
// resolves it against the wrapper message's typed oneof (#132). The decoded
// proto exposes the typed variant via WhichOneof.
func TestEncodeResponseFlattensFlatUpstreamToTypedOneof(t *testing.T) {
	r := newDiscResolver(t)
	a := NewProtoJSON(r)

	const upstream = `{"pet":{"petType":"cat","meow":"purr"},"pets":[{"petType":"dog","woof":"bark"}]}`
	out, _, werr := a.EncodeResponse(tb{method: "POST", route: "/x", req: "acme.v1.Env", resp: "acme.v1.Env"}, []byte(upstream))
	if werr != nil {
		t.Fatalf("EncodeResponse: %v", werr)
	}
	env := msgOf(t, r.files, "acme.v1.Env")
	msg := dynamicpb.NewMessage(env)
	if err := proto.Unmarshal(out, msg); err != nil {
		t.Fatalf("output is not valid protobuf for Env: %v", err)
	}
	pet := msg.Get(env.Fields().ByName("pet")).Message()
	variant := pet.Descriptor().Oneofs().ByName("variant")
	active := pet.WhichOneof(variant)
	if active == nil || string(active.Name()) != "cat" {
		t.Fatalf("pet oneof active = %v, want cat", active)
	}
	cat := pet.Get(active).Message()
	if got := cat.Get(cat.Descriptor().Fields().ByName("meow")).String(); got != "purr" {
		t.Errorf("cat.meow = %q, want purr", got)
	}
	// The repeated field also went through the walker.
	pets := msg.Get(env.Fields().ByName("pets")).List()
	if pets.Len() != 1 {
		t.Fatalf("pets.len = %d, want 1", pets.Len())
	}
	elem := pets.Get(0).Message()
	activeElem := elem.WhichOneof(elem.Descriptor().Oneofs().ByName("variant"))
	if activeElem == nil || string(activeElem.Name()) != "dog" {
		t.Fatalf("pets[0] active = %v, want dog", activeElem)
	}
}

// TestDecodeRequestFlattensWrappedProtoToUpstream: a proto with the typed
// oneof set must emit the flattened OpenAPI shape (discriminator property +
// member fields at the top level) rather than the protojson "wrapped" shape.
func TestDecodeRequestFlattensWrappedProtoToUpstream(t *testing.T) {
	r := newDiscResolver(t)
	a := NewProtoJSON(r)

	// Build an Env with pet.cat set and pet.cat.{petType=cat, meow=purr}.
	envMD := msgOf(t, r.files, "acme.v1.Env")
	env := dynamicpb.NewMessage(envMD)
	petField := envMD.Fields().ByName("pet")
	petMsg := dynamicpb.NewMessage(petField.Message())
	catField := petField.Message().Fields().ByName("cat")
	catMsg := dynamicpb.NewMessage(catField.Message())
	catMsg.Set(catField.Message().Fields().ByName("petType"), protoreflect.ValueOfString("cat"))
	catMsg.Set(catField.Message().Fields().ByName("meow"), protoreflect.ValueOfString("purr"))
	petMsg.Set(catField, protoreflect.ValueOfMessage(catMsg))
	env.Set(petField, protoreflect.ValueOfMessage(petMsg))

	raw, err := proto.Marshal(env)
	if err != nil {
		t.Fatalf("marshal env: %v", err)
	}
	call, werr := a.DecodeRequest(tb{method: "POST", route: "/x", req: "acme.v1.Env", resp: "acme.v1.Env"}, raw)
	if werr != nil {
		t.Fatalf("DecodeRequest: %v", werr)
	}
	var decoded map[string]any
	if err := json.Unmarshal(call.Body, &decoded); err != nil {
		t.Fatalf("adapter body is not JSON: %v (body=%s)", err, call.Body)
	}
	pet, ok := decoded["pet"].(map[string]any)
	if !ok {
		t.Fatalf("decoded.pet is not an object: %v", decoded["pet"])
	}
	if _, wrapped := pet["cat"]; wrapped {
		t.Fatalf("decoded.pet still carries the wrapped shape %v", pet)
	}
	if pet["petType"] != "cat" || pet["meow"] != "purr" {
		t.Errorf("decoded.pet = %v, want {petType:cat, meow:purr}", pet)
	}
}

// TestEncodeResponseMissingDiscriminatorIsUpstreamError: an upstream body that
// omits the discriminator property for a wrapper position is a contract
// violation. The adapter reports upstream_error rather than fabricating a
// variant.
func TestEncodeResponseMissingDiscriminatorIsUpstreamError(t *testing.T) {
	r := newDiscResolver(t)
	a := NewProtoJSON(r)
	// pet is a wrapper position; no discriminator property.
	_, _, werr := a.EncodeResponse(tb{method: "POST", route: "/x", req: "acme.v1.Env", resp: "acme.v1.Env"}, []byte(`{"pet":{"meow":"purr"}}`))
	if werr == nil || werr.Code() != "upstream_error" {
		t.Fatalf("want upstream_error, got %v", werr)
	}
}

// TestEncodeResponseUnknownDiscriminatorValueIsUpstreamError: a discriminator
// value that is not in the mapping is a contract violation. The adapter
// reports upstream_error.
func TestEncodeResponseUnknownDiscriminatorValueIsUpstreamError(t *testing.T) {
	r := newDiscResolver(t)
	a := NewProtoJSON(r)
	_, _, werr := a.EncodeResponse(tb{method: "POST", route: "/x", req: "acme.v1.Env", resp: "acme.v1.Env"}, []byte(`{"pet":{"petType":"bird","chirp":"tweet"}}`))
	if werr == nil || werr.Code() != "upstream_error" {
		t.Fatalf("want upstream_error, got %v", werr)
	}
}
