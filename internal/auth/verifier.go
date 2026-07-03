package auth

import (
	"errors"
	"fmt"
	"strings"

	"github.com/open-rails/authkit/verify"
)

// Verifier validates bearer tokens against configured issuers/JWKS.
type Verifier interface {
	Verify(token string) (verify.Claims, error)
}

// NewIssuerVerifier builds an AuthKit-backed verifier over an explicit issuer
// allowlist. This is only for embedded hosts that deliberately opt into trusting
// their own host-app JWTs; standalone OpenRails uses its control-plane verifier.
func NewIssuerVerifier(issuers []string, expectedAudience string) (Verifier, error) {
	if len(issuers) == 0 {
		return nil, errors.New("at least one auth issuer is required")
	}

	expectedAudience = strings.TrimSpace(expectedAudience)
	v := verify.NewVerifier()

	addedIssuers := 0
	for _, issuer := range issuers {
		issuer = strings.TrimRight(strings.TrimSpace(issuer), "/")
		if issuer == "" {
			continue
		}
		var audiences []string
		if expectedAudience != "" {
			audiences = []string{expectedAudience}
		}
		if err := v.AddIssuer(issuer, audiences, verify.IssuerOptions{
			JWKSURI: issuer + "/.well-known/jwks.json",
		}); err != nil {
			return nil, fmt.Errorf("add auth issuer %q: %w", issuer, err)
		}
		addedIssuers++
	}
	if addedIssuers == 0 {
		return nil, errors.New("at least one non-empty auth issuer is required")
	}

	return v, nil
}
