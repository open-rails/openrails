package middleware

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/open-rails/authkit"
	"github.com/open-rails/openrails/pkg/merchant"
)

// This file holds the gin-free net/http analogues of the global engine
// middleware (issue #282). The embedded surface (server.NewHTTPHandler) wraps its
// ServeMux with these so it can run security headers, body limits, CORS, and
// merchant resolution WITHOUT importing gin. The standalone gin server keeps using
// the gin versions in security.go / merchant.go.
//
// Rate-limiting + the captcha challenge flow ARE available gin-free: the neutral
// engine lives in ratelimit_neutral.go (EvaluateRateLimit / RateLimitHTTP), driven
// by both the gin surface (via ginmw) and the embedded surface, so an embedded
// host enforces OpenRails' own rate limits + captcha without its own gateway.

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
			h.Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; connect-src 'self'; font-src 'self'")
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
// Empty allowedOrigins means no browser origin is granted CORS. OPTIONS
// preflight is answered with 204 either way; denied origins simply receive no
// CORS grant headers.
func CORSHTTP(allowedOrigins []string) HTTPMiddleware {
	const (
		allowHeaders  = "Origin,Content-Length,Content-Type,Authorization,X-Request-ID,X-Forwarded-For,X-Real-IP,X-Idempotency-Key,X-E2E-Run-ID,X-Captcha-Token,Accept-Language"
		allowMethods  = "GET,POST,PUT,DELETE,OPTIONS"
		exposeHeaders = "X-Request-ID,X-RateLimit-Remaining,X-RateLimit-Reset,X-Captcha-Required"
	)
	maxAge := strconv.Itoa(int((12 * time.Hour).Seconds()))

	setCommon := func(h http.Header) {
		h.Set("Access-Control-Allow-Headers", allowHeaders)
		h.Set("Access-Control-Allow-Methods", allowMethods)
		h.Set("Access-Control-Expose-Headers", exposeHeaders)
		h.Set("Access-Control-Max-Age", maxAge)
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := strings.TrimSpace(r.Header.Get("Origin"))
			if origin != "" && authkit.OriginAllowed(origin, allowedOrigins) {
				h := w.Header()
				h.Set("Access-Control-Allow-Origin", origin)
				h.Add("Vary", "Origin")
				h.Set("Access-Control-Allow-Credentials", "true")
				setCommon(h)
			}
			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// ResolveMerchantHTTP is the net/http analogue of ResolveMerchant. It pins the
// engine's CONSTRUCTION-TIME merchant (`configured`) onto the request context
// BEFORE any merchant-owned DB access, so MerchantDBConnMW pins the connection to
// the correct merchant (issue #223/#227). An OpenRails engine is bound to a single
// merchant at construction — there is NO default merchant (#336).
//
// If `configured` is zero, NOTHING is pinned: downstream merchant.Require fails,
// so a missing merchant is a hard error rather than a silent default.
func ResolveMerchantHTTP(configured merchant.ID) HTTPMiddleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if configured.IsZero() {
				next.ServeHTTP(w, r)
				return
			}
			ctx := merchant.WithID(r.Context(), configured)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}
