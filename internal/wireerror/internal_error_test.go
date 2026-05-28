package wireerror

import (
	"net/http"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/dynamicpb"
)

func TestInternalErrorIs500(t *testing.T) {
	e := InternalError("boom")
	if e.Code() != "internal_error" {
		t.Errorf("Code()=%q want internal_error", e.Code())
	}
	if e.HTTPStatus() != http.StatusInternalServerError {
		t.Errorf("HTTPStatus()=%d want 500", e.HTTPStatus())
	}
	if ct := e.Headers().Get("Content-Type"); ct != "application/protobuf" {
		t.Errorf("Content-Type=%q want application/protobuf", ct)
	}
	if ra := e.Headers().Get("Retry-After"); ra != "" {
		t.Errorf("Retry-After=%q, want unset on internal_error", ra)
	}
	m := dynamicpb.NewMessage(errorMD)
	if err := proto.Unmarshal(e.ProtoBody(), m); err != nil {
		t.Fatalf("ProtoBody not a wavefront.v0.Error: %v", err)
	}
	if got := m.Get(errorMD.Fields().ByName("code")).String(); got != "internal_error" {
		t.Errorf("proto code=%q want internal_error", got)
	}
	if got := m.Get(errorMD.Fields().ByName("message")).String(); got != "boom" {
		t.Errorf("proto message=%q want boom", got)
	}
}

func TestInternalErrorDefaultMessage(t *testing.T) {
	e := InternalError("")
	if e.Message() == "" {
		t.Error("empty message should fall back to a default")
	}
}
