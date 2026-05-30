// Package tracing emits a single wavefront-attributed span per proxied request
// to an OTLP/HTTP collector, without pulling in the OpenTelemetry SDK. It parses
// the inbound W3C traceparent, hand-marshals the minimal OTLP/JSON payload, and
// ships it on a bounded background queue so span emission never blocks or fails
// the request path. With no endpoint configured the exporter is nil and every
// method is a zero-overhead no-op.
package tracing

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"io"
	"math"
	"net/http"
	"strings"
	"sync"
	"time"
)

// StatusCode mirrors the OTLP span status codes: UNSET (omitted on the wire),
// OK, and ERROR.
type StatusCode int

const (
	StatusUnset StatusCode = 0
	StatusOK    StatusCode = 1
	StatusError StatusCode = 2
)

// queueDepth bounds the in-flight span backlog. Beyond it, spans are dropped
// rather than blocking the request path.
const queueDepth = 1024

// Config controls the exporter. An empty Endpoint disables tracing entirely.
type Config struct {
	// Endpoint is the collector base URL; "/v1/traces" is appended. Empty
	// disables the exporter.
	Endpoint string
	// ServiceName is reported as the resource service.name; defaults to
	// "wavefront".
	ServiceName string
	// SampleRatio is the probability [0,1] of recording a root span (a request
	// with no inbound traceparent). Continued traces follow the inbound sampled
	// flag instead. Values <=0 or >1 are treated as 1 (always sample).
	SampleRatio float64
}

// Span is one in-flight wavefront span. All methods are safe on a nil receiver
// (the disabled / unsampled path), so callers never need a nil check.
type Span struct {
	exp        *Exporter
	traceID    [16]byte
	spanID     [8]byte
	parentID   [8]byte
	hasParent  bool
	name       string
	start      time.Time
	end        time.Time
	attrs      map[string]string
	statusCode StatusCode
}

// SetAttr records a span attribute. No-op on a nil span.
func (s *Span) SetAttr(key, value string) {
	if s == nil {
		return
	}
	s.attrs[key] = value
}

// SetStatus sets the span status code. No-op on a nil span.
func (s *Span) SetStatus(code StatusCode) {
	if s == nil {
		return
	}
	s.statusCode = code
}

// End stamps the end time and enqueues the span for export. No-op on a nil span.
func (s *Span) End() {
	if s == nil {
		return
	}
	s.end = time.Now()
	s.exp.enqueue(s)
}

// Exporter ships spans to an OTLP/HTTP collector on a background worker.
type Exporter struct {
	endpoint string
	service  string
	ratio    float64
	client   *http.Client
	queue    chan *Span
	stopCh   chan struct{}
	doneCh   chan struct{}
	stopOnce sync.Once
}

// NewExporter builds an exporter and starts its background worker. It returns
// nil when Endpoint is empty, so tracing is fully disabled with zero overhead.
func NewExporter(cfg Config) *Exporter {
	if strings.TrimSpace(cfg.Endpoint) == "" {
		return nil
	}
	ratio := cfg.SampleRatio
	if ratio <= 0 || ratio > 1 {
		ratio = 1.0
	}
	service := cfg.ServiceName
	if service == "" {
		service = "wavefront"
	}
	e := &Exporter{
		endpoint: strings.TrimRight(cfg.Endpoint, "/") + "/v1/traces",
		service:  service,
		ratio:    ratio,
		client:   &http.Client{Timeout: 5 * time.Second},
		queue:    make(chan *Span, queueDepth),
		stopCh:   make(chan struct{}),
		doneCh:   make(chan struct{}),
	}
	go e.run()
	return e
}

// StartSpan begins a span named name, continuing the inbound W3C traceparent if
// present and sampled (the inbound span becomes our parent), otherwise starting
// a fresh root trace subject to the configured sample ratio. It returns nil when
// the exporter is disabled or the trace is not sampled — callers pass that nil
// through the request and rely on Span's nil-safe methods.
func (e *Exporter) StartSpan(name, traceparent string) *Span {
	if e == nil {
		return nil
	}
	var (
		traceID   [16]byte
		parentID  [8]byte
		hasParent bool
	)
	if tp, ok := ParseTraceparent(traceparent); ok {
		if !tp.Sampled {
			return nil
		}
		traceID = tp.TraceID
		parentID = tp.ParentID
		hasParent = true
	} else {
		if !e.rootSampled() {
			return nil
		}
		randBytes(traceID[:])
	}
	s := &Span{
		exp:       e,
		traceID:   traceID,
		parentID:  parentID,
		hasParent: hasParent,
		name:      name,
		start:     time.Now(),
		attrs:     make(map[string]string),
	}
	randBytes(s.spanID[:])
	return s
}

// Shutdown stops the worker and waits for the queue to drain or ctx to expire.
// Safe on a nil exporter.
func (e *Exporter) Shutdown(ctx context.Context) error {
	if e == nil {
		return nil
	}
	e.stopOnce.Do(func() { close(e.stopCh) })
	select {
	case <-e.doneCh:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// enqueue offers a span to the worker, dropping it if the queue is full so the
// request path never blocks.
func (e *Exporter) enqueue(s *Span) {
	select {
	case e.queue <- s:
	default:
	}
}

func (e *Exporter) run() {
	defer close(e.doneCh)
	for {
		select {
		case s := <-e.queue:
			e.export(s)
		case <-e.stopCh:
			for {
				select {
				case s := <-e.queue:
					e.export(s)
				default:
					return
				}
			}
		}
	}
}

func (e *Exporter) export(s *Span) {
	payload := encodeTraces(e.service, s)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.endpoint, bytes.NewReader(payload))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := e.client.Do(req)
	if err != nil {
		return
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
}

// rootSampled reports whether a root trace should be recorded, per the sample
// ratio. A crypto/rand failure falls back to sampling (visibility over silence).
func (e *Exporter) rootSampled() bool {
	if e.ratio >= 1 {
		return true
	}
	if e.ratio <= 0 {
		return false
	}
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return true
	}
	return float64(binary.BigEndian.Uint64(b[:]))/float64(math.MaxUint64) < e.ratio
}

func randBytes(b []byte) {
	_, _ = rand.Read(b)
}
