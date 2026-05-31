package bundle_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/alternet-dev/wavefront/internal/bundle"
	"github.com/alternet-dev/wavefront/internal/bundletest"
)

// versionsWithErrors binds the fixture's POST /v3/echo plus two declared error
// types drawn from the fixture FileDescriptorSet (Item, Meta). Any message
// resolves — the data plane is generic over descriptors — so the fixture's
// existing types stand in for real domain error types.
const versionsWithErrors = `version: 1
contracts:
  - contract_version: "2024-11"
    route: /v3/echo
    method: POST
    request_message: acme.v1.Ping
    response_message: acme.v1.Pong
    error_messages:
      "409": acme.v1.Item
      "422": acme.v1.Meta
`

func TestLoadBindsErrorMessages(t *testing.T) {
	b, err := bundle.Load(bundletest.Dir(t, versionsWithErrors))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	c, ok := b.LookupRoute("/v3/echo", "POST")
	if !ok {
		t.Fatal("route /v3/echo POST not bound")
	}
	if name, ok := c.ErrorMessage(409); !ok || name != "acme.v1.Item" {
		t.Errorf("ErrorMessage(409) = %q, %v; want acme.v1.Item, true", name, ok)
	}
	if name, ok := c.ErrorMessage(422); !ok || name != "acme.v1.Meta" {
		t.Errorf("ErrorMessage(422) = %q, %v; want acme.v1.Meta, true", name, ok)
	}
	if _, ok := c.ErrorMessage(404); ok {
		t.Error("ErrorMessage(404) present; want absent (undeclared)")
	}
	if c.Strict() {
		t.Error("Strict() = true; want false by default")
	}
}

func TestLoadRejectsInvalidErrorMessages(t *testing.T) {
	base := `version: 1
contracts:
  - contract_version: "2024-11"
    route: /v3/echo
    method: POST
    request_message: acme.v1.Ping
    response_message: acme.v1.Pong
    error_messages:
`
	cases := []struct {
		name   string
		tail   string
		assert func(t *testing.T, err error)
	}{
		{
			name: "2xx key",
			tail: "      \"200\": acme.v1.Item\n",
			assert: func(t *testing.T, err error) {
				t.Helper()
				var ve *bundle.ValidationError
				if !errors.As(err, &ve) || ve.Field != "error_messages" {
					t.Fatalf("want ValidationError(field=error_messages), got %v", err)
				}
			},
		},
		{
			name: "ceiling key",
			tail: "      \"206\": acme.v1.Item\n",
			assert: func(t *testing.T, err error) {
				t.Helper()
				var ve *bundle.ValidationError
				if !errors.As(err, &ve) || ve.Field != "error_messages" {
					t.Fatalf("want ValidationError(field=error_messages), got %v", err)
				}
			},
		},
		{
			name: "redirect key",
			tail: "      \"301\": acme.v1.Item\n",
			assert: func(t *testing.T, err error) {
				t.Helper()
				var ve *bundle.ValidationError
				if !errors.As(err, &ve) || ve.Field != "error_messages" {
					t.Fatalf("want ValidationError(field=error_messages), got %v", err)
				}
			},
		},
		{
			name: "non-numeric key",
			tail: "      \"default\": acme.v1.Item\n",
			assert: func(t *testing.T, err error) {
				t.Helper()
				var ve *bundle.ValidationError
				if !errors.As(err, &ve) || ve.Field != "error_messages" {
					t.Fatalf("want ValidationError(field=error_messages), got %v", err)
				}
			},
		},
		{
			name: "unresolvable",
			tail: "      \"409\": acme.v1.DoesNotExist\n",
			assert: func(t *testing.T, err error) {
				t.Helper()
				var me *bundle.MessageNotFoundError
				if !errors.As(err, &me) || me.Message != "acme.v1.DoesNotExist" {
					t.Fatalf("want MessageNotFoundError(acme.v1.DoesNotExist), got %v", err)
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := bundle.Load(bundletest.Dir(t, base+tc.tail))
			if err == nil {
				t.Fatalf("Load accepted invalid error_messages (%s); want error", tc.name)
			}
			tc.assert(t, err)
		})
	}
}

func TestLoadBindsNonPassthroughStatus(t *testing.T) {
	// The loader must accept error_messages keys that are outside the proxy's
	// typical passthrough set. 503 is not in {401,403,404,405,409,410,422,429,451}
	// and is not 2xx or a capability ceiling, so it must load without error and
	// be retrievable via ErrorMessage.
	versions := `version: 1
contracts:
  - contract_version: "2024-11"
    route: /v3/echo
    method: POST
    request_message: acme.v1.Ping
    response_message: acme.v1.Pong
    error_messages:
      "503": acme.v1.Item
`
	b, err := bundle.Load(bundletest.Dir(t, versions))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	c, ok := b.LookupRoute("/v3/echo", "POST")
	if !ok {
		t.Fatal("route /v3/echo POST not bound")
	}
	if name, ok := c.ErrorMessage(503); !ok || name != "acme.v1.Item" {
		t.Errorf("ErrorMessage(503) = %q, %v; want acme.v1.Item, true", name, ok)
	}
}

func TestLoadParsesStrictFlag(t *testing.T) {
	versions := strings.Replace(versionsWithErrors,
		"    error_messages:\n      \"409\": acme.v1.Item\n      \"422\": acme.v1.Meta\n",
		"    strict: true\n", 1)
	b, err := bundle.Load(bundletest.Dir(t, versions))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	c, _ := b.LookupRoute("/v3/echo", "POST")
	if !c.Strict() {
		t.Error("Strict() = false; want true")
	}
}
