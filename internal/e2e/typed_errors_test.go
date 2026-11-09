package e2e

import (
	"bytes"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/dynamicpb"

	"github.com/alternet-dev/wavefront/internal/bundlegen"
)

const typedErrorOpenAPI = `{
  "openapi": "3.0.0",
  "info": {"title": "acme", "version": "2027-03-03"},
  "paths": {"/v3/widgets": {"post": {
    "operationId": "createWidget",
    "requestBody": {"content": {"application/json": {"schema": {"$ref": "#/components/schemas/Widget"}}}},
    "responses": {
      "200": {"content": {"application/json": {"schema": {"$ref": "#/components/schemas/Widget"}}}},
      "409": {"content": {"application/json": {"schema": {"$ref": "#/components/schemas/Problem"}}}}
    }
  }}},
  "components": {"schemas": {
    "Widget": {"type": "object", "properties": {"id": {"type": "string"}}},
    "Problem": {"type": "object", "properties": {"detail": {"type": "string"}, "status": {"type": "integer"}}}
  }}
}`

// bodyDirectWKTErrorOpenAPI binds opaque error bodies directly to the struct
// well-known types. These bindings are intentionally not nested under a
// generated message: the error body itself is the open object or untagged
// union. (#121)
const bodyDirectWKTErrorOpenAPI = `{
  "openapi": "3.1.0",
  "info": {"title": "opaque errors", "version": "2027-03-04"},
  "paths": {
    "/v3/error/open": {"post": {"operationId": "openError",
      "requestBody": {"content": {"application/json": {"schema": {"$ref": "#/components/schemas/Request"}}}},
      "responses": {
        "200": {"content": {"application/json": {"schema": {"$ref": "#/components/schemas/Response"}}}},
        "422": {"content": {"application/json": {"schema": {"$ref": "#/components/schemas/OpenError"}}}}
      }}},
    "/v3/error/value": {"post": {"operationId": "valueError",
      "requestBody": {"content": {"application/json": {"schema": {"$ref": "#/components/schemas/Request"}}}},
      "responses": {
        "200": {"content": {"application/json": {"schema": {"$ref": "#/components/schemas/Response"}}}},
        "500": {"content": {"application/json": {"schema": {"$ref": "#/components/schemas/ValueError"}}}}
      }}}
  },
  "components": {"schemas": {
    "Request": {"type": "object", "properties": {"id": {"type": "string"}}},
    "Response": {"type": "object", "properties": {"id": {"type": "string"}}},
    "OpenError": {"type": "object", "additionalProperties": true},
    "ValueError": {"oneOf": [{"type": "string"}, {"type": "integer"}]}
  }}
}`

func typedErrorBundleDir(t *testing.T) string {
	t.Helper()
	in := filepath.Join(t.TempDir(), "openapi.json")
	if err := os.WriteFile(in, []byte(typedErrorOpenAPI), 0o600); err != nil {
		t.Fatalf("write openapi: %v", err)
	}
	out := t.TempDir()
	if err := bundlegen.Add(in, out, false); err != nil {
		t.Fatalf("bundlegen.Add: %v", err)
	}
	return out
}

// A declared upstream 409 reaches the client as a typed Problem with the status
// preserved and no X-Wavefront-Error.
func TestE2EDeclaredErrorIsTyped(t *testing.T) {
	dir := typedErrorBundleDir(t)
	h := Spawn(t, SpawnOpts{
		BundleDir: dir,
		BackendHandler: func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict)
			_, _ = io.WriteString(w, `{"detail":"duplicate","status":409}`)
		},
	})

	// A Widget request body; the proxy decodes protobuf → JSON upstream.
	c, ok := h.Bundle.LookupRoute("/v3/widgets", http.MethodPost)
	if !ok {
		t.Fatal("route not bound")
	}
	reqMD, err := h.Bundle.Message(c.RequestMessage())
	if err != nil {
		t.Fatalf("resolve request_message: %v", err)
	}
	reqBody, _ := proto.Marshal(dynamicpb.NewMessage(reqMD))

	req, _ := http.NewRequest(http.MethodPost, h.Proxy.URL+"/v3/widgets", bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/protobuf")
	req.Header.Set("X-Api-Contract-Version", "2027-03-03")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409", resp.StatusCode)
	}
	if got := resp.Header.Get("X-Wavefront-Error"); got != "" {
		t.Errorf("X-Wavefront-Error = %q, want empty (declared typed error)", got)
	}
	name, ok := c.ErrorMessage(409)
	if !ok {
		t.Fatal("409 not declared in generated bundle")
	}
	out, _ := io.ReadAll(resp.Body)
	probMD, err := h.Bundle.Message(name)
	if err != nil {
		t.Fatalf("resolve %s: %v", name, err)
	}
	prob := dynamicpb.NewMessage(probMD)
	if err := proto.Unmarshal(out, prob); err != nil {
		t.Fatalf("body is not %s: %v (raw=%q)", name, err, out)
	}
	if got := prob.Get(probMD.Fields().ByName("detail")).String(); got != "duplicate" {
		t.Errorf("decoded detail = %q, want duplicate", got)
	}
}

