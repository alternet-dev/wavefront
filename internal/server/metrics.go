package server

import (
	"github.com/prometheus/client_golang/prometheus"
)

// metrics is a deliberately small surface. It uses a private registry — no
// global state. Bundle-loaded status is intentionally NOT a metric: it is a
// readiness boolean, served by /ready, not a time series.
//
// Both counters carry a contract_version label so cohorts of pinned clients
// are independently visible. Cardinality is bounded by the bundle's layer
// count plus the literal "unknown" used when negotiation hasn't resolved a
// version yet; unrecognized client-supplied version headers are normalized
// to "unknown" rather than passed through verbatim.
type metrics struct {
	reg      *prometheus.Registry
	requests *prometheus.CounterVec
	errors   *prometheus.CounterVec
}

func newMetrics() *metrics {
	reg := prometheus.NewRegistry()
	m := &metrics{
		reg: reg,
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "wavefront_requests_total",
			Help: "Proxy requests handled since process start, by negotiated contract version.",
		}, []string{"contract_version"}),
		errors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "wavefront_errors_total",
			Help: "wavefront-originated errors, by code and contract version; transform_outcome distinguishes the request side from the response side for transform_failed.",
		}, []string{"code", "contract_version", "transform_outcome"}),
	}
	reg.MustRegister(m.requests, m.errors)
	// Seed the requests counter with the "unknown" cohort so the metric is
	// visible to scrapers from boot, before any request has been observed.
	// A bare CounterVec with no observed labels emits nothing — not even
	// HELP/TYPE — and that hides the metric until the first request lands.
	m.requests.WithLabelValues(versionUnknown)
	return m
}
