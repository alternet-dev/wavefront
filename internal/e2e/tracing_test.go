package e2e

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"

	"github.com/alternet-dev/wavefront/internal/config"
)

// otlpKV mirrors a single OTLP/JSON key/value attribute for decoding the
// captured span payload.
type otlpKV struct {
	Key   string `json:"key"`
	Value struct {
		StringValue string `json:"stringValue"`
	} `json:"value"`
}

func attrMap(kvs []otlpKV) map[string]string {
	m := make(map[string]string, len(kvs))
	for _, kv := range kvs {
		m[kv.Key] = kv.Value.StringValue
	}
	return m
}

// TestTraceparentEmitsWavefrontSpan asserts the #71 claim end to end: with an
// OTLP endpoint configured, a request carrying a W3C traceparent yields exactly
// one wavefront-attributed span at the collector, continuing the inbound trace
// (trace id preserved, inbound span id becomes our parent, a fresh child span
// id) and carrying the service.name resource plus the contract_version / target
// / transform_outcome attributes — while the forwarded traceparent stays
// untouched.
func TestTraceparentEmitsWavefrontSpan(t *testing.T) {
	spans := make(chan []byte, 4)
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/traces" {
			t.Errorf("collector path = %s, want /v1/traces", r.URL.Path)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("collector Content-Type = %q, want application/json", ct)
		}
		b, _ := io.ReadAll(r.Body)
		spans <- b
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(collector.Close)

	dir := generatedBundleDir(t)
	h := Spawn(t, SpawnOpts{
		BundleDir: dir,
		BackendHandler: func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"text":"hello-back","ok":true}`)
		},
		ConfigOverride: func(cfg *config.Config) {
			cfg.TracesEndpoint = collector.URL
			cfg.TracesServiceName = "wavefront"
			cfg.TracesSampleRatio = 1
		},
	})

	contract, ok := h.Bundle.Contract("2026-05-17")
	if !ok {
		t.Fatal("generated contract not found")
	}
	reqMD, err := h.Bundle.Message(contract.RequestMessage())
	if err != nil {
		t.Fatalf("resolve request message: %v", err)
	}
	reqMsg := dynamicpb.NewMessage(reqMD)
	reqMsg.Set(reqMD.Fields().ByName("text"), protoreflect.ValueOfString("hi"))
	reqBytes, err := proto.Marshal(reqMsg)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}

	const tp = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
	req, _ := http.NewRequest(http.MethodPost, h.Proxy.URL+"/v3/echo", strings.NewReader(string(reqBytes)))
	req.Header.Set("Content-Type", "application/protobuf")
	req.Header.Set("X-Api-Contract-Version", "2026-05-17")
	req.Header.Set("traceparent", tp)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	if got := h.Backend.Last().Header.Get("traceparent"); got != tp {
		t.Errorf("forwarded traceparent = %q, want %q (untouched)", got, tp)
	}

	var raw []byte
	select {
	case raw = <-spans:
	case <-time.After(2 * time.Second):
		t.Fatal("collector received no span within 2s")
	}

	var td struct {
		ResourceSpans []struct {
			Resource struct {
				Attributes []otlpKV `json:"attributes"`
			} `json:"resource"`
			ScopeSpans []struct {
				Spans []struct {
					TraceID      string   `json:"traceId"`
					SpanID       string   `json:"spanId"`
					ParentSpanID string   `json:"parentSpanId"`
					Name         string   `json:"name"`
					Attributes   []otlpKV `json:"attributes"`
				} `json:"spans"`
			} `json:"scopeSpans"`
		} `json:"resourceSpans"`
	}
	if err := json.Unmarshal(raw, &td); err != nil {
		t.Fatalf("decode OTLP payload: %v\n%s", err, raw)
	}
	if len(td.ResourceSpans) != 1 || len(td.ResourceSpans[0].ScopeSpans) != 1 || len(td.ResourceSpans[0].ScopeSpans[0].Spans) != 1 {
		t.Fatalf("want exactly one span, got payload: %s", raw)
	}
	if got := attrMap(td.ResourceSpans[0].Resource.Attributes)["service.name"]; got != "wavefront" {
		t.Errorf("resource service.name = %q, want wavefront", got)
	}

	sp := td.ResourceSpans[0].ScopeSpans[0].Spans[0]
	if sp.Name != "wavefront.proxy" {
		t.Errorf("span name = %q, want wavefront.proxy", sp.Name)
	}
	if sp.TraceID != "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Errorf("traceId = %s, want the inbound trace id", sp.TraceID)
	}
	if sp.ParentSpanID != "00f067aa0ba902b7" {
		t.Errorf("parentSpanId = %s, want the inbound span id", sp.ParentSpanID)
	}
	if sp.SpanID == "" || sp.SpanID == sp.ParentSpanID {
		t.Errorf("spanId = %q, want a fresh non-parent id", sp.SpanID)
	}
	attrs := attrMap(sp.Attributes)
	if attrs["contract_version"] != "2026-05-17" {
		t.Errorf("attr contract_version = %q, want 2026-05-17", attrs["contract_version"])
	}
	if _, ok := attrs["target"]; !ok {
		t.Errorf("span missing target attribute; attrs=%v", attrs)
	}
	if got, ok := attrs["transform_outcome"]; !ok || got != "" {
		t.Errorf("transform_outcome = (%q, present=%v), want (\"\", true) on the success path", got, ok)
	}
}
