package negotiate_test

import (
	"testing"

	"github.com/alternet-dev/wavefront/internal/bundle"
	"github.com/alternet-dev/wavefront/internal/bundletest"
	"github.com/alternet-dev/wavefront/internal/negotiate"
)

func BenchmarkResolve(b *testing.B) {
	loaded, err := bundle.Load(bundletest.Dir(b, ""))
	if err != nil {
		b.Fatalf("load bundle: %v", err)
	}
	b.ReportAllocs()
	b.ResetTimer()

	for b.Loop() {
		if _, werr := negotiate.Resolve(loaded, "/v3/echo", "POST", "2024-11"); werr != nil {
			b.Fatalf("Resolve: %v", werr)
		}
	}
}
