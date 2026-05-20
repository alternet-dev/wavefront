package e2e

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"

	"github.com/alternet-dev/wavefront/internal/bundle"
	"github.com/alternet-dev/wavefront/internal/bundletest"
)

const stanzaVersions = `version: 1
contracts:
  - contract_version: "2024-11"
    route: /v3/echo
    method: POST
    request_message: acme.v1.Ping
    response_message: acme.v1.Pong
    request:
      - rename: { from: text, to: message }
      - coerce: { field: n, to: string }
    response:
      - rename: { from: msg, to: text }
`

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

func TestTransformE2EBothDirections(t *testing.T) {
	b, err := bundle.Load(bundletest.Dir(t, stanzaVersions))
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
	if got != `{"message":"hi","n":"7"}` {
		t.Errorf("upstream saw %q", got)
	}
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
	if got != `{"text":"hi","n":7}` {
		t.Errorf("passthrough body changed: saw=%q want {\"text\":\"hi\",\"n\":7}", got)
	}
}

func TestRequestTransformFailureIs422E2E(t *testing.T) {
	noText := `version: 1
contracts:
  - contract_version: "2024-11"
    route: /v3/echo
    method: POST
    request_message: acme.v1.Ping
    response_message: acme.v1.Pong
    request:
      - rename: { from: missing, to: x }
`
	b, err := bundle.Load(bundletest.Dir(t, noText))
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
