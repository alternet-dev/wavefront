package e2e

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
	t.Helper()
	in := filepath.Join(t.TempDir(), "openapi.json")
	if err := os.WriteFile(in, []byte(sampleOpenAPI), 0o600); err != nil {
		t.Fatalf("write openapi: %v", err)
	}
	out := t.TempDir()
	if err := bundlegen.Add(in, out); err != nil {
		t.Fatalf("bundlegen.Add: %v", err)
	}
	return out
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
