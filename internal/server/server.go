// Package server is the proxy core: a data-plane listener that runs the
// negotiate → decode → one upstream call → encode pipeline, and an ops
// listener for /metrics, /healthz, /readyz. The bundle is held behind an
// atomic.Pointer (set once at boot in v0.1; the v0.3 SIGHUP-swap hook point).
package server

import (
	"context"
	"errors"
	"io"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/alternet-dev/wavefront/internal/adapter"
	"github.com/alternet-dev/wavefront/internal/bundle"
	"github.com/alternet-dev/wavefront/internal/config"
)

type Server struct {
	cfg     *config.Config
	bundle  atomic.Pointer[bundle.Bundle]
	adapter adapter.Adapter
	client  *http.Client
	metrics *metrics
}

func New(cfg *config.Config) *Server {
	s := &Server{
		cfg:     cfg,
		client:  &http.Client{}, // the per-request context owns the deadline
		metrics: newMetrics(),
	}
	s.adapter = adapter.NewProtoJSON(s) // *Server is the MessageResolver
	return s
}

// SetBundle installs the bundle and marks the proxy ready. v0.1 calls this
// once at boot; the atomic pointer is the v0.3 hot-swap hook point.
func (s *Server) SetBundle(b *bundle.Bundle) {
	s.bundle.Store(b)
	s.metrics.bundleSet.Set(1)
}

func (s *Server) Bundle() *bundle.Bundle { return s.bundle.Load() }

// Message implements adapter.MessageResolver against the current bundle.
func (s *Server) Message(fullName string) (protoreflect.MessageDescriptor, error) {
	b := s.bundle.Load()
	if b == nil {
		return nil, errors.New("no bundle loaded")
	}
	return b.Message(fullName)
}

func (s *Server) DataHandler() http.Handler { return http.HandlerFunc(s.proxy) }

func (s *Server) OpsHandler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(s.metrics.reg, promhttp.HandlerOpts{}))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, "ok")
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		if s.bundle.Load() == nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, "not ready")
			return
		}
		_, _ = io.WriteString(w, "ready")
	})
	return mux
}

// Run starts both listeners and shuts them down gracefully when ctx is done.
func (s *Server) Run(ctx context.Context) error {
	data := &http.Server{Addr: s.cfg.ListenAddr, Handler: s.DataHandler()}
	ops := &http.Server{Addr: s.cfg.MetricsAddr, Handler: s.OpsHandler()}

	errc := make(chan error, 2)
	go func() { errc <- data.ListenAndServe() }()
	go func() { errc <- ops.ListenAndServe() }()

	select {
	case <-ctx.Done():
		shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = data.Shutdown(shutCtx)
		_ = ops.Shutdown(shutCtx)
		return nil
	case err := <-errc:
		return err
	}
}
