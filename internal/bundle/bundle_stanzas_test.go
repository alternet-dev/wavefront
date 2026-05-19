package bundle

import (
	"errors"
	"testing"

	"github.com/alternet-dev/wavefront/internal/bundletest"
)

const v2OK = `version: 2
contracts:
  - contract_version: "2024-11"
    route: /v3/echo
    method: POST
    request_message: acme.v1.Ping
    response_message: acme.v1.Pong
    request:
      - rename: { from: text, to: message }
      - coerce: { field: n, to: string }
    response:
      - optionalize: { field: at }
`

func TestV2BundleParsesOps(t *testing.T) {
	b, err := Load(bundletest.Dir(t, v2OK))
	if err != nil {
		t.Fatalf("v2 load: %v", err)
	}
	c, ok := b.Contract("2024-11")
	if !ok {
		t.Fatal("contract missing")
	}
	if len(c.RequestOps()) != 2 || c.RequestOps()[0].Kind != "rename" {
		t.Errorf("request ops wrong: %+v", c.RequestOps())
	}
	if len(c.ResponseOps()) != 1 || c.ResponseOps()[0].Kind != "optionalize" {
		t.Errorf("response ops wrong: %+v", c.ResponseOps())
	}
	r := c.RequestOps()
	if r[0].Kind != "rename" || r[0].From != "text" || r[0].To != "message" {
		t.Errorf("rename op wrong: %+v", r[0])
	}
	if r[1].Kind != "coerce" || r[1].Field != "n" || r[1].CoerceTo != "string" {
		t.Errorf("coerce op wrong: %+v", r[1])
	}
	if op := c.ResponseOps()[0]; op.Kind != "optionalize" || op.Field != "at" {
		t.Errorf("optionalize op wrong: %+v", op)
	}
}

func TestV1WithStanzasRejected(t *testing.T) {
	bad := `version: 1
contracts:
  - contract_version: "2024-11"
    route: /v3/echo
    method: POST
    request_message: acme.v1.Ping
    response_message: acme.v1.Pong
    request:
      - rename: { from: text, to: message }
`
	_, err := Load(bundletest.Dir(t, bad))
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("want ValidationError, got %v", err)
	}
}

func TestUnknownVerbRejected(t *testing.T) {
	bad := `version: 2
contracts:
  - contract_version: "2024-11"
    route: /v3/echo
    method: POST
    request_message: acme.v1.Ping
    response_message: acme.v1.Pong
    request:
      - frobnicate: { x: 1 }
`
	_, err := Load(bundletest.Dir(t, bad))
	var pe *ParseError
	if !errors.As(err, &pe) {
		t.Fatalf("unknown verb must be ParseError, got %v", err)
	}
}

func TestRenameFromEqualsToRejected(t *testing.T) {
	bad := `version: 2
contracts:
  - contract_version: "2024-11"
    route: /v3/echo
    method: POST
    request_message: acme.v1.Ping
    response_message: acme.v1.Pong
    request:
      - rename: { from: text, to: text }
`
	if _, err := Load(bundletest.Dir(t, bad)); err == nil {
		t.Fatal("from==to must be rejected at load")
	}
}

func TestCoerceToInvalidRejected(t *testing.T) {
	bad := `version: 2
contracts:
  - contract_version: "2024-11"
    route: /v3/echo
    method: POST
    request_message: acme.v1.Ping
    response_message: acme.v1.Pong
    request:
      - coerce: { field: n, to: int }
`
	if _, err := Load(bundletest.Dir(t, bad)); err == nil {
		t.Fatal("coerce.to=int must be rejected at load")
	}
}

func TestTwoVerbOpRejected(t *testing.T) {
	bad := `version: 2
contracts:
  - contract_version: "2024-11"
    route: /v3/echo
    method: POST
    request_message: acme.v1.Ping
    response_message: acme.v1.Pong
    request:
      - rename: { from: a, to: b }
        default: { field: c, value: 1 }
`
	_, err := Load(bundletest.Dir(t, bad))
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("two-verb op must be ValidationError, got %v", err)
	}
}

func TestEmptyOpRejected(t *testing.T) {
	bad := `version: 2
contracts:
  - contract_version: "2024-11"
    route: /v3/echo
    method: POST
    request_message: acme.v1.Ping
    response_message: acme.v1.Pong
    request:
      - {}
`
	_, err := Load(bundletest.Dir(t, bad))
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("empty op must be ValidationError, got %v", err)
	}
}

func TestNonScalarDefaultRejected(t *testing.T) {
	bad := `version: 2
contracts:
  - contract_version: "2024-11"
    route: /v3/echo
    method: POST
    request_message: acme.v1.Ping
    response_message: acme.v1.Pong
    request:
      - default: { field: meta, value: { nested: 1 } }
`
	_, err := Load(bundletest.Dir(t, bad))
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("non-scalar default value must be a ValidationError at load, got %v", err)
	}
}

func TestV1BundleStillLoadsUnderV2Binary(t *testing.T) {
	b, err := Load(bundletest.Dir(t, "")) // ValidVersions == version: 1, no stanzas
	if err != nil {
		t.Fatalf("v1 bundle must still load: %v", err)
	}
	c, _ := b.Contract("2024-11")
	if len(c.RequestOps()) != 0 || len(c.ResponseOps()) != 0 {
		t.Error("v1 contract must have zero ops")
	}
}
