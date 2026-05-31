package adapter

import (
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
