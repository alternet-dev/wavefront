package wireerror

import (
	"net/http"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/dynamicpb"
)

func TestTransformFailedRequestIs422(t *testing.T) {
	e := TransformFailedRequest("bad")
	if e.Code() != "transform_failed" {
		t.Errorf("Code()=%q want transform_failed", e.Code())
	}
	if e.HTTPStatus() != http.StatusUnprocessableEntity {
		t.Errorf("HTTPStatus()=%d want 422", e.HTTPStatus())
	}
	if ct := e.Headers().Get("Content-Type"); ct != "application/protobuf" {
		t.Errorf("Content-Type=%q want application/protobuf", ct)
	}
	m := dynamicpb.NewMessage(errorMD)
	if err := proto.Unmarshal(e.ProtoBody(), m); err != nil {
		t.Fatalf("ProtoBody not a wavefront.v1.Error: %v", err)
	}
	if got := m.Get(errorMD.Fields().ByName("code")).String(); got != "transform_failed" {
		t.Errorf("proto code=%q want transform_failed", got)
	}
	if got := m.Get(errorMD.Fields().ByName("message")).String(); got != "bad" {
		t.Errorf("proto message=%q want bad", got)
	}
}

func TestTransformFailedResponseIs502(t *testing.T) {
	e := TransformFailedResponse("")
	if e.Code() != "transform_failed" {
		t.Errorf("Code()=%q want transform_failed", e.Code())
	}
	if e.HTTPStatus() != http.StatusBadGateway {
		t.Errorf("HTTPStatus()=%d want 502", e.HTTPStatus())
	}
	if e.Message() == "" {
		t.Error("empty message should fall back to a default")
	}
}
