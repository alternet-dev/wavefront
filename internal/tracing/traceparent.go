package tracing

import (
	"encoding/hex"
	"strings"
)

// Traceparent is the parsed W3C trace-context `traceparent` header:
// `version-traceid-spanid-flags`. ParentID is the inbound span's id, which
// becomes our emitted span's parentSpanId. Sampled is bit 0 of trace-flags.
type Traceparent struct {
	TraceID  [16]byte
	ParentID [8]byte
	Sampled  bool
}

// ParseTraceparent parses a W3C `traceparent` value. It returns ok=false for
// any malformed or absent value (wrong field count/length, non-hex, or an
// all-zero trace/span id, which the spec forbids). A false result means the
// caller should start a fresh root trace rather than continue an inbound one.
// The header itself is never modified — wavefront forwards it untouched.
func ParseTraceparent(v string) (Traceparent, bool) {
	var tp Traceparent
	parts := strings.Split(v, "-")
	if len(parts) != 4 {
		return tp, false
	}
	if len(parts[0]) != 2 || len(parts[1]) != 32 || len(parts[2]) != 16 || len(parts[3]) != 2 {
		return tp, false
	}
	traceID, err := hex.DecodeString(parts[1])
	if err != nil || allZero(traceID) {
		return tp, false
	}
	spanID, err := hex.DecodeString(parts[2])
	if err != nil || allZero(spanID) {
		return tp, false
	}
	flags, err := hex.DecodeString(parts[3])
	if err != nil {
		return tp, false
	}
	copy(tp.TraceID[:], traceID)
	copy(tp.ParentID[:], spanID)
	tp.Sampled = flags[0]&0x01 != 0
	return tp, true
}

func allZero(b []byte) bool {
	for _, x := range b {
		if x != 0 {
			return false
		}
	}
	return true
}
