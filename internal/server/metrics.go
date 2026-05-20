package server

import (
	"github.com/prometheus/client_golang/prometheus"
)

// metrics is a deliberately small surface. It uses a private registry — no
// global state. Bundle-loaded status is intentionally NOT a metric: it is a
// readiness boolean, served by /ready, not a time series.
type metrics struct {
	reg      *prometheus.Registry
	requests prometheus.Counter
	errors   *prometheus.CounterVec
}

func newMetrics() *metrics {
	reg := prometheus.NewRegistry()
	m := &metrics{
		reg: reg,
		requests: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "wavefront_requests_total",
			Help: "Proxy requests handled since process start.",
		}),
		errors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "wavefront_errors_total",
			Help: "wavefront-originated errors, by code.",
		}, []string{"code"}),
	}
	reg.MustRegister(m.requests, m.errors)
	return m
}
