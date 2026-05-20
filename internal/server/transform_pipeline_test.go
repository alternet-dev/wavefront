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
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"

	"github.com/alternet-dev/wavefront/internal/bundle"
	"github.com/alternet-dev/wavefront/internal/bundletest"
	"github.com/alternet-dev/wavefront/internal/config"
)

const stanzaBundle = `version: 1
contracts:
  - contract_version: "2024-11"
    route: /v3/echo
    method: POST
    request_message: acme.v1.Ping
    response_message: acme.v1.Pong
    request:
      - rename: { from: text, to: message }
    response:
      - rename: { from: msg, to: text }
`

func testCfg(up string) *config.Config {
	return &config.Config{
		UpstreamBaseURL:       up,
		ContractVersionHeader: "X-Api-Contract-Version",
		RequestTimeout:        2 * time.Second,
		MaxBodyBytes:          1 << 20,
	}
}

func transformPing(t *testing.T, b *bundle.Bundle) []byte {
	t.Helper()
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
	return raw
}

func TestPipelineAppliesTransformBothDirections(t *testing.T) {
	b, err := bundle.Load(bundletest.Dir(t, stanzaBundle))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	var mu sync.Mutex
	var sawBody string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		sawBody = string(body)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"msg":"pong"}`)
	}))
	defer up.Close()

	s := New(testCfg(up.URL))
	s.SetBundle(b)
	fs := httptest.NewServer(s.DataHandler())
	defer fs.Close()

	req, _ := http.NewRequest(http.MethodPost, fs.URL, bytes.NewReader(transformPing(t, b)))
	req.Header.Set("X-Api-Contract-Version", "2024-11")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	mu.Lock()
	got := sawBody
	mu.Unlock()
	// protojson.Marshal injects randomized whitespace between tokens to
	// discourage exact-byte assertions on its output, so compare semantically.
	var gotJSON map[string]any
	if err := json.Unmarshal([]byte(got), &gotJSON); err != nil {
		t.Fatalf("upstream body not JSON: %v (raw=%q)", err, got)
	}
	if want := (map[string]any{"message": "hi"}); !reflect.DeepEqual(gotJSON, want) {
		t.Errorf("upstream body wrong: got=%v want=%v", gotJSON, want)
	}
	md, _ := b.Message("acme.v1.Pong")
	out := dynamicpb.NewMessage(md)
	raw, _ := io.ReadAll(resp.Body)
	if err := proto.Unmarshal(raw, out); err != nil {
		t.Fatalf("client body not Pong: %v", err)
	}
	if got := out.Get(md.Fields().ByName("text")).String(); got != "pong" {
		t.Errorf("client got text=%q want pong", got)
	}
}

func TestPipelineResponseDriftIs502(t *testing.T) {
	b, err := bundle.Load(bundletest.Dir(t, stanzaBundle))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"unexpected":1}`)
	}))
	defer up.Close()

	s := New(testCfg(up.URL))
	s.SetBundle(b)
	fs := httptest.NewServer(s.DataHandler())
	defer fs.Close()

	req, _ := http.NewRequest(http.MethodPost, fs.URL, bytes.NewReader(transformPing(t, b)))
	req.Header.Set("X-Api-Contract-Version", "2024-11")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("want 502, got %d", resp.StatusCode)
	}
	if resp.Header.Get("X-Wavefront-Error") != "transform_failed" {
		t.Errorf("want X-Wavefront-Error transform_failed, got %q", resp.Header.Get("X-Wavefront-Error"))
	}
	if resp.Header.Get("X-Wavefront-Contract-Version") != "2024-11" {
		t.Errorf("contract-version header = %q", resp.Header.Get("X-Wavefront-Contract-Version"))
	}
}

func TestPipelineRequestTransformFailureIs422(t *testing.T) {
	b, err := bundle.Load(bundletest.Dir(t, stanzaBundle))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	up := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		t.Error("upstream must not be called when the request transform fails")
	}))
	defer up.Close()

	s := New(testCfg(up.URL))
	s.SetBundle(b)
	fs := httptest.NewServer(s.DataHandler())
	defer fs.Close()

	md, merr := b.Message("acme.v1.Ping")
	if merr != nil {
		t.Fatalf("resolve Ping: %v", merr)
	}
	empty, perr := proto.Marshal(dynamicpb.NewMessage(md)) // no fields set -> {}
	if perr != nil {
		t.Fatalf("marshal: %v", perr)
	}

	req, _ := http.NewRequest(http.MethodPost, fs.URL, bytes.NewReader(empty))
	req.Header.Set("X-Api-Contract-Version", "2024-11")
	resp, derr := http.DefaultClient.Do(req)
	if derr != nil {
		t.Fatalf("request: %v", derr)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("want 422, got %d", resp.StatusCode)
	}
	if resp.Header.Get("X-Wavefront-Error") != "transform_failed" {
		t.Errorf("X-Wavefront-Error=%q want transform_failed", resp.Header.Get("X-Wavefront-Error"))
	}
	if resp.Header.Get("X-Wavefront-Contract-Version") != "2024-11" {
		t.Errorf("contract-version header=%q", resp.Header.Get("X-Wavefront-Contract-Version"))
	}
}
