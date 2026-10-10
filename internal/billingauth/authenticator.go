package billingauth

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/open-rails/openrails/internal/api"
)

// SessionAuthenticator verifies a session of the standalone server's own
// accounts, for the rate limiter's key and the server's user methods.
type SessionAuthenticator interface {
	Authenticate(ctx context.Context, r *http.Request) (UserContext, error)
}

// SessionAuthenticatorFunc adapts an ordinary function to the
// [SessionAuthenticator] interface.
type SessionAuthenticatorFunc func(ctx context.Context, r *http.Request) (UserContext, error)

// Authenticate implements [SessionAuthenticator].
func (f SessionAuthenticatorFunc) Authenticate(ctx context.Context, r *http.Request) (UserContext, error) {
	return f(ctx, r)
}

// Optional is net/http middleware that attempts authentication and lets the
// request through without a UserContext when it fails.
func Optional(a SessionAuthenticator) func(http.Handler) http.Handler {
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
