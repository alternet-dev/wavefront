package adapter

import (
	"bytes"
	"testing"
)

var _ Adapter = (*GraphQL)(nil)

func TestGraphQLDecodeRequestProducesGraphQLUpstreamCall(t *testing.T) {
	a := NewGraphQL()
	in := []byte("{\n" +
		`  "query": "query Item($id: ID!) { item(id: $id) { id name } }",` + "\n" +
		`  "operationName": "Item",` + "\n" +
		`  "variables": {"id": "item-7"},` + "\n" +
		`  "extensions": {"trace": true}` + "\n" +
		"}")

	call, werr := a.DecodeRequest(tb{
		method: "GET", route: "/ignored", req: "acme.v1.Ping", resp: "acme.v1.Pong",
	}, in)
	if werr != nil {
		t.Fatalf("DecodeRequest: %v", werr)
	}
	if call.Method != "POST" || call.Path != "/graphql" {
		t.Errorf("method/path = %q %q, want POST /graphql", call.Method, call.Path)
	}
	if call.RawQuery != "" {
		t.Errorf("RawQuery = %q, want empty", call.RawQuery)
	}
	if call.ContentType != "application/json" {
		t.Errorf("ContentType = %q, want application/json", call.ContentType)
	}
	if !bytes.Equal(call.Body, in) {
		t.Errorf("body changed:\n got: %s\nwant: %s", call.Body, in)
	}
}

func TestGraphQLDecodeRequestAcceptsNullableOptionalParameters(t *testing.T) {
	a := NewGraphQL()
	in := []byte(`{"query":"{ viewer { id } }","operationName":null,"variables":null,"extensions":null}`)

	call, werr := a.DecodeRequest(tb{}, in)
	if werr != nil {
		t.Fatalf("DecodeRequest: %v", werr)
	}
	if !bytes.Equal(call.Body, in) {
		t.Errorf("body changed: got %s, want %s", call.Body, in)
	}
}

func TestGraphQLDecodeRequestLeavesDocumentValidationUpstream(t *testing.T) {
	a := NewGraphQL()
	in := []byte(`{"query":""}`)

	call, werr := a.DecodeRequest(tb{}, in)
	if werr != nil {
		t.Fatalf("DecodeRequest: %v", werr)
	}
	if !bytes.Equal(call.Body, in) {
		t.Errorf("body changed: got %s, want %s", call.Body, in)
	}
}

func TestGraphQLDecodeRequestRejectsInvalidEnvelope(t *testing.T) {
	tests := map[string][]byte{
		"malformed JSON":          []byte(`{"query":`),
		"non-object":              []byte(`[{"query":"{ viewer { id } }"}]`),
		"missing query":           []byte(`{"variables":{}}`),
		"non-string query":        []byte(`{"query":42}`),
		"operationName boolean":   []byte(`{"query":"{ viewer { id } }","operationName":true}`),
		"variables array":         []byte(`{"query":"{ viewer { id } }","variables":[]}`),
		"extensions array":        []byte(`{"query":"{ viewer { id } }","extensions":[]}`),
		"duplicate member":        []byte(`{"query":"{ viewer { id } }","query":"{ item { id } }"}`),
		"nested duplicate member": []byte(`{"query":"{ viewer { id } }","variables":{"id":"1","id":"2"}}`),
		"malformed UTF-8":         {0x7b, 0x22, 0x71, 0x75, 0x65, 0x72, 0x79, 0x22, 0x3a, 0x22, 0xff, 0x22, 0x7d},
		"trailing JSON":           []byte(`{"query":"{ viewer { id } }"} {}`),
	}

	a := NewGraphQL()
	for name, in := range tests {
		t.Run(name, func(t *testing.T) {
			_, werr := a.DecodeRequest(tb{}, in)
			if werr == nil || werr.Code() != "decode_failed" {
				t.Fatalf("DecodeRequest error = %v, want decode_failed", werr)
			}
		})
	}
}

func TestGraphQLEncodeResponsePreservesRequestShapedEnvelope(t *testing.T) {
	a := NewGraphQL()
	in := []byte("{\n" +
		`  "data": {"aliasedItem": {"id": "item-7", "name": "seven"}},` + "\n" +
		`  "extensions": {"cost": 2}` + "\n" +
		"}")

	out, ct, werr := a.EncodeResponse(tb{resp: "acme.v1.Pong"}, in)
	if werr != nil {
		t.Fatalf("EncodeResponse: %v", werr)
	}
	if ct != "application/graphql-response+json" {
		t.Errorf("contentType = %q, want application/graphql-response+json", ct)
	}
	if !bytes.Equal(out, in) {
		t.Errorf("response changed:\n got: %s\nwant: %s", out, in)
	}
}

func TestGraphQLEncodeResponseAcceptsErrorEnvelope(t *testing.T) {
	a := NewGraphQL()
	in := []byte(`{"data":null,"errors":[{"message":"not found","locations":[{"line":2,"column":4}],"path":["item",0],"extensions":{"code":"NOT_FOUND"}}]}`)

	out, ct, werr := a.EncodeResponse(tb{}, in)
	if werr != nil {
		t.Fatalf("EncodeResponse: %v", werr)
	}
	if ct != "application/graphql-response+json" {
		t.Errorf("contentType = %q, want application/graphql-response+json", ct)
	}
	if !bytes.Equal(out, in) {
		t.Errorf("response changed: got %s, want %s", out, in)
	}
}

