package server

import (
	"github.com/prometheus/client_golang/prometheus"
)

// metrics is a deliberately small surface. It uses a private registry — no
// global state. Bundle-loaded status is intentionally NOT a metric: it is a
// readiness boolean, served by /ready, not a time series.
//
// The two main counters carry a contract_version label so cohorts of pinned
// clients are independently visible. Cardinality is bounded by the bundle's
// layer count plus the literal "unknown" used when negotiation hasn't
// resolved a version yet; unrecognized client-supplied version headers are
// normalized to "unknown" rather than passed through verbatim.
//
// stdlibBoundary observes responses net/http emitted before the wavefront
// handler ran (408 slow-header / 431 oversized headers / 417 unexpected
// Expect), documented as out-of-envelope per issue #39. The status code is
// captured by sniffing the response status line on the wire via a wrapped
// net.Conn (see stdlib_boundary.go), and surfaced as the `status` label.
// Cardinality is bounded: net/http only emits a handful of status codes from
// its request reader (408, 431, 417, 400, 501), plus the literal `"unknown"`
// for the slow-header path which closes the connection without writing a
// response line. Different stdlib-boundary failures need different
// remediations (a 408 cohort points at WAVEFRONT_READ_HEADER_TIMEOUT_MS or a
// slow client; a 431 cohort points at oversized headers; a 417 cohort points
// at a client sending non-100-continue Expect), so the label is the bridge
// from "stdlib boundaries are happening" to "this is the kind that is
// happening". Operators querying `sum(wavefront_stdlib_boundary_total)` get
// the original aggregate; querying `{status="408"}` filters to the slow-header
// cohort.
type metrics struct {
	reg            *prometheus.Registry
	requests       *prometheus.CounterVec
	errors         *prometheus.CounterVec
	stdlibBoundary *prometheus.CounterVec
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
		stdlibBoundary: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "wavefront_stdlib_boundary_total",
			Help: "Number of times net/http emitted a stdlib-boundary response (e.g. 408 / 431 / 417) before the wavefront handler ran. These are documented as out-of-envelope per issue #39; this counter surfaces them for operator visibility, labelled by the captured HTTP status code (or 'unknown' if net/http closed the connection without writing a response line).",
		}, []string{"status"}),
	}
	reg.MustRegister(m.requests, m.errors, m.stdlibBoundary)
	// Seed the requests counter with the "unknown" cohort so the metric is
	// visible to scrapers from boot, before any request has been observed.
	// A bare CounterVec with no observed labels emits nothing — not even
	// HELP/TYPE — and that hides the metric until the first request lands.
	// The same is true for stdlibBoundary; seed the "unknown" cohort so
	// `wavefront_stdlib_boundary_total` shows up under a `sum()` from boot.
	// (The string is independent of versionUnknown: contract_version and HTTP
	// status are different label namespaces that happen to share the spelling.)
	m.requests.WithLabelValues(versionUnknown)
	m.stdlibBoundary.WithLabelValues(stdlibBoundaryStatusUnknown)
	return m
}
