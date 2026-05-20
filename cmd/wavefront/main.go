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

	"github.com/alternet-dev/wavefront/internal/bundle"
	"github.com/alternet-dev/wavefront/internal/config"
	"github.com/alternet-dev/wavefront/internal/server"
)

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

	slog.Info("wavefront listening", "data", cfg.ListenAddr, "ops", cfg.MetricsAddr)
	if err := srv.Run(ctx); err != nil {
		slog.Error("server stopped with error", "err", err)
		os.Exit(1)
	}
}
