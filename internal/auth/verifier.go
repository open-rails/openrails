package auth

import (
	"net/http"

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