func TestGraphQLEncodeResponseRejectsInvalidEnvelope(t *testing.T) {
	tests := map[string][]byte{
		"malformed JSON":            []byte(`{"data":`),
		"non-object":                []byte(`[1,2,3]`),
		"no result member":          []byte(`{"extensions":{"cost":1}}`),
		"data array":                []byte(`{"data":[]}`),
		"null data alone":           []byte(`{"data":null}`),
		"empty errors":              []byte(`{"errors":[]}`),
		"errors null":               []byte(`{"errors":null}`),
		"error not object":          []byte(`{"errors":["bad"]}`),
		"message missing":           []byte(`{"errors":[{"path":["item"]}]}`),
		"message not text":          []byte(`{"errors":[{"message":42}]}`),
		"unknown top-level member":  []byte(`{"data":{},"unexpected":true}`),
		"path object":               []byte(`{"errors":[{"message":"not found","path":{}}]}`),
		"path boolean segment":      []byte(`{"errors":[{"message":"not found","path":[true]}]}`),
		"locations object":          []byte(`{"errors":[{"message":"not found","locations":{}}]}`),
		"location missing column":   []byte(`{"errors":[{"message":"not found","locations":[{"line":2}]}]}`),
		"location non-integer line": []byte(`{"errors":[{"message":"not found","locations":[{"line":"two","column":4}]}]}`),
		"extensions array":          []byte(`{"data":{},"extensions":[]}`),
		"extensions null":           []byte(`{"data":{},"extensions":null}`),
		"duplicate member":          []byte(`{"data":{},"data":{}}`),
		"nested duplicate member":   []byte(`{"data":{"item":{"id":"1","id":"2"}}}`),
		"malformed UTF-8":           {0x7b, 0x22, 0x64, 0x61, 0x74, 0x61, 0x22, 0x3a, 0x7b, 0x22, 0x6e, 0x61, 0x6d, 0x65, 0x22, 0x3a, 0x22, 0xff, 0x22, 0x7d, 0x7d},
		"trailing JSON":             []byte(`{"data":{}} {}`),
	}

	a := NewGraphQL()
	for name, in := range tests {
		t.Run(name, func(t *testing.T) {
			_, _, werr := a.EncodeResponse(tb{}, in)
			if werr == nil || werr.Code() != "upstream_error" {
				t.Fatalf("EncodeResponse error = %v, want upstream_error", werr)
			}
		})
	}
}

func TestGraphQLEncodeResponseRejectsInvalidErrorExtensions(t *testing.T) {
	a := NewGraphQL()
	in := []byte(`{"errors":[{"message":"not found","extensions":["invalid"]}]}`)

	_, _, werr := a.EncodeResponse(tb{}, in)
	if werr == nil || werr.Code() != "upstream_error" {
		t.Fatalf("EncodeResponse error = %v, want upstream_error", werr)
	}
}

func TestGraphQLEncodeErrorUsesGraphQLEnvelope(t *testing.T) {
	a := NewGraphQL()
	in := []byte(`{"errors":[{"message":"rate limited"}]}`)

	out, ct, werr := a.EncodeError(tb{}, 429, in)
	if werr != nil {
		t.Fatalf("EncodeError: %v", werr)
	}
	if ct != "application/graphql-response+json" {
		t.Errorf("contentType = %q, want application/graphql-response+json", ct)
	}
	if !bytes.Equal(out, in) {
		t.Errorf("response changed: got %s, want %s", out, in)
	}
}

func TestGraphQLEncodeErrorRejectsInvalidEnvelope(t *testing.T) {
	tests := map[string][]byte{
		"malformed JSON":   []byte(`{"errors":`),
		"no result member": []byte(`{"extensions":{"cost":1}}`),
		"empty errors":     []byte(`{"errors":[]}`),
	}

	a := NewGraphQL()
	for name, in := range tests {
		t.Run(name, func(t *testing.T) {
			_, _, werr := a.EncodeError(tb{}, 500, in)
			if werr == nil || werr.Code() != "upstream_error" {
				t.Fatalf("EncodeError error = %v, want upstream_error", werr)
			}
		})
	}
}

func TestGraphQLEncodeResponseAcceptsPartialResult(t *testing.T) {
	a := NewGraphQL()
	// GraphQL spec allows data and errors together for partial results.
	in := []byte(`{"data":{"item":{"id":"1"}},"errors":[{"message":"fetch warning","path":["item","extra"]}]}`)

	out, ct, werr := a.EncodeResponse(tb{}, in)
	if werr != nil {
		t.Fatalf("EncodeResponse: %v", werr)
	}
	if ct != "application/graphql-response+json" {
		t.Errorf("contentType = %q, want application/graphql-response+json", ct)
	}
	if !bytes.Equal(out, in) {
		t.Errorf("response changed: got %s, want %s", out, in)
	}
}

func TestGraphQLDecodeRequestAcceptsEmptyObjectParameters(t *testing.T) {
	a := NewGraphQL()
	// Empty objects are valid for variables and extensions per the spec.
	in := []byte(`{"query":"{ viewer { id } }","variables":{},"extensions":{}}`)

	call, werr := a.DecodeRequest(tb{}, in)
	if werr != nil {
		t.Fatalf("DecodeRequest: %v", werr)
	}
	if !bytes.Equal(call.Body, in) {
		t.Errorf("body changed: got %s, want %s", call.Body, in)
	}
}
