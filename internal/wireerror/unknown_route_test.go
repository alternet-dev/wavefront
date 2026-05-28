package wireerror

import (
	"net/http"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/dynamicpb"
)

func TestUnknownRouteIs404(t *testing.T) {
	e := UnknownRoute("no contract binds GET /nope")
	if e.Code() != "unknown_route" {
		t.Errorf("Code()=%q want unknown_route", e.Code())
	}
	if e.HTTPStatus() != http.StatusNotFound {
		t.Errorf("HTTPStatus()=%d want 404", e.HTTPStatus())
	}
	if ct := e.Headers().Get("Content-Type"); ct != "application/protobuf" {
		t.Errorf("Content-Type=%q want application/protobuf", ct)
	}
	if ra := e.Headers().Get("Retry-After"); ra != "" {
		t.Errorf("Retry-After=%q, want unset on unknown_route", ra)
	}
	m := dynamicpb.NewMessage(errorMD)
	if err := proto.Unmarshal(e.ProtoBody(), m); err != nil {
		t.Fatalf("ProtoBody not a wavefront.v0.Error: %v", err)
	}
	if got := m.Get(errorMD.Fields().ByName("code")).String(); got != "unknown_route" {
		t.Errorf("proto code=%q want unknown_route", got)
	}
	if got := m.Get(errorMD.Fields().ByName("message")).String(); got != "no contract binds GET /nope" {
		t.Errorf("proto message=%q want %q", got, "no contract binds GET /nope")
	}
}

func TestUnknownRouteDefaultMessage(t *testing.T) {
	e := UnknownRoute("")
	if e.Message() == "" {
		t.Error("empty message should fall back to a default")
	}
}
