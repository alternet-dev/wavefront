package tracing

import (
	"encoding/hex"
	"encoding/json"
	"sort"
	"strconv"
)

// OTLP span kind for an inbound server request (SERVER = 2). wavefront receives
// the client request, so its span is always a server span.
const spanKindServer = 2

// Instrumentation scope name carried on every emitted span.
const scopeName = "wavefront"

// The hand-maintained OTLP/HTTP JSON structs below cover the minimal subset of
// the TracesData message wavefront emits. Field order and json tags are the
// wire contract a collector reads — pinned by the golden test in otlp_test.go.

type otlpTracesData struct {
	ResourceSpans []otlpResourceSpans `json:"resourceSpans"`
}

type otlpResourceSpans struct {
	Resource   otlpResource     `json:"resource"`
	ScopeSpans []otlpScopeSpans `json:"scopeSpans"`
}

type otlpResource struct {
	Attributes []otlpKeyValue `json:"attributes"`
}

type otlpScopeSpans struct {
	Scope otlpScope  `json:"scope"`
	Spans []otlpSpan `json:"spans"`
}

type otlpScope struct {
	Name string `json:"name"`
}

type otlpSpan struct {
	TraceID           string         `json:"traceId"`
	SpanID            string         `json:"spanId"`
	ParentSpanID      string         `json:"parentSpanId,omitempty"`
	Name              string         `json:"name"`
	Kind              int            `json:"kind"`
	StartTimeUnixNano string         `json:"startTimeUnixNano"`
	EndTimeUnixNano   string         `json:"endTimeUnixNano"`
	Attributes        []otlpKeyValue `json:"attributes,omitempty"`
	Status            *otlpStatus    `json:"status,omitempty"`
}

type otlpKeyValue struct {
	Key   string       `json:"key"`
	Value otlpAnyValue `json:"value"`
}

type otlpAnyValue struct {
	StringValue string `json:"stringValue"`
}

type otlpStatus struct {
	Code int `json:"code"`
}

// encodeTraces marshals a single span into the OTLP/HTTP JSON wire shape a
// collector accepts at /v1/traces. Attributes are emitted sorted by key for a
// stable payload; a root span omits parentSpanId and an unset status is omitted
// entirely (an empty attribute set likewise drops the attributes field).
func encodeTraces(serviceName string, s *Span) []byte {
	span := otlpSpan{
		TraceID:           hex.EncodeToString(s.traceID[:]),
		SpanID:            hex.EncodeToString(s.spanID[:]),
		Name:              s.name,
		Kind:              spanKindServer,
		StartTimeUnixNano: strconv.FormatInt(s.start.UnixNano(), 10),
		EndTimeUnixNano:   strconv.FormatInt(s.end.UnixNano(), 10),
	}
	if s.hasParent {
		span.ParentSpanID = hex.EncodeToString(s.parentID[:])
	}
	if len(s.attrs) > 0 {
		keys := make([]string, 0, len(s.attrs))
		for k := range s.attrs {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		span.Attributes = make([]otlpKeyValue, 0, len(keys))
		for _, k := range keys {
			span.Attributes = append(span.Attributes, otlpKeyValue{
				Key:   k,
				Value: otlpAnyValue{StringValue: s.attrs[k]},
			})
		}
	}
	if s.statusCode != StatusUnset {
		span.Status = &otlpStatus{Code: int(s.statusCode)}
	}
	data := otlpTracesData{
		ResourceSpans: []otlpResourceSpans{{
			Resource: otlpResource{
				Attributes: []otlpKeyValue{{
					Key:   "service.name",
					Value: otlpAnyValue{StringValue: serviceName},
				}},
			},
			ScopeSpans: []otlpScopeSpans{{
				Scope: otlpScope{Name: scopeName},
				Spans: []otlpSpan{span},
			}},
		}},
	}
	b, _ := json.Marshal(data)
	return b
}
