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
		"malformed JSON":        []byte(`{"query":`),
		"non-object":            []byte(`[{"query":"{ viewer { id } }"}]`),
		"missing query":         []byte(`{"variables":{}}`),
		"non-string query":      []byte(`{"query":42}`),
		"operationName boolean": []byte(`{"query":"{ viewer { id } }","operationName":true}`),
		"variables array":       []byte(`{"query":"{ viewer { id } }","variables":[]}`),
		"extensions array":      []byte(`{"query":"{ viewer { id } }","extensions":[]}`),
		"trailing JSON":         []byte(`{"query":"{ viewer { id } }"} {}`),
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
	in := []byte(`{"data":null,"errors":[{"message":"not found","path":["item"]}]}`)

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
		"malformed JSON":   []byte(`{"data":`),
		"non-object":       []byte(`[1,2,3]`),
		"no result member": []byte(`{"extensions":{"cost":1}}`),
		"data array":       []byte(`{"data":[]}`),
		"null data alone":  []byte(`{"data":null}`),
		"empty errors":     []byte(`{"errors":[]}`),
		"errors null":      []byte(`{"errors":null}`),
		"error not object": []byte(`{"errors":["bad"]}`),
		"message missing":  []byte(`{"errors":[{"path":["item"]}]}`),
		"message not text": []byte(`{"errors":[{"message":42}]}`),
		"extensions array": []byte(`{"data":{},"extensions":[]}`),
		"extensions null":  []byte(`{"data":{},"extensions":null}`),
		"trailing JSON":    []byte(`{"data":{}} {}`),
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
