package negotiate_test

import (
	"testing"

	"github.com/alternet-dev/wavefront/internal/bundle"
	"github.com/alternet-dev/wavefront/internal/bundletest"
	"github.com/alternet-dev/wavefront/internal/negotiate"
)

func loadBundle(t *testing.T) *bundle.Bundle {
	t.Helper()
	b, err := bundle.Load(bundletest.Dir(t, ""))
	if err != nil {
		t.Fatalf("load bundle: %v", err)
	}
	return b
}

func TestResolveKnownContract(t *testing.T) {
	b := loadBundle(t)
	c, werr := negotiate.Resolve(b, "2024-11")
	if werr != nil {
		t.Fatalf("Resolve: %v", werr)
	}
	if c.Route() != "/v3/echo" || c.Method() != "POST" {
		t.Errorf("resolved wrong contract: %q %q", c.Route(), c.Method())
	}
}

func TestResolveMissingHeaderIsUnsupported(t *testing.T) {
	b := loadBundle(t)
	_, werr := negotiate.Resolve(b, "")
	if werr == nil || werr.Code() != "unsupported_contract_version" {
		t.Fatalf("want unsupported_contract_version, got %v", werr)
	}
}

func TestResolveBlankHeaderIsUnsupported(t *testing.T) {
	b := loadBundle(t)
	_, werr := negotiate.Resolve(b, "   ")
	if werr == nil || werr.Code() != "unsupported_contract_version" {
		t.Fatalf("want unsupported_contract_version, got %v", werr)
	}
}

func TestResolveUnknownContractIsUnsupported(t *testing.T) {
	b := loadBundle(t)
	_, werr := negotiate.Resolve(b, "2099-01")
	if werr == nil || werr.Code() != "unsupported_contract_version" {
		t.Fatalf("want unsupported_contract_version, got %v", werr)
	}
	if werr.HTTPStatus() != 400 {
		t.Errorf("status = %d, want 400", werr.HTTPStatus())
	}
}
