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
// error message (acme.v1.Item stands in for a domain error type) and a 503
// declared strict-irrelevant, exercising the declared-typed path.
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
