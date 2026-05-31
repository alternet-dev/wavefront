package server_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/dynamicpb"

	"github.com/alternet-dev/wavefront/internal/bundle"
	"github.com/alternet-dev/wavefront/internal/bundletest"
	"github.com/alternet-dev/wavefront/internal/server"
)

// loadBundleWithErrors loads the fixture bundle with a 409 bound to a typed
// error message (acme.v1.Item stands in for a domain error type), exercising
// the declared-typed path.
func loadBundleWithErrors(t *testing.T) *bundle.Bundle {
	t.Helper()
	versions := `version: 1
contracts:
  - contract_version: "2024-11"
    route: /v3/echo
    method: POST
    request_message: acme.v1.Ping
    response_message: acme.v1.Pong
    error_messages:
      "409": acme.v1.Item
`
	b, err := bundle.Load(bundletest.Dir(t, versions))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	return b
}

// TestDeclaredErrorStatusIsTyped: an upstream 409 whose status is declared in
// error_messages becomes a first-class typed response — status preserved,
// body = the bound message encoded from the upstream JSON, and NO
// X-Wavefront-Error (it is a contract response, not a wavefront error).
func TestDeclaredErrorStatusIsTyped(t *testing.T) {
	b := loadBundleWithErrors(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"id":7,"text":"already exists"}`))
	}))
	defer upstream.Close()

	s := server.New(baseCfg(upstream.URL))
	s.SetBundle(b)
	front := httptest.NewServer(s.DataHandler())
	defer front.Close()

	req, _ := http.NewRequest(http.MethodPost, front.URL+"/v3/echo", strings.NewReader(string(pingBytes(t, b, "hi", 1))))
	req.Header.Set("Content-Type", "application/protobuf")
	req.Header.Set("X-Api-Contract-Version", "2024-11")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (declared status preserved)", resp.StatusCode)
	}
	if got := resp.Header.Get("X-Wavefront-Error"); got != "" {
		t.Errorf("X-Wavefront-Error = %q on a declared (route,status), want empty", got)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/protobuf" {
		t.Errorf("Content-Type = %q, want application/protobuf", ct)
	}
	if v := resp.Header.Get("X-Wavefront-Contract-Version"); v != "2024-11" {
		t.Errorf("contract-version = %q, want 2024-11", v)
	}
	out, _ := io.ReadAll(resp.Body)
	itemMD, _ := b.Message("acme.v1.Item")
	item := dynamicpb.NewMessage(itemMD)
	if err := proto.Unmarshal(out, item); err != nil {
		t.Fatalf("body is not acme.v1.Item: %v (raw=%q)", err, out)
	}
	if got := item.Get(itemMD.Fields().ByName("id")).Int(); got != 7 {
		t.Errorf("decoded id = %d, want 7", got)
	}
}

// TestDeclaredErrorBodyMismatchIs502: a declared (route,status) whose upstream
// body cannot decode into the bound message is not a typed response — the
// encode fails and the request collapses to a hard 502 upstream_error. Here the
// upstream sends a 409 (declared acme.v1.Item) but `id` arrives as an array,
// which cannot decode into the int32 field.
func TestDeclaredErrorBodyMismatchIs502(t *testing.T) {
	b := loadBundleWithErrors(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"id":[1,2,3]}`))
	}))
	defer upstream.Close()

	s := server.New(baseCfg(upstream.URL))
	s.SetBundle(b)
	front := httptest.NewServer(s.DataHandler())
	defer front.Close()

	req, _ := http.NewRequest(http.MethodPost, front.URL+"/v3/echo", strings.NewReader(string(pingBytes(t, b, "hi", 1))))
	req.Header.Set("Content-Type", "application/protobuf")
	req.Header.Set("X-Api-Contract-Version", "2024-11")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (declared status but unmatchable body)", resp.StatusCode)
	}
	if got := resp.Header.Get("X-Wavefront-Error"); got != "upstream_error" {
		t.Errorf("X-Wavefront-Error = %q, want upstream_error", got)
	}
}

