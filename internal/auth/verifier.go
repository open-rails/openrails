package auth

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/open-rails/authkit/verify"
	"github.com/open-rails/openrails/pkg/billingauth"
	log "github.com/sirupsen/logrus"
)

// Verifier validates bearer tokens against configured issuers/JWKS.
type Verifier interface {
	Verify(token string) (verify.Claims, error)
}

// RequestVerifier validates the credential presented on a whole request.
// authkit's *verify.Verifier satisfies it (VerifyRequest), which is how an
// embedded host injects the verifier it already uses everywhere else: the
// request shape carries the host's full credential chain (API-key branch, 2FA
// enrollment gate, delegated-issuer enrichment), none of which a bare
// Verify(token) sees.
type RequestVerifier interface {
	VerifyRequest(r *http.Request) (verify.Claims, error)
}

// RequestVerifierFor adapts a token-shaped Verifier (the JWKS issuer verifier
// this package builds) to the request shape: parse the bearer header, verify
// the token. Deliberately NOT an upgrade to the verifier's own VerifyRequest
// when it has one — the issuer path verifies REMOTE tokens and must keep the
// exact semantics it documents, while an injected host verifier is used
// verbatim.
func RequestVerifierFor(v Verifier) RequestVerifier {
	if v == nil {
		return nil
	}
	return tokenRequestVerifier{v: v}
}

type tokenRequestVerifier struct{ v Verifier }

func (t tokenRequestVerifier) VerifyRequest(r *http.Request) (verify.Claims, error) {
	if r == nil {
		return verify.Claims{}, billingauth.ErrUnauthenticated
	}
	token := bearerToken(r.Header.Get("Authorization"))
	if token == "" {
		return verify.Claims{}, billingauth.ErrUnauthenticated
	}
	cl, err := t.v.Verify(token)
	if err != nil {
		// Non-JWT bearers (API keys, etc.) reach this verifier as an expected
		// fallback in the credential chain — debug, not warn (#845).
		if looksLikeJWT(token) {
			log.WithError(err).Warn("jwt verification failed")
		} else {
			log.WithError(err).Debug("bearer token is not a jwt; jwt verification skipped")
		}
		return verify.Claims{}, err
	}
	return cl, nil
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
