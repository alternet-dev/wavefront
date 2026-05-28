package main

import (
	"context"
	"testing"

	"github.com/alternet-dev/wavefront/internal/config"
)

// minimalCfg builds a Config valid enough for initOTel; only the OTel fields
// matter here.
func otelCfg(endpoint, service string, frac float64) *config.Config {
	return &config.Config{
		OTelExporterEndpoint: endpoint,
		OTelServiceName:      service,
		OTelSamplingFraction: frac,
	}
}

func TestInitOTelReturnsShutdown(t *testing.T) {
	ctx := context.Background()
	shutdown, err := initOTel(ctx, otelCfg("http://localhost:4317", "wavefront-test", 1.0))
	if err != nil {
		t.Fatalf("initOTel: %v", err)
	}
	if shutdown == nil {
		t.Fatal("initOTel returned nil shutdown function")
	}
	if err := shutdown(ctx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
}

func TestInitOTelShutdownIsIdempotent(t *testing.T) {
	ctx := context.Background()
	shutdown, err := initOTel(ctx, otelCfg("http://localhost:4317", "wavefront-test", 0.5))
	if err != nil {
		t.Fatalf("initOTel: %v", err)
	}
	if err := shutdown(ctx); err != nil {
		t.Fatalf("first shutdown: %v", err)
	}
	if err := shutdown(ctx); err != nil {
		t.Fatalf("second shutdown should be a no-op, got: %v", err)
	}
}

func TestInitOTelInvalidEndpointReturnsError(t *testing.T) {
	ctx := context.Background()
	// A scheme-less, host-less URL the exporter cannot dial-target.
	_, err := initOTel(ctx, otelCfg("::not a url::", "wavefront-test", 1.0))
	if err == nil {
		t.Fatal("expected error for invalid endpoint, got nil")
	}
}
