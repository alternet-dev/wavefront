package adapter

import (
	"encoding/binary"
	"testing"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"
)

func testGRPCFrame(payload []byte) []byte {
	out := make([]byte, grpcEnvelopeSize+len(payload))
	binary.BigEndian.PutUint32(out[1:grpcEnvelopeSize], uint32(len(payload)))
	copy(out[grpcEnvelopeSize:], payload)
	return out
}

func TestGRPCJSONDecodeRequest(t *testing.T) {
	files := testFiles(t)
	a := NewGRPCJSON(testResolver{files})

	ping := dynamicpb.NewMessage(msgOf(t, files, "acme.v1.Ping"))
	ping.Set(ping.Descriptor().Fields().ByName("text"), protoreflect.ValueOfString("hello"))
	ping.Set(ping.Descriptor().Fields().ByName("n"), protoreflect.ValueOfInt32(7))
	payload, err := proto.Marshal(ping)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}

	call, werr := a.DecodeRequest(
		tb{method: "POST", route: "/v3/echo", req: "acme.v1.Ping", resp: "acme.v1.Pong"},
		testGRPCFrame(payload),
	)
	if werr != nil {
		t.Fatalf("DecodeRequest: %v", werr)
	}
	if call.Method != "POST" || call.Path != "/v3/echo" {
		t.Errorf("method/path = %q %q", call.Method, call.Path)
	}
	if call.ContentType != "application/json" {
		t.Errorf("ContentType = %q, want application/json", call.ContentType)
	}
	got := dynamicpb.NewMessage(msgOf(t, files, "acme.v1.Ping"))
	if err := protojson.Unmarshal(call.Body, got); err != nil {
		t.Fatalf("upstream body is not Ping JSON: %v", err)
	}
	if text := got.Get(got.Descriptor().Fields().ByName("text")).String(); text != "hello" {
		t.Errorf("text = %q, want hello", text)
	}
	if n := got.Get(got.Descriptor().Fields().ByName("n")).Int(); n != 7 {
		t.Errorf("n = %d, want 7", n)
	}
}

func TestGRPCJSONDecodeRequestRejectsNonUnaryEnvelope(t *testing.T) {
	a := NewGRPCJSON(testResolver{testFiles(t)})
	binding := tb{method: "POST", route: "/v3/echo", req: "acme.v1.Ping", resp: "acme.v1.Pong"}

	compressed := testGRPCFrame([]byte{0x08, 0x01})
	compressed[0] = 1
	reservedFlag := testGRPCFrame(nil)
	reservedFlag[0] = 2
	truncated := testGRPCFrame([]byte{0x08, 0x01})
	binary.BigEndian.PutUint32(truncated[1:grpcEnvelopeSize], 3)
	extraFrame := append(testGRPCFrame(nil), testGRPCFrame(nil)...)

	tests := []struct {
		name string
		in   []byte
	}{
		{name: "missing envelope", in: []byte{0x08, 0x01}},
		{name: "compressed message", in: compressed},
		{name: "reserved flag", in: reservedFlag},
		{name: "truncated payload", in: truncated},
		{name: "additional frame", in: extraFrame},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, werr := a.DecodeRequest(binding, tc.in)
			if werr == nil || werr.Code() != "decode_failed" {
				t.Fatalf("DecodeRequest error = %v, want decode_failed", werr)
			}
		})
	}
}

func TestGRPCJSONEncodeResponse(t *testing.T) {
	files := testFiles(t)
	a := NewGRPCJSON(testResolver{files})
	binding := tb{method: "POST", route: "/v3/echo", req: "acme.v1.Ping", resp: "acme.v1.Pong"}

	out, contentType, werr := a.EncodeResponse(binding, []byte(`{"text":"ok","at":"2026-05-17T00:00:00Z"}`))
	if werr != nil {
		t.Fatalf("EncodeResponse: %v", werr)
	}
	if contentType != MediaTypeGRPCProto {
		t.Errorf("content type = %q, want %q", contentType, MediaTypeGRPCProto)
	}
	payload, werr := decodeUnaryGRPC(out)
	if werr != nil {
		t.Fatalf("decode framed response: %v", werr)
	}
	pong := dynamicpb.NewMessage(msgOf(t, files, "acme.v1.Pong"))
	if err := proto.Unmarshal(payload, pong); err != nil {
		t.Fatalf("response payload is not Pong: %v", err)
	}
	if text := pong.Get(pong.Descriptor().Fields().ByName("text")).String(); text != "ok" {
		t.Errorf("text = %q, want ok", text)
	}
}

