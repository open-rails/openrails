package middleware

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/open-rails/openrails/pkg/tenant"
)

// This file holds the gin-free net/http analogues of the global engine
// middleware (issue #282). The embedded surface (server.NewHTTPHandler) wraps its
// ServeMux with these so it can run security headers, body limits, CORS, and
// tenant resolution WITHOUT importing gin. The standalone gin server keeps using
// the gin versions in security.go / tenant.go.
//
// NOTE: rate-limiting + the captcha challenge flow (RateLimitWithChallengeStore)
// are intentionally NOT ported here. They are gin- and captcha-flow-coupled, and
// embedded hosts front billing with their own gateway/rate-limiting. The
// standalone gin surface retains full rate-limiting.

// HTTPMiddleware is a standard net/http middleware (outermost wrapper).
type HTTPMiddleware func(http.Handler) http.Handler

// ChainHTTP composes mw around h so mw[0] is the outermost wrapper (runs first).
func ChainHTTP(h http.Handler, mw ...HTTPMiddleware) http.Handler {
	for i := len(mw) - 1; i >= 0; i-- {
		h = mw[i](h)
	}
	return h
}

// SecurityHeadersHTTP is the net/http analogue of SecurityHeaders.
func SecurityHeadersHTTP() HTTPMiddleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h := w.Header()
			h.Set("X-Frame-Options", "DENY")
			h.Set("X-Content-Type-Options", "nosniff")
			h.Set("X-XSS-Protection", "1; mode=block")
			h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
			if !isDebugNMITokenizationPath(r.URL.Path) {
				h.Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; connect-src 'self'; font-src 'self'")
			}
			h.Set("Server", "")
			next.ServeHTTP(w, r)
		})
	}
}

// BodyLimitHTTP is the net/http analogue of BodyLimit. Webhook paths are exempt
// (their bodies are verified by signature, may be large, and must be read raw).
func BodyLimitHTTP(maxBytes int64) HTTPMiddleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if maxBytes > 0 && r.Body != nil && !isWebhookPath(r.URL.Path) {
				r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
			}
			next.ServeHTTP(w, r)
		})
	}
}

// CORSHTTP is the net/http analogue of CORS. It mirrors the gin CORS policy:
// the same allowed headers/methods, exposed headers, credentials, and max-age.
// When allowedOrigins is empty it allows all origins (development); otherwise it
// reflects the request Origin only when it is in the allow-list. OPTIONS
// preflight is answered with 204.
func CORSHTTP(allowedOrigins []string) HTTPMiddleware {
	allowAll := len(allowedOrigins) == 0
	allowed := make(map[string]struct{}, len(allowedOrigins))
	for _, o := range allowedOrigins {
		allowed[strings.TrimSpace(o)] = struct{}{}
	}
	const (
		allowHeaders  = "Origin,Content-Length,Content-Type,Authorization,X-Request-ID,X-Forwarded-For,X-Real-IP,X-Idempotency-Key,X-E2E-Run-ID,X-Captcha-Token,Accept-Language"
		allowMethods  = "GET,POST,PUT,DELETE,OPTIONS"
		exposeHeaders = "X-Request-ID,X-RateLimit-Remaining,X-RateLimit-Reset,X-Captcha-Required"
	)
	maxAge := strconv.Itoa(int((12 * time.Hour).Seconds()))

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")
			if origin != "" {
				_, ok := allowed[origin]
				if allowAll || ok {
					h := w.Header()
					h.Set("Access-Control-Allow-Origin", origin)
					h.Add("Vary", "Origin")
					h.Set("Access-Control-Allow-Credentials", "true")
					h.Set("Access-Control-Allow-Headers", allowHeaders)
					h.Set("Access-Control-Allow-Methods", allowMethods)
					h.Set("Access-Control-Expose-Headers", exposeHeaders)
					h.Set("Access-Control-Max-Age", maxAge)
				}
			}
			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// ResolveTenantHTTP is the net/http analogue of ResolveTenant. It pins the single
// default tenant onto the request context BEFORE any tenant-owned DB access, so
// TenantDBConnMW pins the connection to the correct tenant (issue #223/#227).
// Multi-tenant resolution is the standalone/public-engine concern; the embedded
// surface is single-tenant by construction.
func ResolveTenantHTTP() HTTPMiddleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := tenant.WithID(r.Context(), tenant.DefaultID)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}
