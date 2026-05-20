// Package server is the proxy core: a data-plane listener that runs the
// negotiate → decode → one upstream call → encode pipeline, and an ops
// listener for /metrics, /health, /ready. The bundle is held behind an
// atomic.Pointer (set once at boot; reserved for future hot-swap).
package server

import (
	"context"
	"errors"
	"io"
	"net/http"
	httppprof "net/http/pprof"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/alternet-dev/wavefront/internal/adapter"
	"github.com/alternet-dev/wavefront/internal/bundle"
	"github.com/alternet-dev/wavefront/internal/config"
)

// HTTP server timeouts. The data-plane WriteTimeout is deliberately left
// unset: a response is bounded by the per-request upstream context
// (WAVEFRONT_REQUEST_TIMEOUT_MS — the operator's override), and a fixed
// WriteTimeout would truncate a legitimately slow-but-valid upstream. The
// ops listener serves only tiny, fast bodies, so it gets a WriteTimeout too.
const (
	srvReadHeaderTimeout = 5 * time.Second
	srvReadTimeout       = 15 * time.Second
	srvIdleTimeout       = 60 * time.Second
	opsWriteTimeout      = 10 * time.Second
	shutdownTimeout      = 10 * time.Second
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

// SetBundle installs the bundle and marks the proxy ready.
func (s *Server) SetBundle(b *bundle.Bundle) {
	s.bundle.Store(b)
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

// OpsHandler serves the ops surface: Prometheus /metrics, liveness/readiness
// probes (/health, /ready), and pprof. It is intentionally bound to the ops
// listener (WAVEFRONT_METRICS_ADDR) only — pprof must never be reachable on
// the data plane, and is registered explicitly on this mux, not the global
// DefaultServeMux.
func (s *Server) OpsHandler() http.Handler {
	const plain = "text/plain; charset=utf-8"
	mux := http.NewServeMux()

	mux.Handle("/metrics", promhttp.HandlerFor(s.metrics.reg, promhttp.HandlerOpts{}))

	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set(headerContentType, plain)
		_, _ = io.WriteString(w, "ok")
	})
	mux.HandleFunc("/ready", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set(headerContentType, plain)
		if s.bundle.Load() == nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, "not ready")
			return
		}
		_, _ = io.WriteString(w, "ready")
	})

	// pprof — ops listener only.
	mux.HandleFunc("/debug/pprof/", httppprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", httppprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", httppprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", httppprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", httppprof.Trace)

	return mux
}

// Run starts both listeners and shuts them down gracefully when ctx is done.
func (s *Server) Run(ctx context.Context) error {
	data := &http.Server{
		Addr:              s.cfg.ListenAddr,
		Handler:           s.DataHandler(),
		ReadHeaderTimeout: srvReadHeaderTimeout,
		ReadTimeout:       srvReadTimeout,
		IdleTimeout:       srvIdleTimeout,
		// WriteTimeout intentionally unset — see the timeout consts.
	}
	ops := &http.Server{
		Addr:              s.cfg.MetricsAddr,
		Handler:           s.OpsHandler(),
		ReadHeaderTimeout: srvReadHeaderTimeout,
		ReadTimeout:       srvReadTimeout,
		WriteTimeout:      opsWriteTimeout,
		IdleTimeout:       srvIdleTimeout,
	}

	errc := make(chan error, 2)
	go func() { errc <- data.ListenAndServe() }()
	go func() { errc <- ops.ListenAndServe() }()

	select {
	case <-ctx.Done():
		shutCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		_ = data.Shutdown(shutCtx)
		_ = ops.Shutdown(shutCtx)
		return nil
	case err := <-errc:
		return err
	}
}
