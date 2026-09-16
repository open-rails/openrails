package billingauth

import (
	"context"
	"fmt"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"

	"github.com/open-rails/openrails/internal/requestauth"
)

type cookieAdmissionKey struct{}

// CookieAuthentication opts a mounted OpenRails HTTP handler into host session
// cookies. The origin must be the browser-facing origin of this mount, supplied
// by trusted deployment configuration. HTTPS is required except for explicit
// localhost/loopback HTTP development origins. Every unsafe cookie request must carry
// that exact Origin; absent, opaque, cross-origin and sibling origins fail closed.
// Mount this OUTSIDE OpenRails' handler. GET/HEAD/OPTIONS must not mutate state.
// Explicit Authorization requests never fall back to cookie credentials.
func CookieAuthentication(origin string) (func(http.Handler) http.Handler, error) {
	if !validCookieOrigin(origin) {
		return nil, fmt.Errorf("cookie authentication requires a canonical HTTPS origin (HTTP is allowed only for localhost or loopback)")
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

// Accept the browser's serialized origin, so exact Origin matching cannot be
// misconfigured with wildcard, case, default-port or IP shorthand spellings.
func validCookieOrigin(origin string) bool {
	u, err := url.Parse(origin)
	if err != nil || u.User != nil || u.Path != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(origin, "#") || u.Opaque != "" {
		return false
	}
	host := u.Hostname()
	if host == "" || strings.ContainsAny(host, "*%") || host != strings.ToLower(host) {
		return false
	}
	canonicalHost := host
	addr, ipErr := netip.ParseAddr(host)
	if ipErr == nil {
		if addr.String() != host || addr.Is4In6() {
			return false
		}
		if addr.Is6() {
			canonicalHost = "[" + host + "]"
		}
	} else {
		if len(host) > 253 {
			return false
		}
		labels := strings.Split(strings.TrimSuffix(host, "."), ".")
		for _, label := range labels {
			if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
				return false
			}
			for _, c := range label {
				if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
					return false
				}
			}
		}
		last := labels[len(labels)-1]
		// Browsers interpret number-ending hosts as IPv4, including abbreviated,
		// octal and hexadecimal forms. Require the strict netip spelling above.
		if strings.Trim(last, "0123456789") == "" || strings.HasPrefix(last, "0x") {
			return false
		}
	}
	if u.Scheme != "https" {
		if u.Scheme != "http" || !(ipErr == nil && addr.IsLoopback() || host == "localhost" || strings.HasSuffix(host, ".localhost")) {
			return false
		}
	}
	if port := u.Port(); port != "" {
		value, err := strconv.Atoi(port)
		if err != nil || value < 1 || value > 65535 || strconv.Itoa(value) != port || u.Scheme == "https" && value == 443 || u.Scheme == "http" && value == 80 {
			return false
		}
		canonicalHost += ":" + port
	}
	return origin == u.Scheme+"://"+canonicalHost
}
