package bundle

import (
	"errors"
	"testing"

	"github.com/alternet-dev/wavefront/internal/bundletest"
	"github.com/alternet-dev/wavefront/internal/transform"
)

func TestStanzasParseIntoOps(t *testing.T) {
	dir := bundletest.Dir(t, "")
	bundletest.WriteResolution(t, dir, `version: 1
overrides:
  - contract_version: "2024-11"
    transform:
      request:
        - rename: { from: text, to: message }
        - coerce: { field: n, to: string }
      response:
        - optionalize: { field: at }
`)
	b, err := Load(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	c, ok := b.Contract("2024-11")
	if !ok {
		t.Fatal("contract missing")
	}
	r := c.RequestOps()
	if len(r) != 2 {
		t.Fatalf("request ops: got %d want 2 (%+v)", len(r), r)
	}
	if r[0].Kind != transform.KindRename || r[0].From.String() != "text" || r[0].To.String() != "message" {
		t.Errorf("rename op wrong: %+v", r[0])
	}
	if r[1].Kind != transform.KindCoerce || r[1].Field.String() != "n" || r[1].CoerceTo != "string" {
		t.Errorf("coerce op wrong: %+v", r[1])
	}
	resp := c.ResponseOps()
	if len(resp) != 1 || resp[0].Kind != transform.KindOptionalize || resp[0].Field.String() != "at" {
		t.Errorf("response ops wrong: %+v", resp)
	}
}

func TestUnknownVerbRejected(t *testing.T) {
	dir := bundletest.Dir(t, "")
	bundletest.WriteResolution(t, dir, `version: 1
overrides:
  - contract_version: "2024-11"
    transform:
      request:
        - frobnicate: { x: 1 }
`)
	_, err := Load(dir)
	var pe *ParseError
	if !errors.As(err, &pe) {
		t.Fatalf("unknown verb must be ParseError, got %v", err)
	}
}

func TestRenameFromEqualsToRejected(t *testing.T) {
	dir := bundletest.Dir(t, "")
	bundletest.WriteResolution(t, dir, `version: 1
overrides:
  - contract_version: "2024-11"
    transform:
      request:
        - rename: { from: text, to: text }
`)
	if _, err := Load(dir); err == nil {
		t.Fatal("from==to must be rejected at load")
	}
}

func TestCoerceToInvalidRejected(t *testing.T) {
	dir := bundletest.Dir(t, "")
	bundletest.WriteResolution(t, dir, `version: 1
overrides:
  - contract_version: "2024-11"
    transform:
      request:
        - coerce: { field: n, to: int }
`)
	if _, err := Load(dir); err == nil {
		t.Fatal("coerce.to=int must be rejected at load")
	}
}

func TestTwoVerbOpRejected(t *testing.T) {
	dir := bundletest.Dir(t, "")
	bundletest.WriteResolution(t, dir, `version: 1
overrides:
  - contract_version: "2024-11"
    transform:
      request:
        - rename: { from: a, to: b }
          default: { field: c, value: 1 }
`)
	_, err := Load(dir)
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("two-verb op must be ValidationError, got %v", err)
	}
}

func TestEmptyOpRejected(t *testing.T) {
	dir := bundletest.Dir(t, "")
	bundletest.WriteResolution(t, dir, `version: 1
overrides:
  - contract_version: "2024-11"
    transform:
      request:
        - {}
`)
	_, err := Load(dir)
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("empty op must be ValidationError, got %v", err)
	}
}

func TestNonScalarDefaultRejected(t *testing.T) {
	dir := bundletest.Dir(t, "")
	bundletest.WriteResolution(t, dir, `version: 1
overrides:
  - contract_version: "2024-11"
    transform:
      request:
        - default: { field: meta, value: { nested: 1 } }
`)
	_, err := Load(dir)
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("non-scalar default value must be a ValidationError at load, got %v", err)
	}
}