// TestUndeclaredStatusUnderStrictIs502: with strict:true, an undeclared status
// collapses to a hard 502 upstream_error rather than the aid envelope.
func TestUndeclaredStatusUnderStrictIs502(t *testing.T) {
	versions := `version: 1
contracts:
  - contract_version: "2024-11"
    route: /v3/echo
    method: POST
    request_message: acme.v1.Ping
    response_message: acme.v1.Pong
    strict: true
`
	b, err := bundle.Load(bundletest.Dir(t, versions))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"detail":"down"}`))
	}))
	defer upstream.Close()

	s := server.New(baseCfg(upstream.URL))
	s.SetBundle(b)
	front := httptest.NewServer(s.DataHandler())
	defer front.Close()

	req, _ := http.NewRequest(http.MethodPost, front.URL+"/v3/echo", strings.NewReader(string(pingBytes(t, b, "hi", 1))))
	req.Header.Set("Content-Type", "application/protobuf")
	req.Header.Set("X-Api-Contract-Version", "2024-11")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (strict undeclared)", resp.StatusCode)
	}
	if got := resp.Header.Get("X-Wavefront-Error"); got != "upstream_error" {
		t.Errorf("X-Wavefront-Error = %q, want upstream_error", got)
	}
}

// TestDeclaredErrorRunsStatusScopedTransform: a resolution error_responses
// rename fires on the (409, Item) pair before the body is encoded, so the
// upstream `detail` field lands in the bound message's `text` field. Without
// the transform the upstream shape does not match acme.v1.Item.
func TestDeclaredErrorRunsStatusScopedTransform(t *testing.T) {
	dir := bundletest.Dir(t, `version: 1
contracts:
  - contract_version: "2024-11"
    route: /v3/echo
    method: POST
    request_message: acme.v1.Ping
    response_message: acme.v1.Pong
    error_messages:
      "409": acme.v1.Item
`)
	bundletest.WriteResolution(t, dir, `version: 1
overrides:
  - contract_version: "2024-11"
    transform:
      error_responses:
        "409":
          - rename:
              from: detail
              to: text
`)
	b, err := bundle.Load(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"id":7,"detail":"already exists"}`))
	}))
	defer upstream.Close()

	s := server.New(baseCfg(upstream.URL))
	s.SetBundle(b)
	front := httptest.NewServer(s.DataHandler())
	defer front.Close()

	req, _ := http.NewRequest(http.MethodPost, front.URL+"/v3/echo", strings.NewReader(string(pingBytes(t, b, "hi", 1))))
	req.Header.Set("Content-Type", "application/protobuf")
	req.Header.Set("X-Api-Contract-Version", "2024-11")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (declared status preserved through the transform)", resp.StatusCode)
	}
	out, _ := io.ReadAll(resp.Body)
	itemMD, _ := b.Message("acme.v1.Item")
	item := dynamicpb.NewMessage(itemMD)
	if err := proto.Unmarshal(out, item); err != nil {
		t.Fatalf("body is not acme.v1.Item: %v", err)
	}
	if got := item.Get(itemMD.Fields().ByName("text")).String(); got != "already exists" {
		t.Errorf("decoded text = %q, want %q (rename detail→text must fire on the 409 body)", got, "already exists")
	}
	if got := item.Get(itemMD.Fields().ByName("id")).Int(); got != 7 {
		t.Errorf("decoded id = %d, want 7", got)
	}
}

// TestAidEnvelopeRelaysInvalidUTF8Body: a non-strict undeclared status whose
// upstream body is not valid UTF-8 must still produce a valid envelope (status
// preserved, upstream_status) with a UTF-8-valid relayed message.
func TestAidEnvelopeRelaysInvalidUTF8Body(t *testing.T) {
	b := loadBundle(t) // no error_messages → undeclared
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("boom\xff\xfe\x80"))
	}))
	defer upstream.Close()

	s := server.New(baseCfg(upstream.URL))
	s.SetBundle(b)
	front := httptest.NewServer(s.DataHandler())
	defer front.Close()

	req, _ := http.NewRequest(http.MethodPost, front.URL+"/v3/echo", strings.NewReader(string(pingBytes(t, b, "hi", 1))))
	req.Header.Set("Content-Type", "application/protobuf")
	req.Header.Set("X-Api-Contract-Version", "2024-11")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (status preserved)", resp.StatusCode)
	}
	if got := resp.Header.Get("X-Wavefront-Error"); got != "upstream_status" {
		t.Errorf("X-Wavefront-Error = %q, want upstream_status", got)
	}
	md := wavefrontErrorMD(t)
	body, _ := io.ReadAll(resp.Body)
	m := dynamicpb.NewMessage(md)
	if err := proto.Unmarshal(body, m); err != nil {
		t.Fatalf("body not wavefront.v0.Error: %v", err)
	}
	if got := m.Get(md.Fields().ByName("message")).String(); !strings.Contains(got, "boom") {
		t.Errorf("relayed message = %q, want it to contain the upstream body prefix", got)
	}
}

