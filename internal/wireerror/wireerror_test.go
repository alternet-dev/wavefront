package wireerror

import (
	"errors"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/dynamicpb"
)

// decodeBody round-trips ProtoBody() back through the same in-code descriptor
// (white-box) so the test proves the wire shape, not just the struct.
func decodeBody(t *testing.T, b []byte) (code, message string) {
	t.Helper()
	m := dynamicpb.NewMessage(errorMD)
	if err := proto.Unmarshal(b, m); err != nil {
		t.Fatalf("unmarshal wavefront.v0.Error: %v", err)
	}
	c := m.Get(errorMD.Fields().ByName("code")).String()
	msg := m.Get(errorMD.Fields().ByName("message")).String()
	return c, msg
}

func TestCodeStatusTableMatchesProtocol(t *testing.T) {
	cases := []struct {
		err    *Error
		code   string
		status int
	}{
		{UnsupportedContractVersion(""), "unsupported_contract_version", 400},
		{DecodeFailed(""), "decode_failed", 400},
		{RequestBodyTooLarge(""), "request_body_too_large", 413},
		{UpstreamTimeout(""), "upstream_timeout", 504},
		{UpstreamError(""), "upstream_error", 502},
		{TransformFailedRequest(""), "transform_failed", 422},
		{TransformFailedResponse(""), "transform_failed", 502},
	}
	for _, c := range cases {
		if c.err.Code() != c.code {
			t.Errorf("Code() = %q, want %q", c.err.Code(), c.code)
		}
		if c.err.HTTPStatus() != c.status {
			t.Errorf("%s: HTTPStatus() = %d, want %d", c.code, c.err.HTTPStatus(), c.status)
		}
	}
}

func TestContentTypeAlwaysProtobuf(t *testing.T) {
	for _, e := range []*Error{
		UnsupportedContractVersion(""), DecodeFailed(""), RequestBodyTooLarge(""),
		UpstreamTimeout(""), UpstreamError(""),
		TransformFailedRequest(""), TransformFailedResponse(""),
	} {
		if got := e.Headers().Get("Content-Type"); got != "application/protobuf" {
			t.Errorf("%s: Content-Type = %q, want application/protobuf", e.Code(), got)
		}
	}
}

func TestRetryAfterOnlyOnUpstreamTimeout(t *testing.T) {
	if got := UpstreamTimeout("").Headers().Get("Retry-After"); got != "0" {
		t.Errorf("upstream_timeout Retry-After = %q, want 0", got)
	}
	for _, e := range []*Error{
		UnsupportedContractVersion(""), DecodeFailed(""), RequestBodyTooLarge(""), UpstreamError(""),
		TransformFailedRequest(""), TransformFailedResponse(""),
	} {
		if got := e.Headers().Get("Retry-After"); got != "" {
			t.Errorf("%s: Retry-After should be unset, got %q", e.Code(), got)
		}
	}
}

func TestDefaultMessageWhenEmpty(t *testing.T) {
	e := UpstreamError("")
	if e.Message() == "" {
		t.Fatal("empty message should fall back to a default, got empty")
	}
}

func TestCustomMessagePreserved(t *testing.T) {
	e := DecodeFailed("field xyz is not a valid int32")
	if e.Message() != "field xyz is not a valid int32" {
		t.Errorf("Message() = %q, want the custom detail", e.Message())
	}
}

func TestProtoBodyRoundTrips(t *testing.T) {
	e := UnsupportedContractVersion("contract 2099-01 is not in the bundle")
	code, msg := decodeBody(t, e.ProtoBody())
	if code != "unsupported_contract_version" {
		t.Errorf("decoded code = %q", code)
	}
	if msg != "contract 2099-01 is not in the bundle" {
		t.Errorf("decoded message = %q", msg)
	}
}

func TestImplementsError(t *testing.T) {
	var err error = DecodeFailed("boom")
	var we *Error
	if !errors.As(err, &we) {
		t.Fatal("*Error should satisfy error and errors.As")
	}
	if we.Error() == "" {
		t.Error("Error() string should be non-empty")
	}
}
