package billingauth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
)

// Authenticator is the framework-neutral auth boundary for embedded OpenRails.
//
// A host application that does NOT use AuthKit (or any particular token format)
// implements this single method to verify the incoming request however it likes
// — a JWT, a session cookie, an opaque API key, an mTLS / gateway header,
// anything — and return the resulting UserContext. It is pure net/http:
// implementers never touch gin, context keys, or status codes. OpenRails adapts
// it into the middleware it needs ([Required]/[Optional] here for the net/http
// form; the gin bridge lives app-side in pkg/authprovider).
//
// Return [ErrUnauthenticated] (or any non-nil error) when the request carries no
// valid credential. OpenRails maps that to 401 on required routes and to
// anonymous access on optional routes. A returned error's message is surfaced on
// required routes, so it should be safe to expose to clients.
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

// Required is framework-neutral net/http middleware that authenticates via a and
// rejects the request with 401 when authentication fails. On success the
// resulting UserContext is stored in the request context (read it with
// [FromContext]). This mirrors authkit's http.Required and lets a host mount
// billing routes into any router (net/http, gin via gin.WrapH, chi, …).
func Required(a Authenticator) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if a == nil {
				WriteJSONError(w, http.StatusInternalServerError, "server_error", "authentication disabled")
				return
			}
			uc, err := a.Authenticate(r.Context(), r)
			if err != nil {
				WriteJSONError(w, http.StatusUnauthorized, "unauthorized", unauthenticatedMessage(err))
				return
			}
			next.ServeHTTP(w, r.WithContext(SetUserContext(r.Context(), uc)))
		})
	}
}

// Optional is framework-neutral net/http middleware that attempts authentication
// but allows the request through with no UserContext when it fails. Mirrors
// authkit's http.Optional.
func Optional(a Authenticator) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if a != nil {
				if uc, err := a.Authenticate(r.Context(), r); err == nil {
					r = r.WithContext(SetUserContext(r.Context(), uc))
				}
			}
			next.ServeHTTP(w, r)
		})
	}
}

func unauthenticatedMessage(err error) string {
	if err == nil || errors.Is(err, ErrUnauthenticated) {
		return "authentication required"
	}
	return err.Error()
}

// WriteJSONError writes the standard OpenRails error envelope to a plain
// http.ResponseWriter. It is the net/http counterpart of the gin response
// helpers; the full neutral response layer is tracked under issue #283.
func WriteJSONError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"object": "error",
		"error": map[string]any{
			"type":    code,
			"message": message,
		},
	})
}