// TestDeclaredErrorTransformAppliesAcrossChain proves the per-status error
// transform walks the whole version chain in reverse, not just the negotiated
// head. The chain is [2024-01 → 2024-12]: only the terminal (2024-12) binds an
// error_responses op for 409, and the head (2024-01) binds none. The terminal's
// rename (detail→text) must still fire, and the head's nil ops must be a clean
// passthrough. Without the full-chain reverse walk only the head's (empty) ops
// would run, the upstream `detail` would stay unknown to acme.v1.Item, the
// encode would fail, and the response would collapse to 502 — so this test is
// red without the chain loop.
func TestDeclaredErrorTransformAppliesAcrossChain(t *testing.T) {
	fds := bundletest.FDSBytes(t)
	mk := func(cv string) bundletest.Layer {
		return bundletest.Layer{
			Name:        cv,
			Descriptors: fds,
			OpenAPI:     bundletest.ValidOpenAPI,
			Versions: "version: 1\ncontracts:\n  - contract_version: \"" + cv + "\"\n" +
				"    route: /v3/echo\n    method: POST\n" +
				"    request_message: acme.v1.Ping\n    response_message: acme.v1.Pong\n" +
				"    error_messages:\n      \"409\": acme.v1.Item\n",
		}
	}
	dir := bundletest.MultiDir(t, mk("2024-01"), mk("2024-12"))
	// 2024-01 (head) only chains to the terminal — it binds NO error_responses,
	// so its ErrorResponseOps(409) is nil (the passthrough link). 2024-12
	// (terminal) renames the upstream `detail` onto the bound acme.v1.Item.text.
	bundletest.WriteResolution(t, dir, `version: 1
overrides:
  - contract_version: "2024-01"
    transform:
      target: "2024-12"
  - contract_version: "2024-12"
    transform:
      error_responses:
        "409":
          - rename:
              from: detail
              to: text
`)
	b, err := bundle.Load(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"id":7,"detail":"already exists"}`))
	}))
	defer upstream.Close()

	s := server.New(baseCfg(upstream.URL))
	s.SetBundle(b)
	front := httptest.NewServer(s.DataHandler())
	defer front.Close()

	req, _ := http.NewRequest(http.MethodPost, front.URL+"/v3/echo", strings.NewReader(string(pingBytes(t, b, "hi", 1))))
	req.Header.Set("Content-Type", "application/protobuf")
	req.Header.Set("X-Api-Contract-Version", "2024-01")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (declared status preserved through the chained transform)", resp.StatusCode)
	}
	if got := resp.Header.Get("X-Wavefront-Error"); got != "" {
		t.Errorf("X-Wavefront-Error = %q on a declared (route,status), want empty", got)
	}
	out, _ := io.ReadAll(resp.Body)
	itemMD, _ := b.Message("acme.v1.Item")
	item := dynamicpb.NewMessage(itemMD)
	if err := proto.Unmarshal(out, item); err != nil {
		t.Fatalf("body is not acme.v1.Item: %v", err)
	}
	if got := item.Get(itemMD.Fields().ByName("text")).String(); got != "already exists" {
		t.Errorf("decoded text = %q, want %q (terminal rename detail→text must fire across the chain)", got, "already exists")
	}
	if got := item.Get(itemMD.Fields().ByName("id")).Int(); got != 7 {
		t.Errorf("decoded id = %d, want 7", got)
	}
}
