package bundle_test

import (
	"testing"

	"github.com/alternet-dev/wavefront/internal/bundle"
	"github.com/alternet-dev/wavefront/internal/bundletest"
)

const veWithError = `version: 1
contracts:
  - contract_version: "2024-11"
    route: /v3/echo
    method: POST
    request_message: acme.v1.Ping
    response_message: acme.v1.Pong
    error_messages:
      "409": acme.v1.Item
`

// A resolution override may carry per-status error_responses ops, validated
// against that status's bound error message. acme.v1.Item has an `id` field that
// acme.v1.Pong (the 2xx response_message) lacks, so renaming the upstream
// `detail` leaf onto `id` is a legal response rename AND proves rename.to is
// cross-checked against the bound error message, not response_message.
func TestLoadBindsErrorResponseOps(t *testing.T) {
	dir := bundletest.Dir(t, veWithError)
	bundletest.WriteResolution(t, dir, `version: 1
overrides:
  - contract_version: "2024-11"
    transform:
      error_responses:
        "409":
          - rename:
              from: detail
              to: id
`)
	b, err := bundle.Load(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	c, _ := b.LookupRoute("/v3/echo", "POST")
	if got := len(c.ErrorResponseOps(409)); got != 1 {
		t.Fatalf("ErrorResponseOps(409) len = %d, want 1", got)
	}
	if got := c.ErrorResponseOps(404); got != nil {
		t.Errorf("ErrorResponseOps(404) = %v, want nil (no ops for an unbound status)", got)
	}
}

// TestLoadRejectsErrorResponseOpAgainstResponseMessageField proves the cross-check
// targets the bound error message, not the 2xx response_message: `at` is a field
// of acme.v1.Pong (response_message) but absent from acme.v1.Item (the 409 error
// message), so a rename onto `at` must be rejected at load.
func TestLoadRejectsErrorResponseOpAgainstResponseMessageField(t *testing.T) {
	dir := bundletest.Dir(t, veWithError)
	bundletest.WriteResolution(t, dir, `version: 1
overrides:
  - contract_version: "2024-11"
    transform:
      error_responses:
        "409":
          - rename:
              from: detail
              to: at
`)
	if _, err := bundle.Load(dir); err == nil {
		t.Fatal("Load accepted rename onto `at` (a response_message field absent from the error message acme.v1.Item); want error")
	}
}

// error_responses keyed by an undeclared status (not in error_messages) or a
// non-numeric key is rejected at load — there is no bound error message to
// validate the ops against.
func TestLoadRejectsErrorResponsesForUndeclaredStatus(t *testing.T) {
	cases := map[string]string{
		"undeclared status": `version: 1
overrides:
  - contract_version: "2024-11"
    transform:
      error_responses:
        "404":
          - optionalize: {field: detail}
`,
		"non-numeric key": `version: 1
overrides:
  - contract_version: "2024-11"
    transform:
      error_responses:
        "default":
          - optionalize: {field: detail}
`,
	}
	for name, res := range cases {
		t.Run(name, func(t *testing.T) {
			dir := bundletest.Dir(t, veWithError)
			bundletest.WriteResolution(t, dir, res)
			if _, err := bundle.Load(dir); err == nil {
				t.Fatalf("Load accepted error_responses (%s); want error", name)
			}
		})
	}
}
