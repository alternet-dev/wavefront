package server

import (
	"net/http"
	"testing"

	"github.com/alternet-dev/wavefront/internal/adapter"
)

func testCodecRegistry() codecRegistry {
	return newCodecRegistry(adapter.NewProtoJSON(nil), adapter.NewJSONJSON(nil))
}

func TestAddVaryMergesTokensAndHonorsWildcard(t *testing.T) {
	h := http.Header{"Vary": {"Origin, Accept-Encoding"}}
	addVary(h, "Accept", "origin")
	if got := h.Get("Vary"); got != "Origin, Accept-Encoding, Accept" {
		t.Errorf("Vary = %q, want merged and deduplicated tokens", got)
	}
	addVary(h, "*")
	if got := h.Get("Vary"); got != "*" {
		t.Errorf("Vary = %q after wildcard, want *", got)
	}
}

func TestRequestAdapterUsesContentTypeOnlyWhenBodyIsPresent(t *testing.T) {
	registry := testCodecRegistry()
	cases := []struct {
		name        string
		contentType string
		hasBody     bool
		want        string
		ok          bool
	}{
		{name: "protobuf", contentType: "application/protobuf", hasBody: true, want: adapter.MediaTypeProtobuf, ok: true},
		{name: "protobuf parameters", contentType: "APPLICATION/PROTOBUF; charset=binary", hasBody: true, want: adapter.MediaTypeProtobuf, ok: true},
		{name: "json", contentType: "application/json; charset=utf-8", hasBody: true, want: adapter.MediaTypeJSON, ok: true},
		{name: "missing with body", hasBody: true, ok: false},
		{name: "unsupported with body", contentType: "text/plain", hasBody: true, ok: false},
		{name: "bodyless defaults protobuf", contentType: "text/plain", hasBody: false, want: adapter.MediaTypeProtobuf, ok: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := registry.requestAdapter(tc.contentType, tc.hasBody)
			if ok != tc.ok {
				t.Fatalf("ok = %v, want %v", ok, tc.ok)
			}
			if ok && got.MediaType() != tc.want {
				t.Errorf("media type = %q, want %q", got.MediaType(), tc.want)
			}
		})
	}
}

func TestResponseAdapterHonorsAcceptWeightsSpecificityAndFallback(t *testing.T) {
	registry := testCodecRegistry()
	protoCodec := registry.byMediaType[adapter.MediaTypeProtobuf]
	jsonCodec := registry.byMediaType[adapter.MediaTypeJSON]
	cases := []struct {
		name     string
		accept   []string
		fallback adapter.Adapter
		want     string
		ok       bool
	}{
		{name: "absent follows protobuf request", fallback: protoCodec, want: adapter.MediaTypeProtobuf, ok: true},
		{name: "absent follows json request", fallback: jsonCodec, want: adapter.MediaTypeJSON, ok: true},
		{name: "exact json", accept: []string{"application/json"}, fallback: protoCodec, want: adapter.MediaTypeJSON, ok: true},
		{name: "exact protobuf", accept: []string{"application/protobuf"}, fallback: jsonCodec, want: adapter.MediaTypeProtobuf, ok: true},
		{name: "quality prefers json", accept: []string{"application/protobuf;q=0.2, application/json;q=0.8"}, fallback: protoCodec, want: adapter.MediaTypeJSON, ok: true},
		{name: "multiple header lines", accept: []string{"application/json;q=0.4", "application/protobuf;q=0.9"}, fallback: jsonCodec, want: adapter.MediaTypeProtobuf, ok: true},
		{name: "wildcard tie follows request", accept: []string{"*/*"}, fallback: jsonCodec, want: adapter.MediaTypeJSON, ok: true},
		{name: "type wildcard tie follows request", accept: []string{"application/*"}, fallback: protoCodec, want: adapter.MediaTypeProtobuf, ok: true},
		{name: "specific exclusion overrides wildcard", accept: []string{"application/json;q=0, */*;q=1"}, fallback: jsonCodec, want: adapter.MediaTypeProtobuf, ok: true},
		{name: "unsupported", accept: []string{"text/plain"}, fallback: protoCodec, ok: false},
		{name: "all supported excluded", accept: []string{"application/json;q=0, application/protobuf;q=0"}, fallback: protoCodec, ok: false},
		{name: "invalid nan quality", accept: []string{"application/json;q=NaN"}, fallback: protoCodec, ok: false},
		{name: "malformed", accept: []string{"not a media range"}, fallback: protoCodec, ok: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := registry.responseAdapter(tc.accept, tc.fallback)
			if ok != tc.ok {
				t.Fatalf("ok = %v, want %v", ok, tc.ok)
			}
			if ok && got.MediaType() != tc.want {
				t.Errorf("media type = %q, want %q", got.MediaType(), tc.want)
			}
		})
	}
}
