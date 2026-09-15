package auth

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/open-rails/authkit/verify"
)

// RequestVerifier preserves AuthKit's credential, assurance and sender-proof
// restrictions by verifying the complete request.
type RequestVerifier interface {
	VerifyRequest(r *http.Request) (verify.Claims, error)
}

type RequestVerifierFunc func(*http.Request) (verify.Claims, error)

func (f RequestVerifierFunc) VerifyRequest(r *http.Request) (verify.Claims, error) {
	return f(r)
}

// NewIssuerVerifier builds an AuthKit-backed verifier over an explicit issuer
// allowlist. This is only for embedded hosts that deliberately opt into trusting
// their own host-app JWTs; standalone OpenRails uses its control-plane verifier.
func NewIssuerVerifier(issuers []string, expectedAudience string) (RequestVerifier, error) {
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
			// This explicit host allowlist owns the user namespace consumed by
			// the bridge. Stored merchant/application issuers never enter it.
			IsLocal: true,
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
