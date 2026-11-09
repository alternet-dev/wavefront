package adapter

import (
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"

	"github.com/alternet-dev/wavefront/internal/bundle"
	"github.com/alternet-dev/wavefront/internal/bundletest"
)

func benchmarkAdapter(b *testing.B) (*ProtoJSON, *bundle.Contract, []byte) {
	b.Helper()

	loaded, err := bundle.Load(bundletest.Dir(b, ""))
	if err != nil {
		b.Fatalf("load bundle: %v", err)
	}
	contract, ok := loaded.Contract("2024-11")
	if !ok {
		b.Fatal("contract 2024-11 not found")
	}
	descriptor, err := loaded.Message(contract.RequestMessage())
	if err != nil {
		b.Fatalf("resolve request message: %v", err)
	}
	message := dynamicpb.NewMessage(descriptor)
	message.Set(descriptor.Fields().ByName("text"), protoreflect.ValueOfString("benchmark"))
	message.Set(descriptor.Fields().ByName("n"), protoreflect.ValueOfInt32(42))
	request, err := proto.Marshal(message)
	if err != nil {
		b.Fatalf("marshal request: %v", err)
	}
	return NewProtoJSON(loaded), contract, request
}

func BenchmarkProtoJSONDecodeRequest(b *testing.B) {
	codec, contract, request := benchmarkAdapter(b)
	b.ReportAllocs()
	b.SetBytes(int64(len(request)))
	b.ResetTimer()

	for b.Loop() {
		if _, werr := codec.DecodeRequest(contract, request); werr != nil {
			b.Fatalf("DecodeRequest: %v", werr)
		}
	}
}

func BenchmarkProtoJSONEncodeResponse(b *testing.B) {
	codec, contract, _ := benchmarkAdapter(b)
	response := []byte(`{"text":"benchmark","at":"2026-10-18T00:00:00Z"}`)
	b.ReportAllocs()
	b.SetBytes(int64(len(response)))
	b.ResetTimer()

	for b.Loop() {
		if _, _, werr := codec.EncodeResponse(contract, response); werr != nil {
			b.Fatalf("EncodeResponse: %v", werr)
		}
	}
}
