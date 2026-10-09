package billingauth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	auth "github.com/open-rails/helpers/auth"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/api"
)

// Authenticator verifies a standalone control-plane user session for the
// control plane's own routes (/v1/merchants, /v1/platform). Billing routes
// use Auth instead.
type Authenticator interface {
	Authenticate(ctx context.Context, r *http.Request) (UserContext, error)
}

// AuthenticatorFunc adapts an ordinary function to the [Authenticator]
// interface, so a host can pass a closure without declaring a type.
type AuthenticatorFunc func(ctx context.Context, r *http.Request) (UserContext, error)

// Authenticate implements [Authenticator].
func (f AuthenticatorFunc) Authenticate(ctx context.Context, r *http.Request) (UserContext, error) {
	return f(ctx, r)
}

// Optional is framework-neutral net/http middleware that attempts authentication
// but allows the request through with no UserContext when it fails. Mirrors
// authkit's http.Optional.
func Optional(a Authenticator) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if a != nil {
				if uc, err := a.Authenticate(r.Context(), r); err == nil && uc.ValidateSubject() == nil {
					r = r.WithContext(SetUserContext(r.Context(), uc))
				}
			}
			next.ServeHTTP(w, r)
		})
	}
}

// UnauthenticatedMessage maps an authentication error to a client-safe message:
// the generic "authentication required" for a nil/ErrUnauthenticated error, or
// the error's own text otherwise. Shared by the net/http, gin, and neutral-router
// auth middleware so the 401 message is identical across all surfaces.
func UnauthenticatedMessage(err error) string {
	if err == nil || errors.Is(err, ErrUnauthenticated) {
		return "authentication required"
	}
	return err.Error()
}

// Unauthenticated is the 401 for an authentication error: a refusal the
// authenticator already coded keeps its code, an expired or revoked credential
// gets its own, and anything else is authentication_required with the error's
// client-safe text.
func Unauthenticated(err error) GateError {
	var gate GateError
	switch {
	case errors.As(err, &gate) && gate.Status == http.StatusUnauthorized && gate.Code != "":
		return gate
	case errors.Is(err, auth.ErrExpired):
		return Refusal(billing.CodeCredentialExpired)
	case errors.Is(err, auth.ErrRevoked):
		return Refusal(billing.CodeCredentialRevoked)
	case errors.Is(err, auth.ErrSenderProofRequired):
		return Refusal(billing.CodeSenderProofRequired)
	}
	return Refusal(billing.CodeAuthenticationRequired, UnauthenticatedMessage(err))
}

// WriteJSONError writes the one OpenRails error envelope
// ({"error":{"type","code","message","request_id"}}) from a plain
// http.ResponseWriter, for middleware that answers before a handler exists.
// code is the stable machine code; the type is the status's category. The
// request id is the one the request-log middleware already put on the response.
func WriteJSONError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	body := api.NewAPIError(status, api.TypeForCode(status, code), code, message)
	if id := strings.TrimSpace(w.Header().Get("X-Request-ID")); id != "" {
		body.WithRequestID(id)
	}
	_ = json.NewEncoder(w).Encode(body.ToResponse())
}
