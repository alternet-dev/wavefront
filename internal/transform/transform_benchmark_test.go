package transform

import "testing"

func benchmarkPath(b *testing.B, value string) Path {
	b.Helper()
	p, err := ParsePath(value)
	if err != nil {
		b.Fatalf("ParsePath(%q): %v", value, err)
	}
	return p
}

func BenchmarkApplyRequest(b *testing.B) {
	defaultValue, err := NewDefaultValue("en-US")
	if err != nil {
		b.Fatalf("NewDefaultValue: %v", err)
	}
	ops := []Op{
		{Kind: KindRename, From: benchmarkPath(b, "items[].external_id"), To: benchmarkPath(b, "items[].internal_id")},
		{Kind: KindCoerce, Field: benchmarkPath(b, "items[].internal_id"), CoerceTo: "string"},
		{Kind: KindDefault, Field: benchmarkPath(b, "meta.locale"), Value: defaultValue},
	}
	body := []byte(`{"items":[{"external_id":1001,"name":"alpha"},{"external_id":1002,"name":"beta"}],"meta":{}}`)
	b.ReportAllocs()
	b.SetBytes(int64(len(body)))
	b.ResetTimer()

	for b.Loop() {
		if _, werr := ApplyRequest(ops, body); werr != nil {
			b.Fatalf("ApplyRequest: %v", werr)
		}
	}
}
