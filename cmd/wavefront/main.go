// Command wavefront is the edge contract-mediation proxy: it maps a versioned
// external contract onto a single evolving internal HTTP/JSON backend.
//
// Compose: parse config → load+validate the bundle (fail-fast; refuse to
// start on a bad bundle) → serve the data-plane and ops listeners until
// SIGINT/SIGTERM.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/alternet-dev/wavefront/internal/bundle"
	"github.com/alternet-dev/wavefront/internal/config"
	"github.com/alternet-dev/wavefront/internal/server"
)

// otelShutdownTimeout bounds the OTel batch-span-processor flush at process
// exit. Matches the server's shutdown grace.
const otelShutdownTimeout = 10 * time.Second

func main() {
	cfg, err := config.Load()
	if err != nil {
		slog.Error("invalid configuration", "err", err)
		os.Exit(1)
	}
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: cfg.LogLevel})))

	b, err := bundle.Load(cfg.BundlePath)
	if err != nil {
		slog.Error("bundle load failed; refusing to start", "err", err)
		os.Exit(1)
	}

	srv := server.New(cfg)
	srv.SetBundle(b)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// OpenTelemetry: opt-in. With either knob unset the SDK is never
	// initialized and the request path stays on the global no-op tracer
	// provider — zero per-request overhead.
	if cfg.OTelExporterEndpoint != "" && cfg.OTelSamplingFraction > 0 {
		shutdownOTel, err := initOTel(ctx, cfg)
		if err != nil {
			// Observability is best-effort: a tracing-init failure must not
			// keep the proxy from serving.
			slog.Warn("otel init failed; continuing without tracing", "err", err)
		} else {
			defer func() {
				shutCtx, cancel := context.WithTimeout(context.Background(), otelShutdownTimeout)
				defer cancel()
				if err := shutdownOTel(shutCtx); err != nil {
					slog.Warn("otel shutdown error", "err", err)
				}
			}()
		}
	}

	hupCh := make(chan os.Signal, 1)
	signal.Notify(hupCh, syscall.SIGHUP)
	defer signal.Stop(hupCh)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-hupCh:
				_ = srv.ReloadBundle() // errors are logged inside; in-flight requests keep using the previous bundle either way
			}
		}
	}()

	slog.Info("wavefront listening", "data", cfg.ListenAddr, "ops", cfg.MetricsAddr)
	if err := srv.Run(ctx); err != nil {
		slog.Error("server stopped with error", "err", err)
		os.Exit(1)
	}
}
