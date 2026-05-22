// Package e2e is the claim-backed integration suite: it loads a real bundle,
// drives the real proxy against a stub backend (both via httptest), and
// asserts the operational claims wavefront makes. Each test reads as "given
// this bundle and config, assert this operational claim." Pure-Go, no
// protoc, no Docker. The shared harness lives in harness_test.go; tests are
// organized into files by claim cluster.
package e2e

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
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

// Harness is a running wavefront proxy plus a stub upstream, both via
// httptest, with all resources released on test cleanup. Proxy.URL is where
// the test sends requests; Backend captures what the proxy sent upstream;
// Ops.URL serves /metrics, /health, /ready.
type Harness struct {
	Proxy   *httptest.Server
	Ops     *httptest.Server
	Backend *StubBackend
	Bundle  *bundle.Bundle
	Config  *config.Config
	Server  *server.Server
}

// SpawnOpts customises Spawn.
type SpawnOpts struct {
	// BundleDir is a pre-existing bundle directory to load. If empty, Spawn
	// builds a default single-layer bundle from bundletest.
	BundleDir string

	// Versions overrides the versions.yaml content of the default bundle.
	// Ignored when BundleDir is set.
	Versions string

	// Resolution, if non-empty, is written as resolution.yaml into the bundle
	// directory (whether the default bundle or one passed via BundleDir).
	Resolution string

	// BackendHandler is invoked when the proxy reaches the stub upstream. If
	// nil, the stub returns 200 with an empty JSON object — enough for the
	// response adapter to encode an empty message.
	BackendHandler http.HandlerFunc

	// ConfigOverride, if non-nil, is called with the populated config just
	// before the server is constructed; tests use it to set RequestTimeout,
	// MaxBodyBytes, Targets, etc.
	ConfigOverride func(*config.Config)
}

// Spawn brings up the proxy and a stub upstream and returns a Harness. The
// proxy's UpstreamBaseURL is pointed at the stub. All resources are released
// via t.Cleanup; tests do not defer Close.
func Spawn(t testing.TB, opts SpawnOpts) *Harness {
	t.Helper()

	dir := opts.BundleDir
	if dir == "" {
		dir = bundletest.Dir(t, opts.Versions)
	}
	if opts.Resolution != "" {
		bundletest.WriteResolution(t, dir, opts.Resolution)
	}
	b, err := bundle.Load(dir)
	if err != nil {
		t.Fatalf("Spawn: bundle.Load: %v", err)
	}

	sb := newStubBackend(t, opts.BackendHandler)

	cfg := &config.Config{
		BundlePath:            dir,
		UpstreamBaseURL:       sb.URL(),
		ContractVersionHeader: "X-Api-Contract-Version",
		RequestTimeout:        2 * time.Second,
		MaxBodyBytes:          1 << 20,
	}
	if opts.ConfigOverride != nil {
		opts.ConfigOverride(cfg)
	}

	s := server.New(cfg)
	s.SetBundle(b)

	proxy := httptest.NewServer(s.DataHandler())
	t.Cleanup(proxy.Close)
	ops := httptest.NewServer(s.OpsHandler())
	t.Cleanup(ops.Close)

	return &Harness{
		Proxy:   proxy,
		Ops:     ops,
		Backend: sb,
		Bundle:  b,
		Config:  cfg,
		Server:  s,
	}
}

// CapturedRequest records what the proxy sent to the stub upstream.
type CapturedRequest struct {
	Method string
	Path   string
	Header http.Header
	Body   []byte
}

// StubBackend is the stub upstream behind the proxy: it captures every
// request the proxy makes and runs the test-supplied handler (or a default
// 200 with empty JSON) to produce the response.
type StubBackend struct {
	server *httptest.Server

	mu       sync.Mutex
	handler  http.HandlerFunc
	captured []CapturedRequest
}

func newStubBackend(t testing.TB, handler http.HandlerFunc) *StubBackend {
	t.Helper()
	sb := &StubBackend{handler: handler}
	sb.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		sb.mu.Lock()
		sb.captured = append(sb.captured, CapturedRequest{
			Method: r.Method,
			Path:   r.URL.Path,
			Header: r.Header.Clone(),
			Body:   append([]byte(nil), body...),
		})
		h := sb.handler
		sb.mu.Unlock()

		if h == nil {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{}`)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		h(w, r)
	}))
	t.Cleanup(sb.server.Close)
	return sb
}

// URL returns the stub upstream's base URL.
func (s *StubBackend) URL() string { return s.server.URL }

// SetHandler replaces the response handler at runtime.
func (s *StubBackend) SetHandler(h http.HandlerFunc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.handler = h
}

// Requests returns a snapshot of every request the proxy has made.
func (s *StubBackend) Requests() []CapturedRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]CapturedRequest, len(s.captured))
	copy(out, s.captured)
	return out
}

// Last returns the most recent captured request. The zero CapturedRequest is
// returned when no request has been captured.
func (s *StubBackend) Last() CapturedRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.captured) == 0 {
		return CapturedRequest{}
	}
	return s.captured[len(s.captured)-1]
}

// Count returns the number of captured requests.
func (s *StubBackend) Count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.captured)
}

// PingBytes marshals an acme.v1.Ping with text and n set. Ping is defined by
// the bundletest fixture FileDescriptorSet.
func PingBytes(t testing.TB, b *bundle.Bundle, text string, n int32) []byte {
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

// PingTextOnly marshals an acme.v1.Ping with only text set; n stays at its
// proto3 zero value, which protojson omits from the encoded JSON.
func PingTextOnly(t testing.TB, b *bundle.Bundle, text string) []byte {
	t.Helper()
	md, err := b.Message("acme.v1.Ping")
	if err != nil {
		t.Fatalf("resolve Ping: %v", err)
	}
	m := dynamicpb.NewMessage(md)
	m.Set(md.Fields().ByName("text"), protoreflect.ValueOfString(text))
	raw, err := proto.Marshal(m)
	if err != nil {
		t.Fatalf("marshal Ping: %v", err)
	}
	return raw
}
