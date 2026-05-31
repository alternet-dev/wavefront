package e2e

import (
	"bytes"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"

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
