package e2e

import (
	"bytes"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"

	"github.com/alternet-dev/wavefront/internal/bundlegen"
	"github.com/alternet-dev/wavefront/internal/bundletest"
)

// buildSplitRouteBundleDir builds a two-layer bundle where each version binds
// a DIFFERENT (path, method):
//
//	"2024-11": POST /v3/echo  (acme.v1.Ping → acme.v1.Pong)
//	"2025-03": GET  /v3/echo  (acme.v1.Ping → acme.v1.Pong)
//
// LookupRoute("/v3/echo", "POST") finds the 2024-11 contract; LookupRoute
// ("/v3/echo", "GET") finds the 2025-03 contract. A request that pairs the
// wrong (path, method) with a version that exists but does not bind it
// must surface unsupported_contract_version (not unknown_route).
func buildSplitRouteBundleDir(t *testing.T) string {
	t.Helper()
	fds := bundletest.FDSBytes(t)
	const v1 = `version: 1
contracts:
  - contract_version: "2024-11"
    route: /v3/echo
    method: POST
    request_message: acme.v1.Ping
    response_message: acme.v1.Pong
`
	const v2 = `version: 1
contracts:
  - contract_version: "2025-03"
    route: /v3/echo
    method: GET
    request_message: acme.v1.Ping
    response_message: acme.v1.Pong
`
	return bundletest.MultiDir(t,
		bundletest.Layer{Name: "2024-11", Descriptors: fds, OpenAPI: bundletest.ValidOpenAPI, Versions: v1},
		bundletest.Layer{Name: "2025-03", Descriptors: fds, OpenAPI: bundletest.ValidOpenAPI, Versions: v2},
	)
}

// multiRouteOpenAPI is a 3-route OpenAPI: POST /v3/items, GET /v3/items/{id},
// DELETE /v3/items/{id}. The generator emits one contract per operation under
// a single contract_version layer; the proxy must route each (path, method)
// pair to its own (request_message, response_message).
const multiRouteOpenAPI = `{
  "openapi": "3.0.0",
  "info": {"title": "acme", "version": "2026-05-17"},
  "paths": {
    "/v3/items": {
      "post": {
        "operationId": "createItem",
        "requestBody": {"content": {"application/json": {"schema": {"$ref": "#/components/schemas/CreateItemRequest"}}}},
        "responses": {"200": {"content": {"application/json": {"schema": {"$ref": "#/components/schemas/Item"}}}}}
      }
    },
    "/v3/items/{id}": {
      "get": {
        "operationId": "getItem",
        "requestBody": {"content": {"application/json": {"schema": {"$ref": "#/components/schemas/GetItemRequest"}}}},
        "responses": {"200": {"content": {"application/json": {"schema": {"$ref": "#/components/schemas/Item"}}}}}
      },
      "delete": {
        "operationId": "deleteItem",
        "requestBody": {"content": {"application/json": {"schema": {"$ref": "#/components/schemas/DeleteItemRequest"}}}},
        "responses": {"200": {"content": {"application/json": {"schema": {"$ref": "#/components/schemas/DeleteItemReply"}}}}}
      }
    }
  },
  "components": {
    "schemas": {
      "CreateItemRequest": {"type": "object", "properties": {
        "name": {"type": "string"}
      }},
      "GetItemRequest": {"type": "object", "properties": {
        "id": {"type": "string"}
      }},
      "DeleteItemRequest": {"type": "object", "properties": {
        "id": {"type": "string"}
      }},
      "Item": {"type": "object", "properties": {
        "id": {"type": "string"},
        "name": {"type": "string"}
      }},
      "DeleteItemReply": {"type": "object", "properties": {
        "deleted": {"type": "boolean"}
      }}
    }
  }
}`

// multiRouteBundleDir builds a multi-route bundle from multiRouteOpenAPI via
// the real generator and returns the bundle directory.
func multiRouteBundleDir(t *testing.T) string {
	t.Helper()
	in := filepath.Join(t.TempDir(), "openapi.json")
	if err := os.WriteFile(in, []byte(multiRouteOpenAPI), 0o600); err != nil {
		t.Fatalf("write openapi: %v", err)
	}
	out := t.TempDir()
	if err := bundlegen.Add(in, out); err != nil {
		t.Fatalf("bundlegen.Add: %v", err)
	}
	return out
}

