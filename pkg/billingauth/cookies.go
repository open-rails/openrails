package billingauth

import (
	"context"
	"fmt"
	"github.com/open-rails/openrails/internal/requestauth"
	"net/http"
	"net/url"
	"strings"
)

type cookieAdmissionKey struct{}

// CookieAuthentication opts a mounted OpenRails HTTP handler into host session
// cookies. The origin must be the browser-facing origin of this mount, supplied
// by trusted deployment configuration. Every unsafe cookie request must carry
// that exact Origin; absent, opaque, cross-origin and sibling origins fail closed.
// Mount this OUTSIDE OpenRails' handler. GET/HEAD/OPTIONS must not mutate state.
// Explicit Authorization requests never fall back to cookie credentials.
func CookieAuthentication(origin string) (func(http.Handler) http.Handler, error) {
	u, err := url.Parse(origin)
	if err != nil || u.Scheme != "https" && u.Scheme != "http" || u.Host == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(origin, "#") || u.Opaque != "" {
		return nil, fmt.Errorf("cookie authentication requires an absolute origin without a path")
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Cookie") != "" && strings.TrimSpace(r.Header.Get("Authorization")) == "" {
				switch r.Method {
				case http.MethodGet, http.MethodHead, http.MethodOptions:
				default:
					if len(r.Header.Values("Origin")) != 1 || r.Header.Get("Origin") != origin {
						WriteJSONError(w, http.StatusForbidden, "csrf_origin_denied", "cookie request origin is not allowed")
						return
					}
				}
				r = r.WithContext(context.WithValue(r.Context(), cookieAdmissionKey{}, true))
			}
			next.ServeHTTP(w, r)
		})
	}, nil
}

// ExplicitCredentials is the common HTTP authentication boundary used by all
// OpenRails mounts. Ambient cookies are unavailable to auth adapters unless
// CookieAuthentication has admitted the request. Hosts may use it on their own
// billing-adjacent routes to preserve the same credential selection.
func ExplicitCredentials(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r = requestauth.Begin(r)
		admitted, _ := r.Context().Value(cookieAdmissionKey{}).(bool)
		if r.Header.Get("Cookie") != "" && (!admitted || strings.TrimSpace(r.Header.Get("Authorization")) != "") {
			r = r.Clone(r.Context())
			r.Header.Del("Cookie")
		}
		next.ServeHTTP(w, r)
	})
}
