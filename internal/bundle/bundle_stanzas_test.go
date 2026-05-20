package bundle

import (
	"errors"
	"testing"

	"github.com/alternet-dev/wavefront/internal/bundletest"
	"github.com/alternet-dev/wavefront/internal/transform"
)

const stanzaBundle = `version: 1
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

func TestStanzasParseIntoOps(t *testing.T) {
	b, err := Load(bundletest.Dir(t, stanzaBundle))
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
	bad := `version: 1
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
	bad := `version: 1
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
	bad := `version: 1
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
	bad := `version: 1
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
	bad := `version: 1
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
	bad := `version: 1
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

func TestBundleWithoutStanzasHasNoOps(t *testing.T) {
	b, err := Load(bundletest.Dir(t, "")) // default ValidVersions: no stanzas
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	c, _ := b.Contract("2024-11")
	if len(c.RequestOps()) != 0 || len(c.ResponseOps()) != 0 {
		t.Error("a contract without stanzas must have zero ops")
	}
}
