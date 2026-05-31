package wireerror_test

import (
	"testing"

	"github.com/alternet-dev/wavefront/internal/wireerror"
)

// IsCapabilityCeiling is the shared predicate for statuses wavefront refuses to
// model at every strictness: 206 (Range), 207/208 (WebDAV), 226 (delta), and
// every 3xx redirect. It is the one taxonomy the bundle loader, the generator,
// and the data plane all consult, so it lives in wireerror beside the codes.
func TestIsCapabilityCeiling(t *testing.T) {
	ceiling := []int{206, 207, 208, 226, 300, 301, 302, 303, 304, 307, 308, 399}
	for _, s := range ceiling {
		if !wireerror.IsCapabilityCeiling(s) {
			t.Errorf("IsCapabilityCeiling(%d) = false, want true", s)
		}
	}
	notCeiling := []int{200, 201, 202, 203, 204, 205, 400, 401, 404, 409, 422, 429, 451, 500, 502, 503}
	for _, s := range notCeiling {
		if wireerror.IsCapabilityCeiling(s) {
			t.Errorf("IsCapabilityCeiling(%d) = true, want false", s)
		}
	}
}
