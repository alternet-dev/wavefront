package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"

	"github.com/alternet-dev/wavefront/internal/bundle"
	"github.com/alternet-dev/wavefront/internal/bundletest"
)

// buildChainBundle builds a two-layer bundle:
//
//	"2024-01": transform request (rename text→message) + target "2024-12"
//	"2024-12": terminal, transform request (default {field: version, value: "v12"})
//
// With chained application the upstream should see both ops:
//
//	{"message":"hi","version":"v12"}
//
// Without chaining (current single-hop): only 2024-01's ops apply; the
// upstream sees {"message":"hi"} (no version field), so the version assertion
// fails and the test catches the regression.
//
// Response: both links have no response ops; upstream returns {"text":"pong"}
// directly, which the adapter decodes into Pong{text:"pong"}.
func buildChainBundle(t *testing.T) *bundle.Bundle {
	t.Helper()
	fds := bundletest.FDSBytes(t)
	mk := func(cv string) bundletest.Layer {
		return bundletest.Layer{
			Name:        cv,
			Descriptors: fds,
			OpenAPI:     bundletest.ValidOpenAPI,
			Versions: "version: 1\ncontracts:\n  - contract_version: \"" + cv + "\"\n" +
				"    route: /v3/echo\n    method: POST\n" +
				"    request_message: acme.v1.Ping\n    response_message: acme.v1.Pong\n",
		}
	}
	dir := bundletest.MultiDir(t, mk("2024-01"), mk("2024-12"))
	// 2024-01 renames the external "text" field to internal "message".
	// 2024-12 is the terminal: injects a "version" default on the internal shape
	// (default.field in request is internal-targeting, so no descriptor check).
	bundletest.WriteResolution(t, dir, `version: 1
overrides:
  - contract_version: "2024-01"
    transform:
      request:
        - rename: { from: text, to: message }
      target: "2024-12"
  - contract_version: "2024-12"
    transform:
      request:
        - default: { field: version, value: "v12" }
`)
	b, err := bundle.Load(dir)
	if err != nil {
		t.Fatalf("build chain bundle: %v", err)
	}
	return b
}

// TestChainAppliesRequestOpsInOrder verifies that a request negotiated to the
// oldest version of a chained bundle is transformed link-by-link:
//   - 2024-01 renames text → message
//   - 2024-12 (terminal) injects default version="v12"
//
// The upstream must receive both fields. The client receives a valid Pong
// with text="pong" (upstream returns {"text":"pong"}, no response ops needed).
func TestChainAppliesRequestOpsInOrder(t *testing.T) {
	b := buildChainBundle(t)

	var mu sync.Mutex
	var sawBody map[string]any

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var got map[string]any
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Errorf("upstream body not JSON: %v (raw=%q)", err, raw)
		}
		mu.Lock()
		sawBody = got
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"text":"pong"}`)
	}))
	defer up.Close()

	s := New(testCfg(up.URL))
	s.SetBundle(b)
	front := httptest.NewServer(s.DataHandler())
	defer front.Close()

	// Build a Ping proto with text="hi"
	md, err := b.Message("acme.v1.Ping")
	if err != nil {
		t.Fatalf("resolve Ping: %v", err)
	}
	m := dynamicpb.NewMessage(md)
	m.Set(md.Fields().ByName("text"), protoreflect.ValueOfString("hi"))
	raw, err := proto.Marshal(m)
	if err != nil {
		t.Fatalf("marshal Ping: %v", err)
	}

	req, _ := http.NewRequest(http.MethodPost, front.URL+"/v3/echo", bytes.NewReader(raw))
	req.Header.Set("X-Api-Contract-Version", "2024-01")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d, body=%q", resp.StatusCode, body)
	}

	// Both chain link request ops must have applied:
	//   2024-01: text → message
	//   2024-12: inject default version="v12"
	mu.Lock()
	got := sawBody
	mu.Unlock()
	want := map[string]any{"message": "hi", "version": "v12"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("upstream saw %v, want %v", got, want)
	}

	// Client receives a valid Pong{text:"pong"} (no response ops; upstream
	// returns {"text":"pong"} which the adapter decodes directly).
	out, _ := io.ReadAll(resp.Body)
	pongMD, _ := b.Message("acme.v1.Pong")
	pong := dynamicpb.NewMessage(pongMD)
	if err := proto.Unmarshal(out, pong); err != nil {
		t.Fatalf("response body not Pong proto: %v", err)
	}
	if gotText := pong.Get(pongMD.Fields().ByName("text")).String(); gotText != "pong" {
		t.Errorf("decoded Pong.text = %q, want pong", gotText)
	}
}
