package tracing

import (
	"strings"
	"testing"
	"time"
)

// TestEncodeTracesGolden pins the exact OTLP/HTTP JSON wire shape for a child
// span: hex trace/span/parent ids, nanos-as-string times, kind=2 (SERVER),
// sorted attributes, and resource service.name. A drift here is a wire-format
// change a collector would notice.
func TestEncodeTracesGolden(t *testing.T) {
	s := &Span{
		traceID:    [16]byte{0x4b, 0xf9, 0x2f, 0x35, 0x77, 0xb3, 0x4d, 0xa6, 0xa3, 0xce, 0x92, 0x9d, 0x0e, 0x0e, 0x47, 0x36},
		spanID:     [8]byte{0x00, 0xf0, 0x67, 0xaa, 0x0b, 0xa9, 0x02, 0xb8},
		parentID:   [8]byte{0x00, 0xf0, 0x67, 0xaa, 0x0b, 0xa9, 0x02, 0xb7},
		hasParent:  true,
		name:       "wavefront.proxy",
		start:      time.Unix(0, 1700000000000000000),
		end:        time.Unix(0, 1700000000005000000),
		attrs:      map[string]string{"contract_version": "2024-11", "target": "accounts"},
		statusCode: StatusOK,
	}
	got := string(encodeTraces("wavefront", s))
	const want = `{"resourceSpans":[{"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"wavefront"}}]},"scopeSpans":[{"scope":{"name":"wavefront"},"spans":[{"traceId":"4bf92f3577b34da6a3ce929d0e0e4736","spanId":"00f067aa0ba902b8","parentSpanId":"00f067aa0ba902b7","name":"wavefront.proxy","kind":2,"startTimeUnixNano":"1700000000000000000","endTimeUnixNano":"1700000000005000000","attributes":[{"key":"contract_version","value":{"stringValue":"2024-11"}},{"key":"target","value":{"stringValue":"accounts"}}],"status":{"code":1}}]}]}]}`
	if got != want {
		t.Errorf("encodeTraces mismatch:\n got: %s\nwant: %s", got, want)
	}
}

// TestEncodeTracesRootSpanOmitsParentAndUnsetStatus: a root span (no inbound
// parent) must omit parentSpanId, and an UNSET status must be omitted entirely.
func TestEncodeTracesRootSpanOmitsParentAndUnsetStatus(t *testing.T) {
	s := &Span{
		traceID:    [16]byte{0x01},
		spanID:     [8]byte{0x02},
		hasParent:  false,
		name:       "wavefront.proxy",
		start:      time.Unix(0, 1000),
		end:        time.Unix(0, 2000),
		attrs:      map[string]string{},
		statusCode: StatusUnset,
	}
	got := string(encodeTraces("wavefront", s))
	if strings.Contains(got, "parentSpanId") {
		t.Errorf("root span must omit parentSpanId: %s", got)
	}
	if strings.Contains(got, `"status"`) {
		t.Errorf("unset status must be omitted: %s", got)
	}
	// The resource block always carries "attributes" (service.name); scope the
	// empty-attribute-set check to the span portion only.
	_, spanPart, _ := strings.Cut(got, `"spans":[`)
	if strings.Contains(spanPart, `"attributes"`) {
		t.Errorf("empty span attribute set must be omitted: %s", got)
	}
}
