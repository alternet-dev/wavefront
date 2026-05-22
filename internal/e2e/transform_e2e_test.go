package e2e

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"
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

func TestTransformE2EBothDirections(t *testing.T) {
	// Claim: a transform override applies request stanzas before the upstream
	// call and response stanzas after; both directions land end-to-end.
	h := Spawn(t, SpawnOpts{
		Resolution: `version: 1
overrides:
  - contract_version: "2024-11"
    transform:
      request:
        - rename: { from: text, to: message }
        - coerce: { field: n, to: string }
      response:
        - rename: { from: msg, to: text }
`,
		BackendHandler: func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"msg":"hello-back"}`)
		},
	})

	req, _ := http.NewRequest(http.MethodPost, h.Proxy.URL, bytes.NewReader(PingBytes(t, h.Bundle, "hi", 7)))
	req.Header.Set("X-Api-Contract-Version", "2024-11")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	// proto3 JSON renders int32 as a number; coerce n→string makes it "7".
	upstreamJSONEquals(t, string(h.Backend.Last().Body), map[string]any{"message": "hi", "n": "7"})
	md, merr := h.Bundle.Message("acme.v1.Pong")
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
	// Claim: a contract version with no resolution override passes the body
	// through untouched (modulo codec).
	h := Spawn(t, SpawnOpts{
		BackendHandler: func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"text":"x"}`)
		},
	})

	req, _ := http.NewRequest(http.MethodPost, h.Proxy.URL, bytes.NewReader(PingBytes(t, h.Bundle, "hi", 7)))
	req.Header.Set("X-Api-Contract-Version", "2024-11")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("passthrough: status=%d want 200", resp.StatusCode)
	}
	upstreamJSONEquals(t, string(h.Backend.Last().Body), map[string]any{"text": "hi", "n": float64(7)})
}

func TestRequestTransformFailureIs422E2E(t *testing.T) {
	// Claim: a request transform verb that cannot apply at runtime returns
	// transform_failed (HTTP 422) and does NOT call the upstream.
	// Coerce text (a non-numeric string) to number: passes the descriptor
	// cross-check at load, but fails at runtime because "hi" cannot be parsed
	// as a number.
	h := Spawn(t, SpawnOpts{
		Resolution: `version: 1
overrides:
  - contract_version: "2024-11"
    transform:
      request:
        - coerce: { field: text, to: number }
`,
	})
	req, _ := http.NewRequest(http.MethodPost, h.Proxy.URL, bytes.NewReader(PingBytes(t, h.Bundle, "hi", 7)))
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
	if c := h.Backend.Count(); c != 0 {
		t.Errorf("upstream must not be called on request transform failure, got %d requests", c)
	}
}

func TestRenameSourceAbsentRequestIs422E2E(t *testing.T) {
	// Claim: a rename whose source field is absent at runtime returns
	// transform_failed (HTTP 422) and does NOT call the upstream.
	// rename n → renamed_n: n exists on the descriptor (passes load-time
	// cross-check), but the request has n unset (proto3 zero-value omitted by
	// protojson), so runtime rename source is absent.
	h := Spawn(t, SpawnOpts{
		Resolution: `version: 1
overrides:
  - contract_version: "2024-11"
    transform:
      request:
        - rename: { from: n, to: renamed_n }
`,
	})
	req, _ := http.NewRequest(http.MethodPost, h.Proxy.URL, bytes.NewReader(PingTextOnly(t, h.Bundle, "hi")))
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
	if c := h.Backend.Count(); c != 0 {
		t.Errorf("upstream must not be called on request transform failure, got %d requests", c)
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
	// Claim: transform stanzas addressed with array-element and nested-key
	// paths apply correctly across the whole pipeline.
	h := Spawn(t, SpawnOpts{
		Versions: nestedArrayVersions,
		Resolution: `version: 1
overrides:
  - contract_version: "2024-12"
    transform:
      request:
        - rename: { from: "items[].text", to: "items[].label" }
        - coerce: { field: "items[].id", to: string }
        - default: { field: meta.locale, value: en-US }
`,
		BackendHandler: func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"text":"server-ok"}`)
		},
	})

	md, err := h.Bundle.Message("acme.v1.PingV2")
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

	req, _ := http.NewRequest(http.MethodPost, h.Proxy.URL, bytes.NewReader(raw))
	req.Header.Set("X-Api-Contract-Version", "2024-12")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}

	upstreamJSONEquals(t, string(h.Backend.Last().Body), map[string]any{
		"items": []any{
			map[string]any{"id": "1", "label": "alpha"},
			map[string]any{"id": "2", "label": "beta"},
		},
		"meta": map[string]any{"locale": "en-US"},
	})

	pongMD, _ := h.Bundle.Message("acme.v1.Pong")
	out := dynamicpb.NewMessage(pongMD)
	rawResp, _ := io.ReadAll(resp.Body)
	if err := proto.Unmarshal(rawResp, out); err != nil {
		t.Fatalf("client body not Pong: %v", err)
	}
	if g := out.Get(pongMD.Fields().ByName("text")).String(); g != "server-ok" {
		t.Errorf("client got text=%q", g)
	}
}