// TestMultiRouteProxiesEachOperationDistinctly is the central claim of
// Chunk 5.2: a multi-route bundle's contracts are dispatched by full
// (path, method, contract_version) — not by version alone. Each of the three
// requests must reach a distinct upstream handler under the same contract
// version, with its own request_message decoded and response_message encoded.
func TestMultiRouteProxiesEachOperationDistinctly(t *testing.T) {
	dir := multiRouteBundleDir(t)

	var (
		mu       sync.Mutex
		hitCount = map[string]int{}
	)

	handler := func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hitCount[r.Method+" "+r.URL.Path]++
		mu.Unlock()
		switch r.Method + " " + r.URL.Path {
		case "POST /v3/items":
			// Item response: id + name
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"new-id","name":"created"}`)
		case "GET /v3/items/{id}":
			// Item response
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"id-123","name":"fetched"}`)
		case "DELETE /v3/items/{id}":
			// DeleteItemReply: deleted=true
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"deleted":true}`)
		default:
			t.Errorf("unexpected upstream request: %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected", http.StatusInternalServerError)
		}
	}

	h := Spawn(t, SpawnOpts{
		BundleDir:      dir,
		BackendHandler: handler,
	})

	const version = "2026-05-17"

	// Helper to build a protobuf body for a contract's request_message.
	requestBody := func(t *testing.T, msgName string, setFields func(m *dynamicpb.Message, md protoreflect.MessageDescriptor)) []byte {
		t.Helper()
		md, err := h.Bundle.Message(msgName)
		if err != nil {
			t.Fatalf("resolve %s: %v", msgName, err)
		}
		m := dynamicpb.NewMessage(md)
		if setFields != nil {
			setFields(m, md)
		}
		raw, err := proto.Marshal(m)
		if err != nil {
			t.Fatalf("marshal %s: %v", msgName, err)
		}
		return raw
	}

	// 1. POST /v3/items: should land on the POST handler and return an Item.
	postCreate, ok := h.Bundle.LookupRoute("/v3/items", http.MethodPost)
	if !ok {
		t.Fatal("bundle has no POST /v3/items")
	}
	postBody := requestBody(t, postCreate.RequestMessage(), func(m *dynamicpb.Message, md protoreflect.MessageDescriptor) {
		m.Set(md.Fields().ByName("name"), protoreflect.ValueOfString("widget"))
	})
	postReq, _ := http.NewRequest(http.MethodPost, h.Proxy.URL+"/v3/items", bytes.NewReader(postBody))
	postReq.Header.Set("Content-Type", "application/protobuf")
	postReq.Header.Set("X-Api-Contract-Version", version)
	postResp, err := http.DefaultClient.Do(postReq)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer postResp.Body.Close()
	if postResp.StatusCode != http.StatusOK {
		t.Fatalf("POST status = %d, want 200", postResp.StatusCode)
	}
	postOut, _ := io.ReadAll(postResp.Body)
	itemMD, err := h.Bundle.Message(postCreate.ResponseMessage())
	if err != nil {
		t.Fatalf("resolve Item: %v", err)
	}
	item := dynamicpb.NewMessage(itemMD)
	if err := proto.Unmarshal(postOut, item); err != nil {
		t.Fatalf("POST response not Item proto: %v (raw=%q)", err, postOut)
	}
	if got := item.Get(itemMD.Fields().ByName("name")).String(); got != "created" {
		t.Errorf("POST Item.name = %q, want %q", got, "created")
	}

	// 2. GET /v3/items/{id}: should land on the GET handler and return an Item.
	getCreate, ok := h.Bundle.LookupRoute("/v3/items/{id}", http.MethodGet)
	if !ok {
		t.Fatal("bundle has no GET /v3/items/{id}")
	}
	getBody := requestBody(t, getCreate.RequestMessage(), func(m *dynamicpb.Message, md protoreflect.MessageDescriptor) {
		m.Set(md.Fields().ByName("id"), protoreflect.ValueOfString("id-123"))
	})
	getReq, _ := http.NewRequest(http.MethodGet, h.Proxy.URL+"/v3/items/{id}", bytes.NewReader(getBody))
	getReq.Header.Set("Content-Type", "application/protobuf")
	getReq.Header.Set("X-Api-Contract-Version", version)
	getResp, err := http.DefaultClient.Do(getReq)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer getResp.Body.Close()
	if getResp.StatusCode != http.StatusOK {
		t.Fatalf("GET status = %d, want 200", getResp.StatusCode)
	}
	getOut, _ := io.ReadAll(getResp.Body)
	getItemMD, err := h.Bundle.Message(getCreate.ResponseMessage())
	if err != nil {
		t.Fatalf("resolve GET Item: %v", err)
	}
	getItem := dynamicpb.NewMessage(getItemMD)
	if err := proto.Unmarshal(getOut, getItem); err != nil {
		t.Fatalf("GET response not Item proto: %v", err)
	}
	if got := getItem.Get(getItemMD.Fields().ByName("name")).String(); got != "fetched" {
		t.Errorf("GET Item.name = %q, want %q", got, "fetched")
	}

	// 3. DELETE /v3/items/{id}: should land on the DELETE handler and return a
	//    DeleteItemReply (a different response_message from the GET contract).
	delContract, ok := h.Bundle.LookupRoute("/v3/items/{id}", http.MethodDelete)
	if !ok {
		t.Fatal("bundle has no DELETE /v3/items/{id}")
	}
	delBody := requestBody(t, delContract.RequestMessage(), func(m *dynamicpb.Message, md protoreflect.MessageDescriptor) {
		m.Set(md.Fields().ByName("id"), protoreflect.ValueOfString("id-123"))
	})
	delReq, _ := http.NewRequest(http.MethodDelete, h.Proxy.URL+"/v3/items/{id}", bytes.NewReader(delBody))
	delReq.Header.Set("Content-Type", "application/protobuf")
	delReq.Header.Set("X-Api-Contract-Version", version)
	delResp, err := http.DefaultClient.Do(delReq)
	if err != nil {
		t.Fatalf("DELETE: %v", err)
	}
	defer delResp.Body.Close()
	if delResp.StatusCode != http.StatusOK {
		t.Fatalf("DELETE status = %d, want 200", delResp.StatusCode)
	}
	delOut, _ := io.ReadAll(delResp.Body)
	delMD, err := h.Bundle.Message(delContract.ResponseMessage())
	if err != nil {
		t.Fatalf("resolve DeleteItemReply: %v", err)
	}
	delReply := dynamicpb.NewMessage(delMD)
	if err := proto.Unmarshal(delOut, delReply); err != nil {
		t.Fatalf("DELETE response not DeleteItemReply proto: %v", err)
	}
	if got := delReply.Get(delMD.Fields().ByName("deleted")).Bool(); !got {
		t.Errorf("DELETE reply.deleted = false, want true")
	}

	// Every upstream handler must have been hit exactly once: a route gate
	// regression that collapsed every (path, method) to the same contract
	// would either fail the per-handler decode/encode checks above or skew
	// these counts.
	mu.Lock()
	defer mu.Unlock()
	wantHits := map[string]int{
		"POST /v3/items":        1,
		"GET /v3/items/{id}":    1,
		"DELETE /v3/items/{id}": 1,
	}
	for k, want := range wantHits {
		if got := hitCount[k]; got != want {
			t.Errorf("upstream %s hit count = %d, want %d", k, got, want)
		}
	}
}

// TestMultiRouteVersionKnownButRouteUnboundReturns400 covers the edge case the
// task names: a request to (path, method) that IS bound in the bundle (route
// gate passes), but at a DIFFERENT version. The named contract-version
// exists in the bundle yet does not bind this (path, method). Expected
// response: unsupported_contract_version (400) — the route is known overall
// but the named version does not serve it, so the version-mismatch story is
// more informative for the client than unknown_route.
func TestMultiRouteVersionKnownButRouteUnboundReturns400(t *testing.T) {
	// Build a hand-constructed two-layer bundle where two versions each bind a
	// DIFFERENT (path, method): 2024-11 binds POST /v3/echo and 2025-03 binds
	// GET /v3/echo. A request to GET /v3/echo with X-Api-Contract-Version:
	// 2024-11 must surface unsupported_contract_version: the route gate sees
	// some version binds GET /v3/echo, but 2024-11 does not.
	dir := buildSplitRouteBundleDir(t)

	h := Spawn(t, SpawnOpts{BundleDir: dir})

	req, _ := http.NewRequest(http.MethodGet, h.Proxy.URL+"/v3/echo", strings.NewReader(""))
	req.Header.Set("X-Api-Contract-Version", "2024-11")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (version exists but does not bind this route)", resp.StatusCode)
	}
	if got := resp.Header.Get("X-Wavefront-Error"); got != "unsupported_contract_version" {
		t.Errorf("X-Wavefront-Error = %q, want unsupported_contract_version", got)
	}
	// Per the existing pre-negotiate echo convention, the response header
	// carries the raw client-sent value so the caller can correlate.
	if got := resp.Header.Get("X-Wavefront-Contract-Version"); got != "2024-11" {
		t.Errorf("X-Wavefront-Contract-Version = %q, want %q", got, "2024-11")
	}
}

// TestMultiRouteUnboundPathStill404 confirms ordering: the route gate at step 2
// fires before negotiate. A request to a path no contract binds returns
// unknown_route 404 even when the contract-version header names a perfectly
// valid version in the bundle. This is the no-regression guarantee versus
// the existing route-gate behavior.
func TestMultiRouteUnboundPathStill404(t *testing.T) {
	dir := multiRouteBundleDir(t)
	h := Spawn(t, SpawnOpts{BundleDir: dir})

	req, _ := http.NewRequest(http.MethodGet, h.Proxy.URL+"/nope/never/bound", strings.NewReader(""))
	req.Header.Set("X-Api-Contract-Version", "2026-05-17") // valid version in this bundle
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404 (route gate fires before negotiate)", resp.StatusCode)
	}
	if got := resp.Header.Get("X-Wavefront-Error"); got != "unknown_route" {
		t.Errorf("X-Wavefront-Error = %q, want unknown_route", got)
	}
}
