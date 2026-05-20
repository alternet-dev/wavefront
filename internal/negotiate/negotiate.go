// Package negotiate resolves the contract the client declared. It matches
// the contract-version header value verbatim against the loaded bundle;
// missing, blank, or unknown ⇒ a typed unsupported_contract_version error,
// never a silent best-guess.
package negotiate

import (
	"strings"

	"github.com/alternet-dev/wavefront/internal/bundle"
	"github.com/alternet-dev/wavefront/internal/wireerror"
)

// Resolve returns the contract for headerValue, or a typed error.
func Resolve(b *bundle.Bundle, headerValue string) (*bundle.Contract, *wireerror.Error) {
	v := strings.TrimSpace(headerValue)
	if v == "" {
		return nil, wireerror.UnsupportedContractVersion("missing contract-version header")
	}
	c, ok := b.Contract(v)
	if !ok {
		return nil, wireerror.UnsupportedContractVersion("contract version " + v + " is not in the bundle")
	}
	return c, nil
}
