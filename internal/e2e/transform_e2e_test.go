package e2e

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

// upstreamJSONEquals decodes `got` and compares against `want` semantically.
// protojson.Marshal deliberately injects randomized whitespace between
// tokens, so exact-byte comparison against its output is flaky on CI.
func upstreamJSONEquals(t *testing.T, got string, want map[string]any) {
	t.Helper()
	var have map[string]any
	if err := json.Unmarshal([]byte(got), &have); err != nil {
		t.Fatalf("upstream body not JSON: %v (raw=%q)", err, got)
	}
	if !reflect.DeepEqual(have, want) {
		t.Errorf("upstream body wrong: got=%v want=%v", have, want)
	}
}

func e2ePing(t *testing.T, b *bundle.Bundle) []byte {
	t.Helper()
	md, err := b.Message("acme.v1.Ping")
	if err != nil {
		t.Fatalf("Ping: %v", err)
	}
	m := dynamicpb.NewMessage(md)
	m.Set(md.Fields().ByName("text"), protoreflect.ValueOfString("hi"))
	m.Set(md.Fields().ByName("n"), protoreflect.ValueOfInt32(7))
	raw, err := proto.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return raw
}

func e2ePingTextOnly(t *testing.T, b *bundle.Bundle) []byte {
	t.Helper()
	md, err := b.Message("acme.v1.Ping")
	if err != nil {
		t.Fatalf("Ping: %v", err)
	}
	m := dynamicpb.NewMessage(md)
	m.Set(md.Fields().ByName("text"), protoreflect.ValueOfString("hi"))
	// n deliberately unset — proto3 zero value will be omitted by protojson.
	raw, err := proto.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return raw
}

func TestTransformE2EBothDirections(t *testing.T) {
	dir := bundletest.Dir(t, "")
	bundletest.WriteResolution(t, dir, `version: 1
overrides:
  - contract_version: "2024-11"
    transform:
      request:
        - rename: { from: text, to: message }
        - coerce: { field: n, to: string }
      response:
        - rename: { from: msg, to: text }
`)
	b, err := bundle.Load(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	var mu sync.Mutex
	var saw string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		saw = string(body)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"msg":"hello-back"}`)
	}))
	defer up.Close()

	fs := front(t, b, cfg(up.URL))
	req, _ := http.NewRequest(http.MethodPost, fs.URL, bytes.NewReader(e2ePing(t, b)))
	req.Header.Set("X-Api-Contract-Version", "2024-11")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	mu.Lock()
	got := saw
	mu.Unlock()
	// proto3 JSON renders int32 as a number; coerce n->string makes it "7"
	upstreamJSONEquals(t, got, map[string]any{"message": "hi", "n": "7"})
	md, merr := b.Message("acme.v1.Pong")
	if merr != nil {
		t.Fatalf("Pong: %v", merr)
	}
	out := dynamicpb.NewMessage(md)
	raw, _ := io.ReadAll(resp.Body)
	if err := proto.Unmarshal(raw, out); err != nil {
		t.Fatalf("client body not Pong: %v", err)
	}
	if g := out.Get(md.Fields().ByName("text")).String(); g != "hello-back" {
		t.Errorf("client text=%q want hello-back", g)
	}
}

func TestNoStanzasPassthrough(t *testing.T) {
	b, err := bundle.Load(bundletest.Dir(t, "")) // no stanzas
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	var mu sync.Mutex
	var saw string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		saw = string(body)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"text":"x"}`)
	}))
	defer up.Close()

	fs := front(t, b, cfg(up.URL))
	req, _ := http.NewRequest(http.MethodPost, fs.URL, bytes.NewReader(e2ePing(t, b)))
	req.Header.Set("X-Api-Contract-Version", "2024-11")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	mu.Lock()
	got := saw
	mu.Unlock()
	if resp.StatusCode != 200 {
		t.Fatalf("passthrough: status=%d want 200", resp.StatusCode)
	}
	upstreamJSONEquals(t, got, map[string]any{"text": "hi", "n": float64(7)})
}

