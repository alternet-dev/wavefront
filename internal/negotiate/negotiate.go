// Package negotiate resolves the contract the client declared. It matches
// the inbound (path, method, contract-version header) tuple verbatim against
// the loaded bundle; missing, blank, or unknown version ⇒ a typed
// unsupported_contract_version error, never a silent best-guess.
package negotiate

import (
	"strings"

	"github.com/alternet-dev/wavefront/internal/bundle"
	"github.com/alternet-dev/wavefront/internal/wireerror"
)

// Resolve returns the contract whose (route, method, contract_version)
// matches the inbound (path, method, version), or a typed wire-error.
//
// The proxy's first-pass route gate (Bundle.LookupRoute) has already
// confirmed that SOME contract in the bundle binds (path, method) before
// this function is called — its job is unknown_route. Resolve's job is
// version negotiation: given that the route is known overall, does the
// CLIENT-NAMED version serve it?
//
// Outcomes:
//   - version is missing/blank → unsupported_contract_version.
//   - version is not in the bundle at all → unsupported_contract_version.
//   - version is in the bundle but does not bind (path, method) →
//     unsupported_contract_version (NOT unknown_route). Rationale: the
//     route gate has already passed (some other version binds it), so the
//     route is known to the bundle; the failure is that this specific
//     version doesn't serve it. Telling the client "this version is
//     unsupported for this route" is more informative than "unknown route",
//     and consistent with the rule that version errors are 400 while
//     routing errors are 404.
//   - all three match → return the contract.
func Resolve(b *bundle.Bundle, path, method, headerValue string) (*bundle.Contract, *wireerror.Error) {
	v := strings.TrimSpace(headerValue)
	if v == "" {
		return nil, wireerror.UnsupportedContractVersion("missing contract-version header")
	}
	c, ok := b.Lookup(path, method, v)
	if ok {
		return c, nil
	}
	// Disambiguate the miss for the operator-readable message; both legs
	// emit the same wire-error code (unsupported_contract_version, 400).
	if _, knownVersion := b.Contract(v); !knownVersion {
		return nil, wireerror.UnsupportedContractVersion("contract version " + v + " is not in the bundle")
	}
	return nil, wireerror.UnsupportedContractVersion(
		"contract version " + v + " does not bind " + method + " " + path,
	)
}