// Direct Struct and Value bindings must work for declared error responses, not
// just for successful request and response bodies. This reaches the proxy's
// EncodeError path and confirms it preserves the upstream JSON shape. (#121)
func TestE2EDeclaredDirectWKTErrorIsTyped(t *testing.T) {
	dir := generatedBundleDirFrom(t, bodyDirectWKTErrorOpenAPI)
	h := Spawn(t, SpawnOpts{
		BundleDir: dir,
		BackendHandler: func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			switch r.URL.Path {
			case "/v3/error/open":
				w.WriteHeader(http.StatusUnprocessableEntity)
				_, _ = io.WriteString(w, `{"field":"name","reasons":["missing","invalid"]}`)
			case "/v3/error/value":
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = io.WriteString(w, `17`)
			default:
				t.Errorf("unexpected upstream route %s", r.URL.Path)
				http.Error(w, "unexpected route", http.StatusInternalServerError)
			}
		},
	})

	for _, tc := range []struct {
		name    string
		path    string
		status  int
		message string
		body    string
	}{
		{
			name:    "Struct",
			path:    "/v3/error/open",
			status:  http.StatusUnprocessableEntity,
			message: "google.protobuf.Struct",
			body:    `{"field":"name","reasons":["missing","invalid"]}`,
		},
		{
			name:    "Value",
			path:    "/v3/error/value",
			status:  http.StatusInternalServerError,
			message: "google.protobuf.Value",
			body:    `17`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			contract, ok := h.Bundle.Lookup(tc.path, http.MethodPost, "2027-03-04")
			if !ok {
				t.Fatalf("contract for %s not found", tc.path)
			}
			requestMD, err := h.Bundle.Message(contract.RequestMessage())
			if err != nil {
				t.Fatalf("resolve request message: %v", err)
			}
			requestBody, err := proto.Marshal(dynamicpb.NewMessage(requestMD))
			if err != nil {
				t.Fatalf("marshal request: %v", err)
			}

			req, err := http.NewRequest(http.MethodPost, h.Proxy.URL+tc.path, bytes.NewReader(requestBody))
			if err != nil {
				t.Fatalf("new request: %v", err)
			}
			req.Header.Set("Content-Type", "application/protobuf")
			req.Header.Set("X-Api-Contract-Version", "2027-03-04")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("POST: %v", err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != tc.status {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tc.status)
			}
			if got := resp.Header.Get("X-Wavefront-Error"); got != "" {
				t.Errorf("X-Wavefront-Error = %q, want empty for declared typed error", got)
			}
			name, ok := contract.ErrorMessage(tc.status)
			if !ok || name != tc.message {
				t.Fatalf("error message for %d = %q, present=%v, want %q", tc.status, name, ok, tc.message)
			}
			responseMD, err := h.Bundle.Message(name)
			if err != nil {
				t.Fatalf("resolve error message: %v", err)
			}
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatalf("read response: %v", err)
			}
			response := dynamicpb.NewMessage(responseMD)
			if err := proto.Unmarshal(body, response); err != nil {
				t.Fatalf("response is not %s protobuf: %v", name, err)
			}
			got, err := protojson.Marshal(response)
			if err != nil {
				t.Fatalf("marshal response as JSON: %v", err)
			}
			assertJSONEqual(t, got, []byte(tc.body))
		})
	}
}

// An undeclared upstream 503 (not in error_messages, non-strict) reaches the
// client with status preserved and the aid envelope.
func TestE2EUndeclaredStatusIsAidRelayed(t *testing.T) {
	dir := typedErrorBundleDir(t)
	h := Spawn(t, SpawnOpts{
		BundleDir: dir,
		BackendHandler: func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, `{"detail":"maintenance"}`)
		},
	})
	c, _ := h.Bundle.LookupRoute("/v3/widgets", http.MethodPost)
	reqMD, _ := h.Bundle.Message(c.RequestMessage())
	reqBody, _ := proto.Marshal(dynamicpb.NewMessage(reqMD))

	req, _ := http.NewRequest(http.MethodPost, h.Proxy.URL+"/v3/widgets", bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/protobuf")
	req.Header.Set("X-Api-Contract-Version", "2027-03-03")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (preserved)", resp.StatusCode)
	}
	if got := resp.Header.Get("X-Wavefront-Error"); got != "upstream_status" {
		t.Errorf("X-Wavefront-Error = %q, want upstream_status", got)
	}
}
