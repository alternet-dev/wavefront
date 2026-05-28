package server_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/dynamicpb"

	"github.com/alternet-dev/wavefront/internal/server"
)

// TestUpstreamRedirectIs502 pins the no-redirect-follow contract from issue
// #39's HTTP status matrix: "Upstream redirects are not followed → 502;
// wavefront never redirects clients (routes are bound verbatim)." A 3xx from
// the upstream is shape drift — the bundle's route binding is the
// authoritative path, so a redirect signals a misconfigured backend that the
// operator needs to see, not silently follow.
//
// 304 Not Modified is folded into the same case: conditional requests are
// not part of the v0.1 contract, so an upstream 304 means the route is
// misconfigured. Treat it as upstream_error 502.
//
// The upstream stub serves two paths:
//   - /v3/echo (the bound route) returns the redirect status under test, with
//     Location pointing at /redirected on the SAME stub. This lets the test
//     prove behaviorally that wavefront does NOT follow the redirect — if it
//     did, the /redirected handler would return 200 + a valid Pong, and the
//     wavefront response would be 200, not 502.
//   - /redirected serves a real, valid upstream response. It also records
//     that it was hit, so the test can assert it was NOT.
//
// Each case asserts:
//   - HTTP status 502 from wavefront
//   - X-Wavefront-Error: upstream_error
//   - X-Wavefront-Contract-Version: <resolved version>
//   - Body decodes as wavefront.v0.Error{code: "upstream_error"}
//   - Location header is NOT relayed (the whole point is to NOT propagate the
//     redirect)
//   - The redirect target (/redirected) was NOT hit by wavefront — the
//     redirect must not be followed.
func TestUpstreamRedirectIs502(t *testing.T) {
	cases := []int{
		http.StatusMovedPermanently,  // 301
		http.StatusFound,             // 302
		http.StatusSeeOther,          // 303
		http.StatusNotModified,       // 304 — out of the passthrough set; conditional requests are not in v0.1
		http.StatusTemporaryRedirect, // 307
		http.StatusPermanentRedirect, // 308
	}
	md := wavefrontErrorMD(t)
	for _, status := range cases {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			b := loadBundle(t)

			var redirectedHits atomic.Int32
			mux := http.NewServeMux()
			mux.HandleFunc("/v3/echo", func(w http.ResponseWriter, _ *http.Request) {
				// Same-origin redirect target so that if wavefront followed
				// the redirect it would succeed against /redirected and the
				// test would see a 200 — not 502.
				w.Header().Set("Location", "/redirected")
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(status)
				_, _ = w.Write([]byte(`{"upstream":"raw body that must not reach the client"}`))
			})
			mux.HandleFunc("/redirected", func(w http.ResponseWriter, _ *http.Request) {
				redirectedHits.Add(1)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"text":"should-not-be-seen"}`))
			})
			upstream := httptest.NewServer(mux)
			defer upstream.Close()

			s := server.New(baseCfg(upstream.URL))
			s.SetBundle(b)
			front := httptest.NewServer(s.DataHandler())
			defer front.Close()

			req, _ := http.NewRequest(http.MethodPost, front.URL+"/v3/echo", strings.NewReader(string(pingBytes(t, b, "hi", 1))))
			req.Header.Set("X-Api-Contract-Version", "2024-11")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusBadGateway {
				t.Fatalf("status = %d, want 502 (upstream %d is a redirect; redirects must not be followed)", resp.StatusCode, status)
			}
			if got := resp.Header.Get("X-Wavefront-Error"); got != "upstream_error" {
				t.Errorf("X-Wavefront-Error = %q, want upstream_error", got)
			}
			if ct := resp.Header.Get("Content-Type"); ct != "application/protobuf" {
				t.Errorf("Content-Type = %q, want application/protobuf (body type invariant)", ct)
			}
			if v := resp.Header.Get("X-Wavefront-Contract-Version"); v != "2024-11" {
				t.Errorf("X-Wavefront-Contract-Version = %q, want 2024-11", v)
			}
			// Location must NOT be relayed — wavefront never propagates
			// upstream redirects to clients.
			if got := resp.Header.Get("Location"); got != "" {
				t.Errorf("Location = %q, want empty (upstream redirect Location must not be relayed)", got)
			}
			// Behavioral assertion: the redirect target must NOT have been
			// hit by wavefront. If the http.Client followed the redirect,
			// /redirected would have been requested.
			if hits := redirectedHits.Load(); hits != 0 {
				t.Errorf("upstream /redirected hit %d times; want 0 (redirect must not be followed)", hits)
			}

			body, _ := io.ReadAll(resp.Body)
			// The upstream's raw JSON (from /v3/echo OR /redirected) must not
			// bleed through.
			if strings.Contains(string(body), "raw body that must not reach the client") {
				t.Errorf("upstream raw body leaked into wavefront response: %q", body)
			}
			if strings.Contains(string(body), "should-not-be-seen") {
				t.Errorf("redirect-target body leaked into wavefront response: %q", body)
			}
			m := dynamicpb.NewMessage(md)
			if err := proto.Unmarshal(body, m); err != nil {
				t.Fatalf("response body is not wavefront.v0.Error: %v (raw=%q)", err, body)
			}
			if got := m.Get(md.Fields().ByName("code")).String(); got != "upstream_error" {
				t.Errorf("decoded code = %q, want upstream_error", got)
			}
			if got := m.Get(md.Fields().ByName("message")).String(); got == "" {
				t.Errorf("decoded message is empty; want a non-empty default")
			}
		})
	}
}
