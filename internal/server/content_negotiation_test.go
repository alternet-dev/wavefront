package server_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/dynamicpb"

	"github.com/alternet-dev/wavefront/internal/server"
)

const pongJSON = `{"text":"pong","at":"2026-10-13T00:00:00Z"}`

func TestJSONRequestDefaultsToJSONResponse(t *testing.T) {
	b := loadBundle(t)
	var upstreamContentType, upstreamAccept string
	var upstreamBody []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamContentType = r.Header.Get("Content-Type")
		upstreamAccept = r.Header.Get("Accept")
		upstreamBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, pongJSON)
	}))
	defer upstream.Close()

	s := server.New(baseCfg(upstream.URL))
	s.SetBundle(b)
	front := httptest.NewServer(s.DataHandler())
	defer front.Close()

	req, _ := http.NewRequest(http.MethodPost, front.URL+"/v3/echo", strings.NewReader(`{ "n": 7, "text": "ping" }`))
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	req.Header.Set("X-Api-Contract-Version", "2024-11")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Type"); got != "application/json" {
		t.Errorf("response Content-Type = %q, want application/json", got)
	}
	if got := resp.Header.Values("Vary"); !headerValuesContainToken(got, "Accept") {
		t.Errorf("Vary = %q, want Accept", got)
	}
	if upstreamContentType != "application/json" {
		t.Errorf("upstream Content-Type = %q, want application/json", upstreamContentType)
	}
	if upstreamAccept != "application/json" {
		t.Errorf("upstream Accept = %q, want application/json", upstreamAccept)
	}
	var requestBody map[string]any
	if err := json.Unmarshal(upstreamBody, &requestBody); err != nil {
		t.Fatalf("upstream body is not JSON: %v", err)
	}
	if got := requestBody["text"]; got != "ping" {
		t.Errorf("upstream text = %v, want ping", got)
	}
	var responseBody map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&responseBody); err != nil {
		t.Fatalf("response body is not JSON: %v", err)
	}
	if got := responseBody["text"]; got != "pong" {
		t.Errorf("response text = %v, want pong", got)
	}
}

func TestRequestAndResponseCodecsNegotiateIndependently(t *testing.T) {
	b := loadBundle(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, pongJSON)
	}))
	defer upstream.Close()
	s := server.New(baseCfg(upstream.URL))
	s.SetBundle(b)
	front := httptest.NewServer(s.DataHandler())
	defer front.Close()

	t.Run("protobuf request json response", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodPost, front.URL+"/v3/echo", strings.NewReader(string(pingBytes(t, b, "ping", 7))))
		req.Header.Set("Content-Type", "application/protobuf")
		req.Header.Set("Accept", "application/json")
		req.Header.Set("X-Api-Contract-Version", "2024-11")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("request: %v", err)
		}
		defer resp.Body.Close()
		if got := resp.Header.Get("Content-Type"); got != "application/json" {
			t.Fatalf("Content-Type = %q, want application/json", got)
		}
		var body map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatalf("response body is not JSON: %v", err)
		}
	})

	t.Run("json request protobuf response", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodPost, front.URL+"/v3/echo", strings.NewReader(`{"text":"ping","n":7}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/protobuf")
		req.Header.Set("X-Api-Contract-Version", "2024-11")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("request: %v", err)
		}
		defer resp.Body.Close()
		if got := resp.Header.Get("Content-Type"); got != "application/protobuf" {
			t.Fatalf("Content-Type = %q, want application/protobuf", got)
		}
		out, _ := io.ReadAll(resp.Body)
		md, err := b.Message("acme.v1.Pong")
		if err != nil {
			t.Fatalf("resolve Pong: %v", err)
		}
		if err := proto.Unmarshal(out, dynamicpb.NewMessage(md)); err != nil {
			t.Fatalf("response body is not protobuf Pong: %v", err)
		}
	})
}

func TestContentNegotiationFailuresPreserveGateOrdering(t *testing.T) {
	b := loadBundle(t)
	var upstreamCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, pongJSON)
	}))
	defer upstream.Close()
	s := server.New(baseCfg(upstream.URL))
	s.SetBundle(b)
	front := httptest.NewServer(s.DataHandler())
	defer front.Close()

	cases := []struct {
		name        string
		path        string
		contentType string
		accept      string
		wantStatus  int
		wantCode    string
	}{
		{name: "unsupported accept", path: "/v3/echo", contentType: "application/json", accept: "text/plain", wantStatus: 406, wantCode: "not_acceptable"},
		{name: "content type wins before accept", path: "/v3/echo", contentType: "text/plain", accept: "text/plain", wantStatus: 415, wantCode: "unsupported_media_type"},
		{name: "route gate wins first", path: "/missing", contentType: "text/plain", accept: "text/plain", wantStatus: 404, wantCode: "unknown_route"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req, _ := http.NewRequest(http.MethodPost, front.URL+tc.path, strings.NewReader(`{"text":"ping","n":7}`))
			req.Header.Set("Content-Type", tc.contentType)
			req.Header.Set("Accept", tc.accept)
			req.Header.Set("X-Api-Contract-Version", "2024-11")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("request: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != tc.wantStatus {
				t.Errorf("status = %d, want %d", resp.StatusCode, tc.wantStatus)
			}
			if got := resp.Header.Get("X-Wavefront-Error"); got != tc.wantCode {
				t.Errorf("X-Wavefront-Error = %q, want %q", got, tc.wantCode)
			}
			if got := resp.Header.Get("Content-Type"); got != "application/protobuf" {
				t.Errorf("Content-Type = %q, want fixed protobuf error envelope", got)
			}
			if tc.wantCode == "not_acceptable" && !headerValuesContainToken(resp.Header.Values("Vary"), "Accept") {
				t.Errorf("Vary = %q, want Accept", resp.Header.Values("Vary"))
			}
		})
	}
	if got := upstreamCalls.Load(); got != 0 {
		t.Errorf("upstream calls = %d, want 0 for pre-upstream negotiation failures", got)
	}
}

func headerValuesContainToken(values []string, want string) bool {
	for _, value := range values {
		for _, token := range strings.Split(value, ",") {
			if strings.EqualFold(strings.TrimSpace(token), want) {
				return true
			}
		}
	}
	return false
}
