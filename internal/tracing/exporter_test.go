package tracing

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestExporterPostsSpanToCollector drives the full enqueue→worker→POST path
// against an httptest collector: a span continuing an inbound traceparent must
// arrive as a single OTLP/JSON span at POST /v1/traces with Content-Type
// application/json, carrying the inbound trace id, the inbound span id as
// parentSpanId, a fresh non-parent span id, and the three wavefront attributes.
func TestExporterPostsSpanToCollector(t *testing.T) {
	type captured struct {
		method      string
		path        string
		contentType string
		body        []byte
	}
	got := make(chan captured, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got <- captured{
			method:      r.Method,
			path:        r.URL.Path,
			contentType: r.Header.Get("Content-Type"),
			body:        b,
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	exp := NewExporter(Config{Endpoint: srv.URL, ServiceName: "wavefront", SampleRatio: 1})
	if exp == nil {
		t.Fatal("NewExporter returned nil for a configured endpoint")
	}

	const tp = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
	span := exp.StartSpan("wavefront.proxy", tp)
	if span == nil {
		t.Fatal("StartSpan returned nil for a sampled traceparent")
	}
	span.SetAttr("contract_version", "2024-11")
	span.SetAttr("transform_outcome", "ok")
	span.SetAttr("target", "accounts")
	span.SetStatus(StatusOK)
	span.End()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := exp.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	var c captured
	select {
	case c = <-got:
	case <-time.After(2 * time.Second):
		t.Fatal("collector received no request")
	}

	if c.method != http.MethodPost {
		t.Errorf("method = %s, want POST", c.method)
	}
	if c.path != "/v1/traces" {
		t.Errorf("path = %s, want /v1/traces", c.path)
	}
	if c.contentType != "application/json" {
		t.Errorf("content-type = %s, want application/json", c.contentType)
	}

	var td struct {
		ResourceSpans []struct {
			ScopeSpans []struct {
				Spans []struct {
					TraceID      string `json:"traceId"`
					SpanID       string `json:"spanId"`
					ParentSpanID string `json:"parentSpanId"`
					Attributes   []struct {
						Key   string `json:"key"`
						Value struct {
							StringValue string `json:"stringValue"`
						} `json:"value"`
					} `json:"attributes"`
				} `json:"spans"`
			} `json:"scopeSpans"`
		} `json:"resourceSpans"`
	}
	if err := json.Unmarshal(c.body, &td); err != nil {
		t.Fatalf("unmarshal captured body: %v\nbody: %s", err, c.body)
	}
	if len(td.ResourceSpans) != 1 || len(td.ResourceSpans[0].ScopeSpans) != 1 || len(td.ResourceSpans[0].ScopeSpans[0].Spans) != 1 {
		t.Fatalf("unexpected span shape: %s", c.body)
	}
	sp := td.ResourceSpans[0].ScopeSpans[0].Spans[0]
	if sp.TraceID != "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Errorf("traceId = %s, want continued inbound trace id", sp.TraceID)
	}
	if sp.ParentSpanID != "00f067aa0ba902b7" {
		t.Errorf("parentSpanId = %s, want inbound span id", sp.ParentSpanID)
	}
	if sp.SpanID == "" || sp.SpanID == sp.ParentSpanID {
		t.Errorf("spanId = %q, want a fresh non-parent id", sp.SpanID)
	}
	attrs := map[string]string{}
	for _, a := range sp.Attributes {
		attrs[a.Key] = a.Value.StringValue
	}
	for k, want := range map[string]string{"contract_version": "2024-11", "transform_outcome": "ok", "target": "accounts"} {
		if attrs[k] != want {
			t.Errorf("attr %s = %q, want %q", k, attrs[k], want)
		}
	}
}

// TestUnsampledTraceparentIsDropped: an inbound traceparent with the sampled
// bit clear means an upstream sampler already decided not to record this trace;
// wavefront honors that and emits no span.
func TestUnsampledTraceparentIsDropped(t *testing.T) {
	exp := NewExporter(Config{Endpoint: "http://127.0.0.1:1/unused", SampleRatio: 1})
	if exp == nil {
		t.Fatal("NewExporter returned nil for a configured endpoint")
	}
	defer func() { _ = exp.Shutdown(context.Background()) }()
	const unsampled = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-00"
	if span := exp.StartSpan("wavefront.proxy", unsampled); span != nil {
		t.Error("StartSpan must return nil for an unsampled inbound traceparent")
	}
}

// TestStartSpanWithoutTraceparentMakesRoot: with no inbound traceparent we are
// the trace originator — generate a fresh non-zero trace id and no parent.
func TestStartSpanWithoutTraceparentMakesRoot(t *testing.T) {
	exp := &Exporter{ratio: 1}
	span := exp.StartSpan("wavefront.proxy", "")
	if span == nil {
		t.Fatal("root span should be sampled at ratio 1")
	}
	if span.hasParent {
		t.Error("root span must not have a parent")
	}
	var zero [16]byte
	if span.traceID == zero {
		t.Error("root span must have a generated non-zero trace id")
	}
}

// TestNilExporterAndSpanAreNoOps pins the zero-overhead disabled path: an unset
// endpoint yields a nil exporter, and every nil-receiver method is a safe no-op.
func TestNilExporterAndSpanAreNoOps(t *testing.T) {
	exp := NewExporter(Config{Endpoint: ""})
	if exp != nil {
		t.Fatal("NewExporter with empty endpoint must return nil")
	}
	span := exp.StartSpan("x", "")
	if span != nil {
		t.Fatal("StartSpan on nil exporter must return nil")
	}
	span.SetAttr("k", "v")
	span.SetStatus(StatusOK)
	span.End()
	if err := exp.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown on nil exporter: %v", err)
	}
}
