package middleware

import (
	"context"
	"net/http"
	"strings"

	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/logx"
)

type routePathKey struct{}

// WithRoutePath pins the route catalog's path, without the host's mount
// prefix, for rate-limit and captcha policy; the request URL is preserved for
// authentication, sender proofs, and host permission checks. canonicalPath is
// a path template, without its HTTP method. The route and the request's id
// label what it logs.
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

// loggedPath is r's path as a log may hold it: a webhook URL's token segment,
// a CCBill PSP's secret, is replaced.
func loggedPath(r *http.Request) string {
	before, rest, found := strings.Cut(r.URL.Path, "/v1/webhooks/")
	if !found {
		return r.URL.Path
	}
	if segments := strings.SplitN(rest, "/", 3); len(segments) == 3 {
		return before + "/v1/webhooks/" + segments[0] + "/" + segments[1] + "/{token}"
	}
	return r.URL.Path
}
