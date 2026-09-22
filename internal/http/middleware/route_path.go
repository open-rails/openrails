package middleware

import (
	"context"
	"net/http"
)

type routePathKey struct{}

// WithRoutePath pins the registered canonical path for rate-limit and captcha
// policy when a host mounts billing under its own prefix. The request URL is
// preserved for authentication, sender proofs, and host permission checks.
// canonicalPath is a path template, without its HTTP method.
func WithRoutePath(canonicalPath string) HTTPMiddleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := context.WithValue(r.Context(), routePathKey{}, canonicalPath)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func policyRequestPath(r *http.Request) string {
	if path, ok := r.Context().Value(routePathKey{}).(string); ok && path != "" {
		return path
	}
	return r.URL.Path
}
