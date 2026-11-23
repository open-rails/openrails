package middleware

import (
	"context"
	"net/http"

	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/logx"
)

type routePathKey struct{}

// WithRoutePath pins the registered canonical path for rate-limit and captcha
// policy when a host mounts billing under its own prefix. The request URL is
// preserved for authentication, sender proofs, and host permission checks.
// canonicalPath is a path template, without its HTTP method. The route and
// the request's id label what it logs.
func WithRoutePath(canonicalPath string) HTTPMiddleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := logx.With(r.Context(), "route", canonicalPath, "request_id", httprequest.EnsureRequestID(r))
			ctx = context.WithValue(ctx, routePathKey{}, canonicalPath)
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
