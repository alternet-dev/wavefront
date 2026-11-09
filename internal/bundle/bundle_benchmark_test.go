package bundle_test

import (
	"testing"

	"github.com/alternet-dev/wavefront/internal/bundle"
	"github.com/alternet-dev/wavefront/internal/bundletest"
)

func BenchmarkLoad(b *testing.B) {
	dir := bundletest.Dir(b, "")
	b.ReportAllocs()
	b.ResetTimer()

	for b.Loop() {
		if _, err := bundle.Load(dir); err != nil {
			b.Fatalf("Load: %v", err)
		}
	}
}
