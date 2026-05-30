package tracing

import "testing"

func TestParseTraceparentValid(t *testing.T) {
	// version 00, 16-byte trace id, 8-byte parent span id, flags 01 (sampled).
	const v = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
	tp, ok := ParseTraceparent(v)
	if !ok {
		t.Fatalf("ParseTraceparent(%q) ok=false, want true", v)
	}
	wantTrace := [16]byte{0x4b, 0xf9, 0x2f, 0x35, 0x77, 0xb3, 0x4d, 0xa6, 0xa3, 0xce, 0x92, 0x9d, 0x0e, 0x0e, 0x47, 0x36}
	if tp.TraceID != wantTrace {
		t.Errorf("TraceID = %x, want %x", tp.TraceID, wantTrace)
	}
	wantParent := [8]byte{0x00, 0xf0, 0x67, 0xaa, 0x0b, 0xa9, 0x02, 0xb7}
	if tp.ParentID != wantParent {
		t.Errorf("ParentID = %x, want %x", tp.ParentID, wantParent)
	}
	if !tp.Sampled {
		t.Errorf("Sampled = false, want true (flags 01)")
	}
}

func TestParseTraceparentUnsampledFlag(t *testing.T) {
	const v = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-00"
	tp, ok := ParseTraceparent(v)
	if !ok {
		t.Fatalf("ok=false, want true")
	}
	if tp.Sampled {
		t.Errorf("Sampled = true, want false (flags 00)")
	}
}

func TestParseTraceparentMalformed(t *testing.T) {
	cases := map[string]string{
		"empty":            "",
		"too few parts":    "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7",
		"too many parts":   "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01-extra",
		"short trace id":   "00-4bf9-00f067aa0ba902b7-01",
		"short span id":    "00-4bf92f3577b34da6a3ce929d0e0e4736-00f0-01",
		"non-hex trace id": "00-zzf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01",
		"all-zero trace":   "00-00000000000000000000000000000000-00f067aa0ba902b7-01",
		"all-zero span":    "00-4bf92f3577b34da6a3ce929d0e0e4736-0000000000000000-01",
		"legacy token":     "tp-1",
	}
	for name, v := range cases {
		t.Run(name, func(t *testing.T) {
			if _, ok := ParseTraceparent(v); ok {
				t.Errorf("ParseTraceparent(%q) ok=true, want false", v)
			}
		})
	}
}