func TestGRPCJSONFramesEmptyMessages(t *testing.T) {
	a := NewGRPCJSON(testResolver{testFiles(t)})
	binding := tb{method: "GET", route: "/v3/items", req: "acme.v1.Empty", resp: "acme.v1.Empty"}

	call, werr := a.DecodeRequest(binding, testGRPCFrame(nil))
	if werr != nil {
		t.Fatalf("DecodeRequest: %v", werr)
	}
	if len(call.Body) != 0 || call.ContentType != "" {
		t.Errorf("bodyless upstream call = body %q, content type %q", call.Body, call.ContentType)
	}

	out, contentType, werr := a.EncodeResponse(binding, nil)
	if werr != nil {
		t.Fatalf("EncodeResponse: %v", werr)
	}
	if contentType != MediaTypeGRPCProto {
		t.Errorf("content type = %q, want %q", contentType, MediaTypeGRPCProto)
	}
	if len(out) != grpcEnvelopeSize {
		t.Fatalf("framed empty response length = %d, want %d", len(out), grpcEnvelopeSize)
	}
	if payload, werr := decodeUnaryGRPC(out); werr != nil || len(payload) != 0 {
		t.Errorf("decode empty response = payload %q, error %v", payload, werr)
	}
}

func TestGRPCJSONEncodeError(t *testing.T) {
	files := testFiles(t)
	a := NewGRPCJSON(testResolver{files})
	binding := tb{
		method: "POST", route: "/v3/echo", req: "acme.v1.Ping", resp: "acme.v1.Pong",
		errs: map[int]string{409: "acme.v1.Ping"},
	}

	out, contentType, werr := a.EncodeError(binding, 409, []byte(`{"text":"conflict","n":42}`))
	if werr != nil {
		t.Fatalf("EncodeError: %v", werr)
	}
	if contentType != MediaTypeGRPCProto {
		t.Errorf("content type = %q, want %q", contentType, MediaTypeGRPCProto)
	}
	payload, werr := decodeUnaryGRPC(out)
	if werr != nil {
		t.Fatalf("decode framed error: %v", werr)
	}
	msg := dynamicpb.NewMessage(msgOf(t, files, "acme.v1.Ping"))
	if err := proto.Unmarshal(payload, msg); err != nil {
		t.Fatalf("error payload is not Ping: %v", err)
	}
	if n := msg.Get(msg.Descriptor().Fields().ByName("n")).Int(); n != 42 {
		t.Errorf("n = %d, want 42", n)
	}
}

// TestGRPCJSONDecodeRequestPropagatesInvalidProto verifies that invalid
// protobuf bytes wrapped in a valid gRPC envelope produce decode_failed, not
// a framing error. The envelope passes; the inner codec rejects the payload.
func TestGRPCJSONDecodeRequestPropagatesInvalidProto(t *testing.T) {
	a := NewGRPCJSON(testResolver{testFiles(t)})
	binding := tb{method: "POST", route: "/v3/echo", req: "acme.v1.Ping", resp: "acme.v1.Pong"}

	garbage := testGRPCFrame([]byte("\xde\xad\xbe\xef not proto"))
	_, werr := a.DecodeRequest(binding, garbage)
	if werr == nil || werr.Code() != "decode_failed" {
		t.Fatalf("DecodeRequest error = %v, want decode_failed", werr)
	}
}

// TestGRPCJSONEncodeResponsePropagatesError verifies that invalid upstream JSON
// in EncodeResponse surfaces as upstream_error through the gRPC wrapper.
func TestGRPCJSONEncodeResponsePropagatesError(t *testing.T) {
	a := NewGRPCJSON(testResolver{testFiles(t)})
	binding := tb{method: "POST", route: "/v3/echo", req: "acme.v1.Ping", resp: "acme.v1.Pong"}

	_, _, werr := a.EncodeResponse(binding, []byte("definitely not json"))
	if werr == nil || werr.Code() != "upstream_error" {
		t.Fatalf("EncodeResponse error = %v, want upstream_error", werr)
	}
}

// TestGRPCJSONEncodeErrorUndeclaredStatus verifies that an undeclared error
// status surfaces as upstream_error through the gRPC wrapper.
func TestGRPCJSONEncodeErrorUndeclaredStatus(t *testing.T) {
	a := NewGRPCJSON(testResolver{testFiles(t)})
	binding := tb{method: "POST", route: "/v3/echo", req: "acme.v1.Ping", resp: "acme.v1.Pong"}

	_, _, werr := a.EncodeError(binding, 500, []byte(`{}`))
	if werr == nil || werr.Code() != "upstream_error" {
		t.Fatalf("EncodeError error = %v, want upstream_error", werr)
	}
}
