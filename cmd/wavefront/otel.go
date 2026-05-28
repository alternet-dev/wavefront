// OpenTelemetry SDK bootstrap. Kept in cmd/wavefront so the OTel surface lives
// at the binary boundary — the request path imports nothing from this file.
//
// Default OFF: main.go only calls initOTel when both an OTLP endpoint and a
// non-zero sampling fraction are configured. With either unset the SDK is
// never touched, and the global tracer provider remains the no-op default
// installed by go.opentelemetry.io/otel.
//
// This chunk wires the SDK; it does not emit spans. Span emission is added
// by wrapping the data-plane handler in otelhttp.NewHandler (and the upstream
// http.Client transport in otelhttp.NewTransport) in a follow-up chunk.
//
// Transport choice: OTLP/HTTP, not OTLP/gRPC. The gRPC variant pulls in
// google.golang.org/grpc + grpc-gateway + genproto + cenkalti/backoff — a
// large indirect surface for negligible benefit at wavefront's expected
// scale (one proxy in front of one backend, not high-throughput batch
// streaming). The HTTP variant speaks the same OTLP protocol over net/http
// and drops all of that.

package main

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"sync"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.41.0"

	"github.com/alternet-dev/wavefront/internal/config"
)

// initOTel builds an OTLP/HTTP trace exporter, wires it into a TracerProvider
// with a TraceIDRatio sampler at cfg.OTelSamplingFraction, sets the global
// tracer provider and W3C tracecontext+baggage propagator, and returns a
// shutdown function that flushes pending spans. The returned shutdown is
// idempotent: subsequent calls are a no-op.
//
// On exporter construction failure the global tracer provider is left
// untouched (the caller logs and continues — observability is best-effort,
// not load-bearing).
func initOTel(ctx context.Context, cfg *config.Config) (func(context.Context) error, error) {
	// Defensive revalidation: config.Load also enforces this, but keep
	// initOTel self-contained so it can be called from anywhere with a
	// hand-built Config (tests, future hot-reload).
	u, err := url.Parse(cfg.OTelExporterEndpoint)
	if err != nil {
		return nil, fmt.Errorf("otel endpoint parse: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, errors.New("otel endpoint scheme must be http or https")
	}
	if u.Host == "" {
		return nil, errors.New("otel endpoint missing host")
	}

	exp, err := otlptracehttp.New(ctx, otlptracehttp.WithEndpointURL(cfg.OTelExporterEndpoint))
	if err != nil {
		return nil, fmt.Errorf("otel exporter: %w", err)
	}

	res, err := resource.Merge(
		resource.Default(),
		resource.NewWithAttributes(
			semconv.SchemaURL,
			semconv.ServiceName(cfg.OTelServiceName),
		),
	)
	if err != nil {
		// Resource.Merge only errors on a schema-URL mismatch; with our own
		// resource using the matching schema, this is unreachable, but if it
		// ever fires, fall back to the attribute-only resource so the SDK
		// still gets a valid service.name.
		res = resource.NewWithAttributes(
			semconv.SchemaURL,
			semconv.ServiceName(cfg.OTelServiceName),
		)
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exp),
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(cfg.OTelSamplingFraction))),
		sdktrace.WithResource(res),
	)
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	var once sync.Once
	var shutdownErr error
	return func(ctx context.Context) error {
		once.Do(func() {
			shutdownErr = tp.Shutdown(ctx)
		})
		return shutdownErr
	}, nil
}
