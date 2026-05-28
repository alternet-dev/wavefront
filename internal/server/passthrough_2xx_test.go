package server_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/dynamicpb"

	"github.com/alternet-dev/wavefront/internal/server"
)

// TestUpstream2xxStatusPreserved exercises the 2xx fidelity contract from
// docs/protocol.md (issue #39): wavefront preserves the upstream's exact
// success status — 200/201/202/203 with the encoded response_message body,
// and 205 with no body — instead of flattening to 200.
//
// 204 has its own dedicated test because the no-body assertions diverge from
// the 200/201/202/203 path (no Content-Type, no protobuf body), and the
// upstream here cannot legally send a 200-shaped JSON body alongside the
// status.
func TestUpstream2xxStatusPreserved(t *testing.T) {
	cases := []int{
		http.StatusOK,                   // 200
		http.StatusCreated,              // 201
		http.StatusAccepted,             // 202
		http.StatusNonAuthoritativeInfo, // 203
	}
	for _, status := range cases {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			b := loadBundle(t)

			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(status)
				_, _ = w.Write([]byte(`{"text":"pong"}`))
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

			if resp.StatusCode != status {
				t.Fatalf("status = %d, want %d (upstream status must be preserved)", resp.StatusCode, status)
			}
			if ct := resp.Header.Get("Content-Type"); ct != "application/protobuf" {
				t.Errorf("Content-Type = %q, want application/protobuf", ct)
			}
			if v := resp.Header.Get("X-Wavefront-Contract-Version"); v != "2024-11" {
				t.Errorf("X-Wavefront-Contract-Version = %q, want 2024-11", v)
			}
			if got := resp.Header.Get("X-Wavefront-Error"); got != "" {
				t.Errorf("X-Wavefront-Error = %q on a 2xx, want empty", got)
			}
			out, _ := io.ReadAll(resp.Body)
			pongMD, _ := b.Message("acme.v1.Pong")
			pong := dynamicpb.NewMessage(pongMD)
			if err := proto.Unmarshal(out, pong); err != nil {
				t.Fatalf("response not protobuf Pong: %v", err)
			}
			if got := pong.Get(pongMD.Fields().ByName("text")).String(); got != "pong" {
				t.Errorf("decoded Pong text = %q, want pong", got)
			}
		})
	}
}

// TestUpstream204NoBody verifies that an upstream 204 No Content flows
// through verbatim — same status, no body bytes, no encoded empty
// response_message. The X-Wavefront-Contract-Version header is still set
// (docs/protocol.md:148-149: it is on every response).
func TestUpstream204NoBody(t *testing.T) {
	b := loadBundle(t)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
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

	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", resp.StatusCode)
	}
	if v := resp.Header.Get("X-Wavefront-Contract-Version"); v != "2024-11" {
		t.Errorf("X-Wavefront-Contract-Version = %q, want 2024-11", v)
	}
	if got := resp.Header.Get("X-Wavefront-Error"); got != "" {
		t.Errorf("X-Wavefront-Error = %q on a 204, want empty", got)
	}
	body, _ := io.ReadAll(resp.Body)
	if len(body) != 0 {
		t.Errorf("body on 204 must be empty, got %d bytes: %q", len(body), body)
	}
	if cl := resp.Header.Get("Content-Length"); cl != "" && cl != "0" {
		t.Errorf("Content-Length = %q on 204, want empty or 0", cl)
	}
}

// TestUpstream205NoBody verifies that an upstream 205 Reset Content flows
// through verbatim — same status, no body bytes, no encoded empty
// response_message. Per RFC 7231 §6.3.6, 205 carries no body.
func TestUpstream205NoBody(t *testing.T) {
	b := loadBundle(t)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusResetContent)
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

	if resp.StatusCode != http.StatusResetContent {
		t.Fatalf("status = %d, want 205", resp.StatusCode)
	}
	if v := resp.Header.Get("X-Wavefront-Contract-Version"); v != "2024-11" {
		t.Errorf("X-Wavefront-Contract-Version = %q, want 2024-11", v)
	}
	if got := resp.Header.Get("X-Wavefront-Error"); got != "" {
		t.Errorf("X-Wavefront-Error = %q on a 205, want empty", got)
	}
	body, _ := io.ReadAll(resp.Body)
	if len(body) != 0 {
		t.Errorf("body on 205 must be empty, got %d bytes: %q", len(body), body)
	}
	if cl := resp.Header.Get("Content-Length"); cl != "" && cl != "0" {
		t.Errorf("Content-Length = %q on 205, want empty or 0", cl)
	}
}

// TestUpstreamOutOfContract2xxIs502 codifies issue #39's matrix decision:
// 2xx codes outside the passthrough set — 206 Partial Content (no Range
// support), 207/208 WebDAV, 226 delta encoding — are treated as shape drift
// and collapse to upstream_error (502).
func TestUpstreamOutOfContract2xxIs502(t *testing.T) {
	cases := []int{
		http.StatusPartialContent, // 206
		207,                       // Multi-Status (WebDAV)
		208,                       // Already Reported (WebDAV)
		226,                       // IM Used (delta encoding)
	}
	for _, status := range cases {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			b := loadBundle(t)

			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(status)
				_, _ = w.Write([]byte(`{"unused":"body"}`))
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
				t.Fatalf("status = %d, want 502 (out-of-contract 2xx %d is shape drift)", resp.StatusCode, status)
			}
			if got := resp.Header.Get("X-Wavefront-Error"); got != "upstream_error" {
				t.Errorf("X-Wavefront-Error = %q, want upstream_error", got)
			}
		})
	}
}
