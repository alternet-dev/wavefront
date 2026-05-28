package wireerror

import (
	"net/http"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/dynamicpb"
)

func TestUnsupportedMediaTypeIs415(t *testing.T) {
	e := UnsupportedMediaType("Content-Type: text/plain")
	if e.Code() != "unsupported_media_type" {
		t.Errorf("Code()=%q want unsupported_media_type", e.Code())
	}
	if e.HTTPStatus() != http.StatusUnsupportedMediaType {
		t.Errorf("HTTPStatus()=%d want 415", e.HTTPStatus())
	}
	if ct := e.Headers().Get("Content-Type"); ct != "application/protobuf" {
		t.Errorf("Content-Type=%q want application/protobuf", ct)
	}
	if ra := e.Headers().Get("Retry-After"); ra != "" {
		t.Errorf("Retry-After=%q, want unset on unsupported_media_type", ra)
	}
	m := dynamicpb.NewMessage(errorMD)
	if err := proto.Unmarshal(e.ProtoBody(), m); err != nil {
		t.Fatalf("ProtoBody not a wavefront.v0.Error: %v", err)
	}
	if got := m.Get(errorMD.Fields().ByName("code")).String(); got != "unsupported_media_type" {
		t.Errorf("proto code=%q want unsupported_media_type", got)
	}
	if got := m.Get(errorMD.Fields().ByName("message")).String(); got != "Content-Type: text/plain" {
		t.Errorf("proto message=%q want %q", got, "Content-Type: text/plain")
	}
}

func TestUnsupportedMediaTypeDefaultMessage(t *testing.T) {
	e := UnsupportedMediaType("")
	if e.Message() == "" {
		t.Error("empty message should fall back to a default")
	}
}
