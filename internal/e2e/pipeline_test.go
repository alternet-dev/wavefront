package e2e

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"

	"github.com/alternet-dev/wavefront/internal/bundlegen"
)

const sampleOpenAPI = `{
  "openapi": "3.0.0",
  "info": {"title": "acme", "version": "2026-05-17"},
  "paths": {"/v3/echo": {"post": {
    "operationId": "echo",
    "requestBody": {"content": {"application/json": {"schema": {"$ref": "#/components/schemas/EchoRequest"}}}},
    "responses": {"200": {"content": {"application/json": {"schema": {"$ref": "#/components/schemas/EchoReply"}}}}}
  }}},
  "components": {"schemas": {
    "EchoRequest": {"type": "object", "properties": {
      "text": {"type": "string"}, "count": {"type": "integer", "format": "int64"}}},
    "EchoReply": {"type": "object", "properties": {
      "text": {"type": "string"}, "ok": {"type": "boolean"}}}
  }}
}`

// generatedBundleDir runs the real OpenAPI → bundle generator and returns the
// resulting bundle directory.
func generatedBundleDir(t *testing.T) string {
	return generatedBundleDirFrom(t, sampleOpenAPI)
}

func generatedBundleDirFrom(t *testing.T, openapi string) string {
	t.Helper()
	in := filepath.Join(t.TempDir(), "openapi.json")
	if err := os.WriteFile(in, []byte(openapi), 0o600); err != nil {
		t.Fatalf("write openapi: %v", err)
	}
	out := t.TempDir()
	if err := bundlegen.Add(in, out, false); err != nil {
		t.Fatalf("bundlegen.Add: %v", err)
	}
	return out
}

const bodyDirectWKTOpenAPI = `{
  "openapi": "3.1.0",
  "info": {"title": "opaque", "version": "2026-10-13"},
  "paths": {
    "/v3/open": {"post": {"operationId": "open",
      "requestBody": {"content": {"application/json": {"schema": {"$ref": "#/components/schemas/Open"}}}},
      "responses": {"200": {"content": {"application/json": {"schema": {"$ref": "#/components/schemas/Open"}}}}}}},
    "/v3/value": {"post": {"operationId": "value",
      "requestBody": {"content": {"application/json": {"schema": {"$ref": "#/components/schemas/Anything"}}}},
      "responses": {"200": {"content": {"application/json": {"schema": {"$ref": "#/components/schemas/Anything"}}}}}}},
    "/v3/union": {"post": {"operationId": "union",
      "requestBody": {"content": {"application/json": {"schema": {"$ref": "#/components/schemas/Choice"}}}},
      "responses": {"200": {"content": {"application/json": {"schema": {"$ref": "#/components/schemas/Choice"}}}}}}}
  },
  "components": {"schemas": {
    "Open": {"type": "object", "additionalProperties": true},
    "Anything": {},
    "Choice": {"oneOf": [
      {"type": "string"},
      {"type": "object", "properties": {"kind": {"type": "string"}, "priority": {"type": "integer"}}}
    ]}
  }}
}`

