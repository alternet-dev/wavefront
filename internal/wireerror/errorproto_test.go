package wireerror

import (
	"testing"
	"unicode/utf8"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/dynamicpb"
)

// marshalError stuffs an arbitrary (possibly relayed-upstream) string into the
// proto3 message field, which must be valid UTF-8 to marshal. Invalid bytes
// must be coerced, never panic.
func TestMarshalErrorCoercesInvalidUTF8(t *testing.T) {
	var body []byte
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("marshalError panicked on invalid UTF-8: %v", r)
			}
		}()
		body = marshalError(codeUpstreamStatus, "boom\xff\xfe\x80tail")
	}()
	if len(body) == 0 {
		t.Fatal("empty body")
	}
	m := dynamicpb.NewMessage(errorMD)
	if err := proto.Unmarshal(body, m); err != nil {
		t.Fatalf("not a valid wavefront.v0.Error: %v", err)
	}
	got := m.Get(errorMD.Fields().ByName("message")).String()
	if !utf8.ValidString(got) {
		t.Errorf("message is not valid UTF-8: %q", got)
	}
}
