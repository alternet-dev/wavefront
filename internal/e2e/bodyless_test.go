package e2e

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/dynamicpb"

	"github.com/alternet-dev/wavefront/internal/bundlegen"
)

// bodylessOpenAPI exercises the two read-side shapes #105 unblocks under one
// contract version: a GET with no requestBody returning a bodied Item, and a
// DELETE with no requestBody whose only declared response is 204 (no body in
// either direction). The generator binds the synthetic Empty to every bodyless
// side; the proxy must then send a clean bodyless upstream request and handle
// the empty response.
const bodylessOpenAPI = `{
  "openapi": "3.0.0",
  "info": {"title": "acme", "version": "2026-09-01"},
  "paths": {
    "/v3/items/{id}": {
      "get": {
        "operationId": "getItem",
        "responses": {"200": {"content": {"application/json": {"schema": {"$ref": "#/components/schemas/Item"}}}}}
      },
      "delete": {
        "operationId": "deleteItem",
        "responses": {"204": {"description": "deleted"}}
      }
    }
  },
  "components": {
    "schemas": {
      "Item": {"type": "object", "properties": {
        "id": {"type": "string"},
        "name": {"type": "string"}
      }}
    }
  }
}`

// bodylessBundleDir builds a bundle from bodylessOpenAPI via the real generator.
func bodylessBundleDir(t *testing.T) string {
	t.Helper()
	in := filepath.Join(t.TempDir(), "openapi.json")
	if err := os.WriteFile(in, []byte(bodylessOpenAPI), 0o600); err != nil {
		t.Fatalf("write openapi: %v", err)
	}
	out := t.TempDir()
	if err := bundlegen.Add(in, out, false); err != nil {
		t.Fatalf("bundlegen.Add: %v", err)
	}
	return out
}

// TestBodylessGETSendsCleanUpstreamRequest: a GET whose request_message is the
// synthetic Empty must reach the upstream with no body and no Content-Type —
// not a `{}` body with application/json, which is the pre-#105 bug. The client
// still receives a protobuf-encoded response from the bodied 200.
func TestBodylessGETSendsCleanUpstreamRequest(t *testing.T) {
	dir := bodylessBundleDir(t)
	h := Spawn(t, SpawnOpts{
		BundleDir: dir,
		BackendHandler: func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"id-1","name":"fetched"}`)
		},
	})

	req, _ := http.NewRequest(http.MethodGet, h.Proxy.URL+"/v3/items/{id}", nil)
	req.Header.Set("X-Api-Contract-Version", "2026-09-01")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	up := h.Backend.Last()
	if up.Method != http.MethodGet {
		t.Errorf("upstream method = %q, want GET", up.Method)
	}
	if len(up.Body) != 0 {
		t.Errorf("upstream body = %q, want empty (bodyless GET must not send {})", up.Body)
	}
	if ct := up.Header.Get("Content-Type"); ct != "" {
		t.Errorf("upstream Content-Type = %q, want absent (no request body)", ct)
	}

	getC, ok := h.Bundle.LookupRoute("/v3/items/{id}", http.MethodGet)
	if !ok {
		t.Fatal("bundle has no GET /v3/items/{id}")
	}
	out, _ := io.ReadAll(resp.Body)
	itemMD, err := h.Bundle.Message(getC.ResponseMessage())
	if err != nil {
		t.Fatalf("resolve response_message: %v", err)
	}
	item := dynamicpb.NewMessage(itemMD)
	if err := proto.Unmarshal(out, item); err != nil {
		t.Fatalf("response not a protobuf message: %v (raw=%q)", err, out)
	}
	if got := item.Get(itemMD.Fields().ByName("name")).String(); got != "fetched" {
		t.Errorf("response name = %q, want fetched", got)
	}
}

// TestBodylessDELETEReturns204: a DELETE whose request_message and
// response_message are both the synthetic Empty must send a clean bodyless
// upstream DELETE and relay the upstream 204 to the client with no body.
func TestBodylessDELETEReturns204(t *testing.T) {
	dir := bodylessBundleDir(t)
	h := Spawn(t, SpawnOpts{
		BundleDir: dir,
		BackendHandler: func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		},
	})

	req, _ := http.NewRequest(http.MethodDelete, h.Proxy.URL+"/v3/items/{id}", nil)
	req.Header.Set("X-Api-Contract-Version", "2026-09-01")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("DELETE: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", resp.StatusCode)
	}
	out, _ := io.ReadAll(resp.Body)
	if len(out) != 0 {
		t.Errorf("client body = %q, want empty for 204", out)
	}

	up := h.Backend.Last()
	if up.Method != http.MethodDelete {
		t.Errorf("upstream method = %q, want DELETE", up.Method)
	}
	if len(up.Body) != 0 {
		t.Errorf("upstream body = %q, want empty (bodyless DELETE)", up.Body)
	}
	if ct := up.Header.Get("Content-Type"); ct != "" {
		t.Errorf("upstream Content-Type = %q, want absent", ct)
	}
}