func TestPipelineRoundTripsBodyDirectWellKnownTypes(t *testing.T) {
	dir := generatedBundleDirFrom(t, bodyDirectWKTOpenAPI)
	h := Spawn(t, SpawnOpts{
		BundleDir: dir,
		BackendHandler: func(w http.ResponseWriter, r *http.Request) {
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Errorf("read upstream request: %v", err)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(body)
		},
	})

	for _, tc := range []struct {
		name     string
		path     string
		message  string
		bodyJSON string
	}{
		{
			name:     "Struct object",
			path:     "/v3/open",
			message:  "google.protobuf.Struct",
			bodyJSON: `{"provider":"acme","flags":[true,3],"config":{"mode":"fast"}}`,
		},
		{
			name:     "Value array",
			path:     "/v3/value",
			message:  "google.protobuf.Value",
			bodyJSON: `["opaque",7,false,null,{"nested":true}]`,
		},
		{
			name:     "Value untagged union",
			path:     "/v3/union",
			message:  "google.protobuf.Value",
			bodyJSON: `{"kind":"custom","priority":3}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			contract, ok := h.Bundle.Lookup(tc.path, http.MethodPost, "2026-10-13")
			if !ok {
				t.Fatalf("contract for %s not found", tc.path)
			}
			if got := contract.RequestMessage(); got != tc.message {
				t.Fatalf("request_message = %q, want %q", got, tc.message)
			}

			md, err := h.Bundle.Message(contract.RequestMessage())
			if err != nil {
				t.Fatalf("resolve request message: %v", err)
			}
			requestMessage := dynamicpb.NewMessage(md)
			if err := protojson.Unmarshal([]byte(tc.bodyJSON), requestMessage); err != nil {
				t.Fatalf("build request message: %v", err)
			}
			requestBytes, err := proto.Marshal(requestMessage)
			if err != nil {
				t.Fatalf("marshal request: %v", err)
			}

			req, err := http.NewRequest(http.MethodPost, h.Proxy.URL+tc.path, strings.NewReader(string(requestBytes)))
			if err != nil {
				t.Fatalf("new request: %v", err)
			}
			req.Header.Set("Content-Type", "application/protobuf")
			req.Header.Set("X-Api-Contract-Version", "2026-10-13")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("proxy request: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200", resp.StatusCode)
			}

			captured := h.Backend.Last()
			assertJSONEqual(t, captured.Body, []byte(tc.bodyJSON))

			responseBytes, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatalf("read response: %v", err)
			}
			responseMessage := dynamicpb.NewMessage(md)
			if err := proto.Unmarshal(responseBytes, responseMessage); err != nil {
				t.Fatalf("response is not %s protobuf: %v", tc.message, err)
			}
			responseJSON, err := protojson.Marshal(responseMessage)
			if err != nil {
				t.Fatalf("marshal response as JSON: %v", err)
			}
			assertJSONEqual(t, responseJSON, []byte(tc.bodyJSON))
		})
	}
}

func assertJSONEqual(t *testing.T, got, want []byte) {
	t.Helper()
	var gotValue, wantValue any
	if err := json.Unmarshal(got, &gotValue); err != nil {
		t.Fatalf("got invalid JSON %q: %v", got, err)
	}
	if err := json.Unmarshal(want, &wantValue); err != nil {
		t.Fatalf("test expected invalid JSON %q: %v", want, err)
	}
	if !reflect.DeepEqual(gotValue, wantValue) {
		t.Errorf("JSON = %s, want %s", got, want)
	}
}

func TestPipelineProxiesEndToEnd(t *testing.T) {
	// Claim: generator → bundle → negotiate → adapter → upstream → adapter
	// integrates end-to-end against a bundle built from a real OpenAPI doc.
	dir := generatedBundleDir(t)
	h := Spawn(t, SpawnOpts{
		BundleDir: dir,
		BackendHandler: func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost || r.URL.Path != "/v3/echo" {
				t.Errorf("upstream got %s %s, want POST /v3/echo", r.Method, r.URL.Path)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"text":"hello-back","ok":true}`)
		},
	})

	contract, ok := h.Bundle.Contract("2026-05-17")
	if !ok {
		t.Fatal("generated contract not found")
	}
	reqMD, err := h.Bundle.Message(contract.RequestMessage())
	if err != nil {
		t.Fatalf("resolve request message: %v", err)
	}
	reqMsg := dynamicpb.NewMessage(reqMD)
	reqMsg.Set(reqMD.Fields().ByName("text"), protoreflect.ValueOfString("hi"))
	reqMsg.Set(reqMD.Fields().ByName("count"), protoreflect.ValueOfInt64(42))
	reqBytes, err := proto.Marshal(reqMsg)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}

	req, _ := http.NewRequest(http.MethodPost, h.Proxy.URL+"/v3/echo", strings.NewReader(string(reqBytes)))
	req.Header.Set("Content-Type", "application/protobuf")
	req.Header.Set("X-Api-Contract-Version", "2026-05-17")
	req.Header.Set("Authorization", "Bearer t0ken")
	req.Header.Set("traceparent", "tp-9")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if resp.Header.Get("Content-Type") != "application/protobuf" {
		t.Errorf("Content-Type = %q", resp.Header.Get("Content-Type"))
	}
	if resp.Header.Get("X-Wavefront-Contract-Version") != "2026-05-17" {
		t.Errorf("X-Wavefront-Contract-Version = %q", resp.Header.Get("X-Wavefront-Contract-Version"))
	}

	capt := h.Backend.Last()
	if got := capt.Header.Get("Authorization"); got != "Bearer t0ken" {
		t.Errorf("Authorization not forwarded untouched: %q", got)
	}
	if got := capt.Header.Get("traceparent"); got != "tp-9" {
		t.Errorf("traceparent not forwarded untouched: %q", got)
	}
	var body map[string]any
	if err := json.Unmarshal(capt.Body, &body); err != nil {
		t.Errorf("upstream body not JSON: %v", err)
	}
	if got, _ := body["text"].(string); got != "hi" {
		t.Errorf("upstream saw text = %q", got)
	}
	// proto3 JSON maps int64 to a string — an important nuance this pipeline
	// test deliberately exercises end to end.
	if got, _ := body["count"].(string); got != "42" {
		t.Errorf("upstream saw count = %q (proto3 int64 JSON should be a string)", got)
	}

	out, _ := io.ReadAll(resp.Body)
	respMD, err := h.Bundle.Message(contract.ResponseMessage())
	if err != nil {
		t.Fatalf("resolve response message: %v", err)
	}
	reply := dynamicpb.NewMessage(respMD)
	if err := proto.Unmarshal(out, reply); err != nil {
		t.Fatalf("client response not protobuf EchoReply: %v", err)
	}
	if got := reply.Get(respMD.Fields().ByName("text")).String(); got != "hello-back" {
		t.Errorf("reply text = %q", got)
	}
	if got := reply.Get(respMD.Fields().ByName("ok")).Bool(); !got {
		t.Errorf("reply ok = %v, want true", got)
	}
}