func TestRequestTransformFailureIs422E2E(t *testing.T) {
	// Coerce text (a non-numeric string) to number: passes descriptor
	// cross-check at load (text exists in acme.v1.Ping), but fails at
	// runtime because "hi" cannot be parsed as a number → 422.
	dir := bundletest.Dir(t, "")
	bundletest.WriteResolution(t, dir, `version: 1
overrides:
  - contract_version: "2024-11"
    transform:
      request:
        - coerce: { field: text, to: number }
`)
	b, err := bundle.Load(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	up := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		t.Error("upstream must not be called on request transform failure")
	}))
	defer up.Close()
	fs := front(t, b, cfg(up.URL))
	req, _ := http.NewRequest(http.MethodPost, fs.URL, bytes.NewReader(e2ePing(t, b)))
	req.Header.Set("X-Api-Contract-Version", "2024-11")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("want 422, got %d", resp.StatusCode)
	}
	if resp.Header.Get("X-Wavefront-Error") != "transform_failed" {
		t.Errorf("X-Wavefront-Error=%q", resp.Header.Get("X-Wavefront-Error"))
	}
}

func TestRenameSourceAbsentRequestIs422E2E(t *testing.T) {
	// Bundle: rename `n` -> `renamed_n`. `n` exists on the descriptor
	// (passes load-time cross-check) but the request has it unset (proto3
	// zero-value omitted by protojson), so runtime rename source is absent
	// -> 422 transform_failed.
	dir := bundletest.Dir(t, "")
	bundletest.WriteResolution(t, dir, `version: 1
overrides:
  - contract_version: "2024-11"
    transform:
      request:
        - rename: { from: n, to: renamed_n }
`)
	b, err := bundle.Load(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	up := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		t.Error("upstream must not be called on request transform failure")
	}))
	defer up.Close()
	fs := front(t, b, cfg(up.URL))
	req, _ := http.NewRequest(http.MethodPost, fs.URL, bytes.NewReader(e2ePingTextOnly(t, b)))
	req.Header.Set("X-Api-Contract-Version", "2024-11")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("want 422, got %d", resp.StatusCode)
	}
	if resp.Header.Get("X-Wavefront-Error") != "transform_failed" {
		t.Errorf("X-Wavefront-Error=%q want transform_failed", resp.Header.Get("X-Wavefront-Error"))
	}
}

const nestedArrayVersions = `version: 1
contracts:
  - contract_version: "2024-12"
    route: /v3/echo
    method: POST
    request_message: acme.v1.PingV2
    response_message: acme.v1.Pong
`

func TestNestedArrayE2E(t *testing.T) {
	dir := bundletest.Dir(t, nestedArrayVersions)
	bundletest.WriteResolution(t, dir, `version: 1
overrides:
  - contract_version: "2024-12"
    transform:
      request:
        - rename: { from: "items[].text", to: "items[].label" }
        - coerce: { field: "items[].id", to: string }
        - default: { field: meta.locale, value: en-US }
`)
	b, err := bundle.Load(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	md, err := b.Message("acme.v1.PingV2")
	if err != nil {
		t.Fatalf("PingV2: %v", err)
	}
	m := dynamicpb.NewMessage(md)
	itemsField := md.Fields().ByName("items")
	itemMD := itemsField.Message()
	list := m.Mutable(itemsField).List()
	for _, v := range []struct {
		id   int32
		text string
	}{{1, "alpha"}, {2, "beta"}} {
		el := dynamicpb.NewMessage(itemMD)
		el.Set(itemMD.Fields().ByName("id"), protoreflect.ValueOfInt32(v.id))
		el.Set(itemMD.Fields().ByName("text"), protoreflect.ValueOfString(v.text))
		list.Append(protoreflect.ValueOfMessage(el))
	}
	raw, err := proto.Marshal(m)
	if err != nil {
		t.Fatalf("marshal PingV2: %v", err)
	}

	var mu sync.Mutex
	var saw string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		saw = string(body)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"text":"server-ok"}`)
	}))
	defer up.Close()

	fs := front(t, b, cfg(up.URL))
	req, _ := http.NewRequest(http.MethodPost, fs.URL, bytes.NewReader(raw))
	req.Header.Set("X-Api-Contract-Version", "2024-12")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}

	mu.Lock()
	got := saw
	mu.Unlock()
	upstreamJSONEquals(t, got, map[string]any{
		"items": []any{
			map[string]any{"id": "1", "label": "alpha"},
			map[string]any{"id": "2", "label": "beta"},
		},
		"meta": map[string]any{"locale": "en-US"},
	})

	pongMD, _ := b.Message("acme.v1.Pong")
	out := dynamicpb.NewMessage(pongMD)
	rawResp, _ := io.ReadAll(resp.Body)
	if err := proto.Unmarshal(rawResp, out); err != nil {
		t.Fatalf("client body not Pong: %v", err)
	}
	if g := out.Get(pongMD.Fields().ByName("text")).String(); g != "server-ok" {
		t.Errorf("client got text=%q", g)
	}
}
