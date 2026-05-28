package server_test

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"

	"github.com/alternet-dev/wavefront/internal/bundle"
	"github.com/alternet-dev/wavefront/internal/bundletest"
	"github.com/alternet-dev/wavefront/internal/config"
	"github.com/alternet-dev/wavefront/internal/server"
)

func loadBundle(t *testing.T) *bundle.Bundle {
	t.Helper()
	b, err := bundle.Load(bundletest.Dir(t, ""))
	if err != nil {
		t.Fatalf("load bundle: %v", err)
	}
	return b
}

func pingBytes(t *testing.T, b *bundle.Bundle, text string, n int32) []byte {
	t.Helper()
	md, err := b.Message("acme.v1.Ping")
	if err != nil {
		t.Fatalf("resolve Ping: %v", err)
	}
	m := dynamicpb.NewMessage(md)
	m.Set(md.Fields().ByName("text"), protoreflect.ValueOfString(text))
	m.Set(md.Fields().ByName("n"), protoreflect.ValueOfInt32(n))
	raw, err := proto.Marshal(m)
	if err != nil {
		t.Fatalf("marshal Ping: %v", err)
	}
	return raw
}

func baseCfg(upstream string) *config.Config {
	return &config.Config{
		UpstreamBaseURL:       upstream,
		ContractVersionHeader: "X-Api-Contract-Version",
		RequestTimeout:        2 * time.Second,
		MaxBodyBytes:          1 << 20,
	}
}

func TestHealthAlways200(t *testing.T) {
	s := server.New(baseCfg("http://unused"))
	ts := httptest.NewServer(s.OpsHandler())
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/health")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("health = %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if strings.TrimSpace(string(body)) != "ok" {
		t.Errorf("health body = %q", body)
	}
}