func TestBundleWithoutStanzasHasNoOps(t *testing.T) {
	b, err := Load(bundletest.Dir(t, "")) // default ValidVersions: no resolution.yaml
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	c, _ := b.Contract("2024-11")
	if len(c.RequestOps()) != 0 || len(c.ResponseOps()) != 0 {
		t.Error("a contract without stanzas must have zero ops")
	}
}

func TestCrossParentRenameRejected(t *testing.T) {
	dir := bundletest.Dir(t, "")
	bundletest.WriteResolution(t, dir, `version: 1
overrides:
  - contract_version: "2024-11"
    transform:
      request:
        - rename: { from: a.b, to: x.y }
`)
	_, err := Load(dir)
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("cross-parent rename must be ValidationError, got %v", err)
	}
}

func TestSameParentDifferentLeafRenameAccepted(t *testing.T) {
	// Single-segment paths (slice-1 case) — different leaves at same (empty) parent.
	dir := bundletest.Dir(t, "")
	bundletest.WriteResolution(t, dir, `version: 1
overrides:
  - contract_version: "2024-11"
    transform:
      request:
        - rename: { from: text, to: message }
`)
	if _, err := Load(dir); err != nil {
		t.Fatalf("same-parent rename must load, got %v", err)
	}
}

// The bundletest FDS has acme.v1.Ping{text string=1, n int32=2}.
// Reference an absent field — must fail at load.
func TestDescriptorCrossCheckRejectsAbsentField(t *testing.T) {
	dir := bundletest.Dir(t, "")
	bundletest.WriteResolution(t, dir, `version: 1
overrides:
  - contract_version: "2024-11"
    transform:
      request:
        - rename: { from: nonexistent, to: x }
`)
	_, err := Load(dir)
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("path naming an absent field must be ValidationError, got %v", err)
	}
}

// Ping.text is a scalar string; using it with [] should fail (not repeated).
func TestDescriptorCrossCheckRejectsScalarUsedAsArray(t *testing.T) {
	dir := bundletest.Dir(t, "")
	bundletest.WriteResolution(t, dir, `version: 1
overrides:
  - contract_version: "2024-11"
    transform:
      request:
        - rename:
            from: "text[].sub"
            to: "text[].alt"
`)
	_, err := Load(dir)
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("scalar treated as array must be ValidationError, got %v", err)
	}
}

// Ping.text is a scalar; using a nested path through it (`text.x`) should fail
// because intermediate must be TYPE_MESSAGE.
func TestDescriptorCrossCheckRejectsScalarAsObjectIntermediate(t *testing.T) {
	dir := bundletest.Dir(t, "")
	bundletest.WriteResolution(t, dir, `version: 1
overrides:
  - contract_version: "2024-11"
    transform:
      request:
        - rename: { from: text.x, to: text.y }
`)
	_, err := Load(dir)
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("scalar used as object intermediate must be ValidationError, got %v", err)
	}
}

// Internal-targeting paths (rename.to / default.field on request stanzas;
// rename.from / coerce.field / optionalize.field on response stanzas) get
// only grammar validation — they reference the internal shape, which we
// don't have descriptors for. So a bundle with a typo on the internal side
// LOADS fine here; the runtime upstream catches the mismatch.
func TestInternalSidePathsNotCrossChecked(t *testing.T) {
	dir := bundletest.Dir(t, "")
	bundletest.WriteResolution(t, dir, `version: 1
overrides:
  - contract_version: "2024-11"
    transform:
      request:
        - rename: { from: text, to: any_internal_name_we_dont_validate }
`)
	if _, err := Load(dir); err != nil {
		t.Fatalf("internal-side path must not be cross-checked at load; got %v", err)
	}
}

// Valid bundle: rename.from references a real field (text); rename.to is
// internal (not validated). Must load cleanly.
func TestValidExternalPathLoadsCleanly(t *testing.T) {
	dir := bundletest.Dir(t, "")
	bundletest.WriteResolution(t, dir, `version: 1
overrides:
  - contract_version: "2024-11"
    transform:
      request:
        - coerce: { field: n, to: string }
`)
	if _, err := Load(dir); err != nil {
		t.Fatalf("valid external path must load; got %v", err)
	}
}
