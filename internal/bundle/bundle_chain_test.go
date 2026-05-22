package bundle

import (
	"errors"
	"testing"

	"github.com/alternet-dev/wavefront/internal/bundletest"
)

// mk3Layer builds a three-layer bundle directory: 2024-01, 2024-06, 2024-12.
// Each layer holds the shared FDSBytes and binds one contract version to
// POST /v3/echo acme.v1.Ping → acme.v1.Pong. The bundle root is returned.
func mk3Layer(t *testing.T) string {
	t.Helper()
	fds := bundletest.FDSBytes(t)
	mk := func(cv string) bundletest.Layer {
		return bundletest.Layer{
			Name:        cv,
			Descriptors: fds,
			OpenAPI:     bundletest.ValidOpenAPI,
			Versions: "version: 1\ncontracts:\n  - contract_version: \"" + cv + "\"\n" +
				"    route: /v3/echo\n    method: POST\n" +
				"    request_message: acme.v1.Ping\n    response_message: acme.v1.Pong\n",
		}
	}
	return bundletest.MultiDir(t, mk("2024-01"), mk("2024-06"), mk("2024-12"))
}

// TestLoadChainResolves asserts that a three-version bundle with transform.target
// links resolves the chain: 2024-01 → 2024-06 → 2024-12 (terminal).
func TestLoadChainResolves(t *testing.T) {
	dir := mk3Layer(t)
	// Both links rename text (a real acme.v1.Ping field) to an internal name
	// (not cross-checked). 2024-12 has no override and is the terminal.
	bundletest.WriteResolution(t, dir, `version: 1
overrides:
  - contract_version: "2024-01"
    transform:
      request:
        - rename: { from: text, to: message }
      target: "2024-06"
  - contract_version: "2024-06"
    transform:
      request:
        - rename: { from: text, to: body }
      target: "2024-12"
`)
	b, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	c01, ok := b.Contract("2024-01")
	if !ok {
		t.Fatal("2024-01 contract not found")
	}
	c06, ok := b.Contract("2024-06")
	if !ok {
		t.Fatal("2024-06 contract not found")
	}
	c12, ok := b.Contract("2024-12")
	if !ok {
		t.Fatal("2024-12 contract not found")
	}

	// 2024-01's chain: [2024-01, 2024-06, 2024-12]
	chain01 := c01.Chain()
	if len(chain01) != 3 {
		t.Fatalf("2024-01 chain length = %d, want 3; chain = %v", len(chain01), contractVersions(chain01))
	}
	if chain01[0] != c01 || chain01[1] != c06 || chain01[2] != c12 {
		t.Errorf("2024-01 chain order wrong: %v", contractVersions(chain01))
	}

	// 2024-06's chain: [2024-06, 2024-12]
	chain06 := c06.Chain()
	if len(chain06) != 2 {
		t.Fatalf("2024-06 chain length = %d, want 2; chain = %v", len(chain06), contractVersions(chain06))
	}
	if chain06[0] != c06 || chain06[1] != c12 {
		t.Errorf("2024-06 chain order wrong: %v", contractVersions(chain06))
	}

	// 2024-12 has no transform override, so its chain is just itself.
	chain12 := c12.Chain()
	if len(chain12) != 1 {
		t.Fatalf("2024-12 chain length = %d, want 1; chain = %v", len(chain12), contractVersions(chain12))
	}
	if chain12[0] != c12 {
		t.Errorf("2024-12 chain[0] wrong: %v", chain12[0].ContractVersion())
	}

	// TransformTarget accessors
	if got := c01.TransformTarget(); got != "2024-06" {
		t.Errorf("2024-01 TransformTarget() = %q, want 2024-06", got)
	}
	if got := c06.TransformTarget(); got != "2024-12" {
		t.Errorf("2024-06 TransformTarget() = %q, want 2024-12", got)
	}
	if got := c12.TransformTarget(); got != "" {
		t.Errorf("2024-12 TransformTarget() = %q, want empty", got)
	}
}

// TestLoadChainUnknownTargetRejected ensures a transform.target naming an
// absent version is a hard *ValidationError at load time.
func TestLoadChainUnknownTargetRejected(t *testing.T) {
	dir := mk3Layer(t)
	bundletest.WriteResolution(t, dir, `version: 1
overrides:
  - contract_version: "2024-01"
    transform:
      request:
        - rename: { from: text, to: msg }
      target: "9999-99"
`)
	_, err := Load(dir)
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("unknown transform target must be *ValidationError, got %v", err)
	}
	if ve.Field != "transform.target" {
		t.Errorf("ValidationError.Field = %q, want transform.target", ve.Field)
	}
}

// TestLoadChainCycleRejected ensures a cycle between transform targets (A→B,
// B→A) is caught at load time as a *ValidationError.
func TestLoadChainCycleRejected(t *testing.T) {
	fds := bundletest.FDSBytes(t)
	mk := func(cv string) bundletest.Layer {
		return bundletest.Layer{
			Name:        cv,
			Descriptors: fds,
			OpenAPI:     bundletest.ValidOpenAPI,
			Versions: "version: 1\ncontracts:\n  - contract_version: \"" + cv + "\"\n" +
				"    route: /v3/echo\n    method: POST\n" +
				"    request_message: acme.v1.Ping\n    response_message: acme.v1.Pong\n",
		}
	}
	dir := bundletest.MultiDir(t, mk("2024-01"), mk("2024-06"))
	bundletest.WriteResolution(t, dir, `version: 1
overrides:
  - contract_version: "2024-01"
    transform:
      request:
        - rename: { from: text, to: message }
      target: "2024-06"
  - contract_version: "2024-06"
    transform:
      request:
        - rename: { from: text, to: body }
      target: "2024-01"
`)
	_, err := Load(dir)
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("cycle must be *ValidationError, got %v", err)
	}
	if ve.Field != "transform.target" {
		t.Errorf("ValidationError.Field = %q, want transform.target", ve.Field)
	}
}

func contractVersions(chain []*Contract) []string {
	out := make([]string, len(chain))
	for i, c := range chain {
		out[i] = c.ContractVersion()
	}
	return out
}