func TestReadyGatesOnBundle(t *testing.T) {
	s := server.New(baseCfg("http://unused"))
	ts := httptest.NewServer(s.OpsHandler())
	defer ts.Close()

	resp, _ := http.Get(ts.URL + "/ready")
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("ready before bundle = %d, want 503", resp.StatusCode)
	}
	resp.Body.Close()

	s.SetBundle(loadBundle(t))
	resp, _ = http.Get(ts.URL + "/ready")
	if resp.StatusCode != 200 {
		t.Fatalf("ready after bundle = %d, want 200", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestMetricsExposition(t *testing.T) {
	s := server.New(baseCfg("http://unused"))
	ts := httptest.NewServer(s.OpsHandler())
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("metrics = %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "wavefront_") {
		t.Errorf("metrics body missing wavefront_ series:\n%s", body)
	}
}

func TestProxySuccessForwardsAndTranslates(t *testing.T) {
	b := loadBundle(t)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v3/echo" {
			t.Errorf("upstream got %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer abc" {
			t.Errorf("Authorization not forwarded untouched: %q", r.Header.Get("Authorization"))
		}
		if r.Header.Get("traceparent") != "tp-1" {
			t.Errorf("traceparent not forwarded untouched: %q", r.Header.Get("traceparent"))
		}
		if r.Header.Get("X-Custom-Thing") != "keep-me" {
			t.Errorf("non-allowlisted client header not passed through: %q", r.Header.Get("X-Custom-Thing"))
		}
		var got map[string]any
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("upstream body not JSON: %v", err)
		}
		if got["text"] != "hi" {
			t.Errorf("upstream JSON text = %v", got["text"])
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"text":"pong"}`))
	}))
	defer upstream.Close()

	s := server.New(baseCfg(upstream.URL))
	s.SetBundle(b)
	front := httptest.NewServer(s.DataHandler())
	defer front.Close()

	req, _ := http.NewRequest(http.MethodPost, front.URL+"/v3/echo", strings.NewReader(string(pingBytes(t, b, "hi", 7))))
	req.Header.Set("Content-Type", "application/protobuf")
	req.Header.Set("X-Api-Contract-Version", "2024-11")
	req.Header.Set("Authorization", "Bearer abc")
	req.Header.Set("traceparent", "tp-1")
	req.Header.Set("X-Custom-Thing", "keep-me")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/protobuf" {
		t.Errorf("Content-Type = %q", ct)
	}
	if v := resp.Header.Get("X-Wavefront-Contract-Version"); v != "2024-11" {
		t.Errorf("X-Wavefront-Contract-Version = %q", v)
	}
	out, _ := io.ReadAll(resp.Body)
	pongMD, _ := b.Message("acme.v1.Pong")
	pong := dynamicpb.NewMessage(pongMD)
	if err := proto.Unmarshal(out, pong); err != nil {
		t.Fatalf("response not protobuf Pong: %v", err)
	}
	if got := pong.Get(pongMD.Fields().ByName("text")).String(); got != "pong" {
		t.Errorf("decoded Pong text = %q", got)
	}
}

func TestBodyTooLargeIs413(t *testing.T) {
	b := loadBundle(t)
	cfg := baseCfg("http://unused")
	cfg.MaxBodyBytes = 4
	s := server.New(cfg)
	s.SetBundle(b)
	front := httptest.NewServer(s.DataHandler())
	defer front.Close()

	req, _ := http.NewRequest(http.MethodPost, front.URL+"/v3/echo", strings.NewReader("way more than four bytes"))
	req.Header.Set("Content-Type", "application/protobuf")
	req.Header.Set("X-Api-Contract-Version", "2024-11")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", resp.StatusCode)
	}
	if resp.Header.Get("X-Wavefront-Error") != "request_body_too_large" {
		t.Errorf("X-Wavefront-Error = %q", resp.Header.Get("X-Wavefront-Error"))
	}
	// request_body_too_large fires before negotiate (MaxBytesReader is
	// installed and consulted before the negotiate.Resolve call). The
	// response header echoes the raw client-sent value so callers can
	// correlate the failure with the version they intended.
	if got := resp.Header.Get("X-Wavefront-Contract-Version"); got != "2024-11" {
		t.Errorf("X-Wavefront-Contract-Version = %q, want %q (raw client value echoed)", got, "2024-11")
	}
}

func TestUnknownContractVersionIs400(t *testing.T) {
	b := loadBundle(t)
	s := server.New(baseCfg("http://unused"))
	s.SetBundle(b)
	front := httptest.NewServer(s.DataHandler())
	defer front.Close()

	req, _ := http.NewRequest(http.MethodPost, front.URL+"/v3/echo", strings.NewReader("ignored"))
	req.Header.Set("Content-Type", "application/protobuf")
	req.Header.Set("X-Api-Contract-Version", "2099-01")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	if resp.Header.Get("X-Wavefront-Error") != "unsupported_contract_version" {
		t.Errorf("X-Wavefront-Error = %q", resp.Header.Get("X-Wavefront-Error"))
	}
	if resp.Header.Get("Content-Type") != "application/protobuf" {
		t.Errorf("Content-Type = %q", resp.Header.Get("Content-Type"))
	}
	// unsupported_contract_version is the canonical pre-negotiate failure
	// where echoing the raw client value pays off — the response now tells
	// the caller exactly which version wavefront received and rejected.
	if got := resp.Header.Get("X-Wavefront-Contract-Version"); got != "2099-01" {
		t.Errorf("X-Wavefront-Contract-Version = %q, want %q (raw client value echoed)", got, "2099-01")
	}
	if body, _ := io.ReadAll(resp.Body); len(body) == 0 {
		t.Error("error body (wavefront.v0.Error) should be non-empty")
	}
}

func TestUpstreamTimeoutIs504(t *testing.T) {
	b := loadBundle(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(300 * time.Millisecond)
		_, _ = w.Write([]byte(`{"text":"late"}`))
	}))
	defer upstream.Close()

	cfg := baseCfg(upstream.URL)
	cfg.RequestTimeout = 40 * time.Millisecond
	s := server.New(cfg)
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
	if resp.StatusCode != http.StatusGatewayTimeout {
		t.Fatalf("status = %d, want 504", resp.StatusCode)
	}
	if resp.Header.Get("X-Wavefront-Error") != "upstream_timeout" {
		t.Errorf("X-Wavefront-Error = %q", resp.Header.Get("X-Wavefront-Error"))
	}
	if resp.Header.Get("Retry-After") != "0" {
		t.Errorf("Retry-After = %q, want 0", resp.Header.Get("Retry-After"))
	}
}

func TestUpstreamNon2xxIs502(t *testing.T) {
	b := loadBundle(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(500)
		_, _ = w.Write([]byte(`{"err":"boom"}`))
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
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}
	if resp.Header.Get("X-Wavefront-Error") != "upstream_error" {
		t.Errorf("X-Wavefront-Error = %q", resp.Header.Get("X-Wavefront-Error"))
	}
}

// Expect: 100-continue + Content-Length > MaxBodyBytes must produce a 413
// directly, with NO "100 Continue" interim status on the wire. RFC 7231
// §5.1.1: an Expect:100-continue request asks the server to acknowledge
// before the client transmits the body; if the server already knows it will
// reject the body for size, it must do so before signalling continue,
// otherwise the client wastes bandwidth sending a body the server discards.
//
// The test uses a raw TCP connection (not http.Client) so it can observe the
// exact byte sequence the server emits and assert no "HTTP/1.1 100 Continue"
// status line appears.
func TestExpect100ContinueOversizedContentLengthRejectedWithoutContinue(t *testing.T) {
	b := loadBundle(t)
	cfg := baseCfg("http://unused")
	cfg.MaxBodyBytes = 16
	s := server.New(cfg)
	s.SetBundle(b)
	front := httptest.NewServer(s.DataHandler())
	defer front.Close()

	u, err := url.Parse(front.URL)
	if err != nil {
		t.Fatalf("parse url: %v", err)
	}
	conn, err := net.Dial("tcp", u.Host)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))

	// Send headers only; do NOT send any body bytes. If wavefront were to
	// emit "100 Continue" we would observe it on the wire even without
	// transmitting the body.
	const bodyLen = 4096 // far above MaxBodyBytes = 16
	req := fmt.Sprintf(
		"POST /v3/echo HTTP/1.1\r\n"+
			"Host: %s\r\n"+
			"Content-Type: application/protobuf\r\n"+
			"X-Api-Contract-Version: 2024-11\r\n"+
			"Content-Length: %d\r\n"+
			"Expect: 100-continue\r\n"+
			"Connection: close\r\n"+
			"\r\n",
		u.Host, bodyLen,
	)
	if _, err := conn.Write([]byte(req)); err != nil {
		t.Fatalf("write request: %v", err)
	}

	br := bufio.NewReader(conn)
	statusLine, err := br.ReadString('\n')
	if err != nil {
		t.Fatalf("read status line: %v", err)
	}
	statusLine = strings.TrimRight(statusLine, "\r\n")
	// The very first response line must be the 413 final status — NOT a
	// "100 Continue" interim status. If we see 100 here, the server has
	// violated the ordering rule.
	if strings.HasPrefix(statusLine, "HTTP/1.1 100") || strings.HasPrefix(statusLine, "HTTP/1.0 100") {
		t.Fatalf("server emitted 100 Continue before rejecting oversized body; status line = %q", statusLine)
	}
	if !strings.HasPrefix(statusLine, "HTTP/1.1 413") && !strings.HasPrefix(statusLine, "HTTP/1.0 413") {
		t.Fatalf("first status line = %q; want a 413", statusLine)
	}

	// Read the rest of the response and confirm the wire-error envelope.
	resp, err := http.ReadResponse(bufio.NewReader(strings.NewReader(statusLine+"\r\n"+readAll(t, br))), nil)
	if err != nil {
		t.Fatalf("parse response: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", resp.StatusCode)
	}
	if got := resp.Header.Get("X-Wavefront-Error"); got != "request_body_too_large" {
		t.Errorf("X-Wavefront-Error = %q, want request_body_too_large", got)
	}
	if got := resp.Header.Get("X-Wavefront-Contract-Version"); got != "2024-11" {
		t.Errorf("X-Wavefront-Contract-Version = %q, want %q (raw client value echoed)", got, "2024-11")
	}
}

// readAll drains br to EOF and returns the result; used by the Expect:100
// test to feed the remaining bytes into http.ReadResponse after peeking at
// the status line.
func readAll(t *testing.T, r *bufio.Reader) string {
	t.Helper()
	var sb strings.Builder
	buf := make([]byte, 1024)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			sb.Write(buf[:n])
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			// timeout or close mid-read — the response was already partially
			// captured into sb; let http.ReadResponse parse what we have.
			break
		}
	}
	return sb.String()
}

// Expect: 100-continue + Content-Length within MaxBodyBytes must proceed
// normally. The Go HTTP client streams the body only after observing a
// "100 Continue"; if wavefront accidentally rejected legitimate Expect-100
// requests, this test would hang or fail.
func TestExpect100ContinueWithinLimitSucceeds(t *testing.T) {
	b := loadBundle(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"text":"pong"}`))
	}))
	defer upstream.Close()

	s := server.New(baseCfg(upstream.URL))
	s.SetBundle(b)
	front := httptest.NewServer(s.DataHandler())
	defer front.Close()

	body := pingBytes(t, b, "hi", 1)
	req, _ := http.NewRequest(http.MethodPost, front.URL+"/v3/echo", strings.NewReader(string(body)))
	req.Header.Set("X-Api-Contract-Version", "2024-11")
	req.Header.Set("Content-Type", "application/protobuf")
	// ExpectContinueTimeout=1s on the transport — Go's client sends headers,
	// waits up to this long for "100 Continue", then sends body.
	req.Header.Set("Expect", "100-continue")
	req.ContentLength = int64(len(body))

	tr := &http.Transport{ExpectContinueTimeout: 1 * time.Second}
	defer tr.CloseIdleConnections()
	client := &http.Client{Transport: tr, Timeout: 5 * time.Second}

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
}
