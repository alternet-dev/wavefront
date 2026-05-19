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
	if _, err := Load(bundletest.Dir(t, bad)); err == nil {
		t.Fatal("unknown verb must be rejected by strict decode")
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
