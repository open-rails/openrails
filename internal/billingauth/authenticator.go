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

// Authenticator verifies a session of the standalone server's own accounts,
// which its Auth admits on the staff routes.
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

// Optional is net/http middleware that attempts authentication and lets the
// request through without a UserContext when it fails.
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

// UnauthenticatedMessage is the client-safe 401 message for err: "authentication
// required" for nil or ErrUnauthenticated, else err's own text.
func UnauthenticatedMessage(err error) string {
	if err == nil || errors.Is(err, ErrUnauthenticated) {
		return "authentication required"
	}
	return err.Error()
}

// Unauthenticated is the 401 for an authentication error: a coded 401 keeps
// its code, an expired, revoked or unproven credential gets its own, and
// anything else is authentication_required.
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

// WriteJSONError writes the OpenRails error envelope from a plain
// http.ResponseWriter, for middleware that answers before a handler exists,
// with the request id the request-log middleware set on the response.
func WriteJSONError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	body := api.NewAPIError(status, api.TypeForCode(status, code), code, message)
	if id := strings.TrimSpace(w.Header().Get("X-Request-ID")); id != "" {
		body.WithRequestID(id)
	}
	_ = json.NewEncoder(w).Encode(body.ToResponse())
}
