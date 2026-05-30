// Package server is the proxy core: a data-plane listener that runs the
// negotiate → decode → one upstream call → encode pipeline, and an ops
// listener for /metrics, /health, /ready. The bundle is held behind an
// atomic.Pointer (set once at boot; reserved for future hot-swap).
package server

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	httppprof "net/http/pprof"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/alternet-dev/wavefront/internal/adapter"
	"github.com/alternet-dev/wavefront/internal/bundle"
	"github.com/alternet-dev/wavefront/internal/config"
	"github.com/alternet-dev/wavefront/internal/tracing"
)

// HTTP server timeouts. ReadHeaderTimeout and ReadTimeout come from config —
// WAVEFRONT_READ_HEADER_TIMEOUT_MS and WAVEFRONT_READ_TIMEOUT_MS — so an
// operator can tune them per deployment. The data-plane WriteTimeout is
// deliberately left unset: a response is bounded by the per-request upstream
// context (WAVEFRONT_REQUEST_TIMEOUT_MS — the operator's override), and a
// fixed WriteTimeout would truncate a legitimately slow-but-valid upstream.
// The ops listener serves only tiny, fast bodies, so it gets a WriteTimeout
// too. IdleTimeout and shutdownTimeout are internal — fixed defaults are
// fine.
const (
	srvIdleTimeout  = 60 * time.Second
	opsWriteTimeout = 10 * time.Second
	shutdownTimeout = 10 * time.Second
)

type Server struct {
	cfg     *config.Config
	bundle  atomic.Pointer[bundle.Bundle]
	adapter adapter.Adapter
	client  *http.Client
	metrics *metrics
	tracer  *tracing.Exporter
	logger  *slog.Logger
	// connStateMap tracks per-connection observability state for the
	// stdlib-boundary detection. See stdlib_boundary.go for the model.
	connStateMap sync.Map
}

func New(cfg *config.Config) *Server {
	s := &Server{
		cfg: cfg,
		// The per-request context owns the deadline; CheckRedirect refuses
		// every 3xx so an upstream redirect surfaces as a 3xx response to
		// the proxy (and falls into the default arm of the status switch in
		// http.go → upstream_error 502). The bundle's route binding is the
		// authoritative path — a redirect from the upstream is shape drift
		// the operator needs to see, not silently follow.
		client: &http.Client{
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		metrics: newMetrics(),
		logger:  slog.Default(),
	}
	s.adapter = adapter.NewProtoJSON(s) // *Server is the MessageResolver
	s.tracer = tracing.NewExporter(tracing.Config{
		Endpoint:    cfg.TracesEndpoint,
		ServiceName: cfg.TracesServiceName,
		SampleRatio: cfg.TracesSampleRatio,
	})
	return s
}

// SetLogger overrides the structured logger used by the request path. The
// default is slog.Default(). Tests use this to capture per-request log lines.
func (s *Server) SetLogger(l *slog.Logger) {
	if l == nil {
		l = slog.Default()
	}
	s.logger = l
}

// SetBundle installs the bundle and marks the proxy ready.
func (s *Server) SetBundle(b *bundle.Bundle) {
	s.bundle.Store(b)
}

func (s *Server) Bundle() *bundle.Bundle { return s.bundle.Load() }

// ReloadBundle re-reads the bundle directory configured on the server and,
// on success, atomically swaps the live bundle. On failure, the previous
// bundle is preserved and the error is both logged and returned so the
// caller (typically a SIGHUP handler) can decide whether to react further.
// In-flight requests continue against the bundle they observed at the start
// of the request — the proxy reads the bundle pointer once per request.
func (s *Server) ReloadBundle() error {
	b, err := bundle.Load(s.cfg.BundlePath)
	if err != nil {
		slog.Error("bundle reload failed; keeping previous bundle",
			"err", err, "path", s.cfg.BundlePath)
		return err
	}
	s.bundle.Store(b)
	slog.Info("bundle reloaded", "path", s.cfg.BundlePath)
	return nil
}

// Message implements adapter.MessageResolver against the current bundle.
func (s *Server) Message(fullName string) (protoreflect.MessageDescriptor, error) {
	b := s.bundle.Load()
	if b == nil {
		return nil, errors.New("no bundle loaded")
	}
	return b.Message(fullName)
}

// DataHandler returns the data-plane handler used by Run. The chain is
// `markHandlerSeen → Recover → proxy`:
//
//   - markHandlerSeen records that the wavefront handler entered, so the
//     ConnState callback knows a later StateClosed is NOT a stdlib-boundary
//     failure. It wraps Recover (not the other way around) so a recovered
//     panic still counts as a handler run — the resulting wavefront-shaped
//     500 envelope is not a stdlib boundary.
//   - Recover converts any panic in the request path into a typed
//     `wavefront.v0.Error` envelope rather than crashing the process or
//     dropping the connection.
//   - proxy is the negotiate → decode → upstream → encode pipeline.
//
// Ops/metrics endpoints (OpsHandler) keep Go's default behavior — a fault in
// pprof or /metrics surfaces normally and the stdlib-boundary detector is
// not wired on that listener.
func (s *Server) DataHandler() http.Handler {
	return s.markHandlerSeen(s.Recover(http.HandlerFunc(s.proxy)))
}

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
		ReadHeaderTimeout: s.cfg.ReadHeaderTimeout,
		ReadTimeout:       s.cfg.ReadTimeout,
		IdleTimeout:       srvIdleTimeout,
		// WriteTimeout intentionally unset — see the timeout consts.
	}
	// Wire ConnContext + ConnState on the data plane so the
	// wavefront_stdlib_boundary_total counter observes responses net/http
	// emitted before the wavefront handler ran. The ops listener is
	// deliberately NOT wired — its responses are wavefront-served and a
	// boundary counter on /metrics traffic would be meaningless.
	s.WireDataServer(data)
	// Open the data-plane listener explicitly so we can wrap it before
	// handing it to Serve. Wrapping AT the listener (rather than re-wrapping
	// inside net/http) is the only way to make the conn that lands in
	// ConnContext / ConnState a *sniffingConn — net/http stores whatever
	// Accept returned as `c.rwc` and writes responses through it, which is
	// exactly what we want to sniff. http.Server.ListenAndServe does not
	// expose a hook to wrap the listener, so we replicate its
	// `net.Listen("tcp", Addr) + Serve` shape here.
	dataLn, err := net.Listen("tcp", s.cfg.ListenAddr)
	if err != nil {
		return err
	}
	dataLn = s.WrapDataListener(dataLn)
	ops := &http.Server{
		Addr:              s.cfg.MetricsAddr,
		Handler:           s.OpsHandler(),
		ReadHeaderTimeout: s.cfg.ReadHeaderTimeout,
		ReadTimeout:       s.cfg.ReadTimeout,
		WriteTimeout:      opsWriteTimeout,
		IdleTimeout:       srvIdleTimeout,
	}

	errc := make(chan error, 2)
	go func() { errc <- data.Serve(dataLn) }()
	go func() { errc <- ops.ListenAndServe() }()

	select {
	case <-ctx.Done():
		shutCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		_ = data.Shutdown(shutCtx)
		_ = ops.Shutdown(shutCtx)
		_ = s.tracer.Shutdown(shutCtx)
		return nil
	case err := <-errc:
		return err
	}
}
