package auth

import (
	"errors"
	"fmt"
	"strings"

	authhttp "github.com/open-rails/authkit/http"
)

// Verifier validates bearer tokens against configured issuers/JWKS.
type Verifier interface {
	Verify(token string) (authhttp.Claims, error)
}

// NewIssuerVerifier builds an AuthKit-backed verifier over an explicit issuer
// allowlist. This is only for embedded hosts that deliberately opt into trusting
// their own host-app JWTs; standalone OpenRails uses its control-plane verifier.
func NewIssuerVerifier(issuers []string, expectedAudience string) (Verifier, error) {
	if len(issuers) == 0 {
		return nil, errors.New("at least one auth issuer is required")
	}

	expectedAudience = strings.TrimSpace(expectedAudience)
	v := authhttp.NewVerifier()

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
		if err := v.AddIssuer(issuer, audiences, authhttp.IssuerOptions{
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

// FormatVerifierError normalises verifier error messages for HTTP responses.
func FormatVerifierError(err error) string {
	if err == nil {
		return ""
	}
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "missing_token"):
		return "missing_token"
	case strings.Contains(msg, "bad_issuer"):
		return "invalid_issuer"
	case strings.Contains(msg, "bad_audience"):
		return "invalid_audience"
	case strings.Contains(msg, "invalid_token"):
		return "invalid_or_expired_token"
	default:
		return msg
	}
}
