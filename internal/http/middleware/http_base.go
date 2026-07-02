package middleware

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/pkg/merchant"
)

// This file holds the net/http base middleware (issue #282; sole stack since
// #670): security headers, body limits, CORS, merchant resolution, recovery,
// request logging. Both the standalone server and the embedded surface wrap
// their muxes with these. Rate-limiting + the captcha challenge flow live in
// ratelimit_neutral.go (EvaluateRateLimit / RateLimitHTTP).

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
			if origin != "" && OriginAllowed(origin, allowedOrigins) {
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

// CORSOriginSource returns the browser Origin allow-list for the current request.
type CORSOriginSource func(context.Context) ([]string, error)

// CORSFromSourceHTTP grants browser CORS headers only to origins allow-listed by
// a per-request source (e.g. AuthKit remote_application state). An empty or
// failing source grants no browser Origin. OPTIONS is still answered with 204 so
// denied preflight fails in the browser without invoking handlers.
func CORSFromSourceHTTP(source CORSOriginSource) HTTPMiddleware {
	staticCORS := CORSHTTP(nil)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origins := []string(nil)
			if source != nil && strings.TrimSpace(r.Header.Get("Origin")) != "" {
				if got, err := source(r.Context()); err == nil {
					origins = got
				}
			}
			if origins == nil {
				staticCORS(next).ServeHTTP(w, r)
				return
			}
			CORSHTTP(origins)(next).ServeHTTP(w, r)
		})
	}
}

// RecoverHTTP converts handler panics into a 500 response (the net/http
// analogue of gin.Recovery, kept for the standalone flip #670).
func RecoverHTTP() HTTPMiddleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if rec := recover(); rec != nil {
					if err, ok := rec.(error); ok && errors.Is(err, http.ErrAbortHandler) {
						panic(rec)
					}
					log.WithField("panic", rec).Error("http handler panicked")
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusInternalServerError)
					_, _ = w.Write([]byte(`{"error":{"type":"api_error","code":"internal_error","message":"internal server error"}}`))
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// RequestLogHTTP logs one line per request (method, path, status, latency) —
// the neutral analogue of the gin logger the standalone server used. skipPaths
// are not logged (health probes).
func RequestLogHTTP(skipPaths ...string) HTTPMiddleware {
	skip := make(map[string]bool, len(skipPaths))
	for _, p := range skipPaths {
		skip[p] = true
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if skip[r.URL.Path] {
				next.ServeHTTP(w, r)
				return
			}
			start := time.Now()
			sw := &statusWriter{ResponseWriter: w}
			next.ServeHTTP(sw, r)
			log.WithFields(log.Fields{
				"status":  sw.status(),
				"latency": time.Since(start).String(),
				"ip":      r.RemoteAddr,
			}).Info(r.Method + " " + r.URL.Path)
		})
	}
}

type statusWriter struct {
	http.ResponseWriter
	code int
}

func (s *statusWriter) WriteHeader(code int) {
	if s.code == 0 {
		s.code = code
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusWriter) Write(b []byte) (int, error) {
	if s.code == 0 {
		s.code = http.StatusOK
	}
	return s.ResponseWriter.Write(b)
}

func (s *statusWriter) status() int {
	if s.code == 0 {
		return http.StatusOK
	}
	return s.code
}

// OriginAllowed reports whether origin is explicitly allow-listed for browser
// CORS. Matching is exact after trimming whitespace; OpenRails does not grant
// wildcard browser credentials.
func OriginAllowed(origin string, allowedOrigins []string) bool {
	origin = strings.TrimSpace(origin)
	if origin == "" {
		return false
	}
	for _, allowed := range allowedOrigins {
		if strings.TrimSpace(allowed) == origin {
			return true
		}
	}
	return false
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
