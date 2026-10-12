package middleware

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	auth "github.com/open-rails/authkit/auth"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/billingauth"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/shared/iputil"
)

// The net/http base middleware: security headers, body limits, CORS, recovery
// and request logging, around both the standalone server and the embedded
// surface. Rate limits and captcha are in ratelimit_neutral.go.

// HTTPMiddleware is a standard net/http middleware (outermost wrapper).
type HTTPMiddleware func(http.Handler) http.Handler

// ChainHTTP composes mw around h so mw[0] is the outermost wrapper (runs first).
func ChainHTTP(h http.Handler, mw ...HTTPMiddleware) http.Handler {
	for i := len(mw) - 1; i >= 0; i-- {
		h = mw[i](h)
	}
	return h
}

// SecurityHeadersHTTP sets the API's security headers: no framing, no
// sniffing, a strict referrer policy and a locked-down CSP.
func SecurityHeadersHTTP() HTTPMiddleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h := w.Header()
			h.Set("X-Frame-Options", "DENY")
			h.Set("X-Content-Type-Options", "nosniff")
			h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
			h.Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; connect-src 'self'; font-src 'self'")
			h.Set("Server", "")
			next.ServeHTTP(w, r)
		})
	}
}

// MaxIdempotencyKeyBytes bounds a caller's Idempotency-Key.
const MaxIdempotencyKeyBytes = 255

// RequestLimitsHTTP refuses an oversized Idempotency-Key and validates the
// complete bounded body before any route can mutate state, including routes
// with optional or absent request bodies.
func RequestLimitsHTTP(maxBytes int64) HTTPMiddleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if len(r.Header.Get("Idempotency-Key")) > MaxIdempotencyKeyBytes {
				w.Header().Set("X-Request-ID", httprequest.EnsureRequestID(r))
				billingauth.WriteJSONError(w, http.StatusBadRequest, billing.CodeInvalidParam, "Idempotency-Key must be at most 255 bytes")
				return
			}
			if maxBytes > 0 && r.Body != nil {
				limit := requestBodyLimit(r, maxBytes)
				body := http.MaxBytesReader(w, r.Body, limit)
				if isMerchantArchiveImport(r) && limit != maxBytes {
					if r.ContentLength > limit {
						w.Header().Set("X-Request-ID", httprequest.EnsureRequestID(r))
						billingauth.WriteJSONError(w, http.StatusRequestEntityTooLarge, billing.CodeRequestBodyTooLarge, "billing archive exceeds its size limit")
						return
					}
					// Authenticate before reading a potentially large archive. Its
					// handler validates the full bounded stream before committing.
					r.Body = body
					next.ServeHTTP(w, r)
					return
				}
				raw, err := io.ReadAll(body)
				_ = body.Close()
				if err != nil {
					// Correlate the refusal like any handler error, in every
					// deployment (the in-process mount has no request logger).
					w.Header().Set("X-Request-ID", httprequest.EnsureRequestID(r))
					var tooLarge *http.MaxBytesError
					if errors.As(err, &tooLarge) {
						billingauth.WriteJSONError(w, http.StatusRequestEntityTooLarge, billing.CodeRequestBodyTooLarge, "request body too large")
					} else {
						billingauth.WriteJSONError(w, http.StatusBadRequest, billing.CodeInvalidRequestBody, "could not read request body")
					}
					return
				}
				r.Body = io.NopCloser(bytes.NewReader(raw))
			}
			next.ServeHTTP(w, r)
		})
	}
}

// BrowserTierRoutes is the set of route patterns in the permissive-CORS
// browser tier; checkout and self-service routes register as they mount.
// Matching is by path, whatever the method: a preflight is OPTIONS, which no
// route registers, so the serving mux cannot answer it.
type BrowserTierRoutes struct {
	mux  *http.ServeMux
	seen map[string]bool
}

// NewBrowserTierRoutes returns an empty registry (matches nothing until
// routes are Added).
func NewBrowserTierRoutes() *BrowserTierRoutes {
	return &BrowserTierRoutes{mux: http.NewServeMux(), seen: make(map[string]bool)}
}

// Add registers pattern, a bare path or "METHOD path" (the method is stripped:
// CORS eligibility never depends on it), as browser tier. The same path may be
// added once per method.
func (b *BrowserTierRoutes) Add(pattern string) {
	if b == nil {
		return
	}
	path := pattern
	if i := strings.IndexByte(pattern, ' '); i >= 0 {
		path = pattern[i+1:]
	}
	if path == "" || b.seen[path] {
		return
	}
	b.seen[path] = true
	b.mux.HandleFunc(path, func(http.ResponseWriter, *http.Request) {})
}

// Match reports whether r's path falls under a registered browser-tier
// pattern, independent of r.Method — an OPTIONS preflight against a
// registered GET-only route still matches.
func (b *BrowserTierRoutes) Match(r *http.Request) bool {
	if b == nil || r == nil {
		return false
	}
	_, pattern := b.mux.Handler(r)
	return pattern != ""
}

// AllRequests is a browser-tier matcher that matches everything, for a
// handler whose whole surface is browser tier (the embedded self-service one).
func AllRequests(*http.Request) bool { return true }

// PermissiveCORSHTTP is the static browser-tier CORS policy. Bearer tokens
// authorize these requests (cookie admission checks Origin itself), so an
// origin allow-list protects nothing: a stolen token replays from curl. A
// browser-tier request gets `Access-Control-Allow-Origin: *` and never
// Allow-Credentials; every other request gets no CORS headers. The header
// lists add the ones credentials travels in when it advertises them
// (authkit/auth Headers): the mount's Authenticator.
func PermissiveCORSHTTP(match func(*http.Request) bool, credentials any) HTTPMiddleware {
	allow := []string{"Origin", "Content-Length", "Content-Type", "Authorization", "X-Request-ID", "X-Forwarded-For", "X-Real-IP", "Idempotency-Key", "X-E2E-Run-ID", "X-Captcha-Token", "Accept-Language"}
	expose := []string{"WWW-Authenticate", "X-Request-ID", "X-RateLimit-Remaining", "X-RateLimit-Reset", "X-Captcha-Required"}
	if h, ok := credentials.(auth.Headers); ok {
		allow, expose = mergeHeaders(allow, h.AllowedHeaders()), mergeHeaders(expose, h.ExposedHeaders())
	}
	allowHeaders, exposeHeaders := strings.Join(allow, ","), strings.Join(expose, ",")
	const allowMethods = "GET,POST,PUT,PATCH,DELETE,OPTIONS"
	maxAge := strconv.Itoa(int((12 * time.Hour).Seconds()))

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if match != nil && match(r) {
				h := w.Header()
				h.Set("Access-Control-Allow-Origin", "*")
				h.Set("Access-Control-Allow-Headers", allowHeaders)
				h.Set("Access-Control-Allow-Methods", allowMethods)
				h.Set("Access-Control-Expose-Headers", exposeHeaders)
				h.Set("Access-Control-Max-Age", maxAge)
				if r.Method == http.MethodOptions {
					w.WriteHeader(http.StatusNoContent)
					return
				}
			}
			next.ServeHTTP(w, r)
		})
	}
}

// mergeHeaders is base with each of more it lacks, ignoring case.
func mergeHeaders(base, more []string) []string {
	out := append([]string(nil), base...)
	for _, name := range more {
		name = strings.TrimSpace(name)
		if name != "" && !slices.ContainsFunc(out, func(have string) bool { return strings.EqualFold(have, name) }) {
			out = append(out, name)
		}
	}
	return out
}

// RecoverHTTP converts a handler panic into a 500 envelope; http.ErrAbortHandler
// keeps propagating.
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

// RequestLogHTTP logs one line per request: method, path, status, latency and
// ip, the client resolver names as the rate limiter does (nil: the socket
// peer). skipPaths (health probes) are not logged.
func RequestLogHTTP(resolver *iputil.TrustedProxies, skipPaths ...string) HTTPMiddleware {
	skip := make(map[string]bool, len(skipPaths))
	for _, p := range skipPaths {
		skip[p] = true
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requestID := httprequest.EnsureRequestID(r)
			w.Header().Set("X-Request-ID", requestID)
			if skip[r.URL.Path] {
				next.ServeHTTP(w, r)
				return
			}
			start := time.Now()
			sw := &statusWriter{ResponseWriter: w}
			next.ServeHTTP(sw, r)
			log.WithFields(log.Fields{
				"status":     sw.status(),
				"latency":    time.Since(start).String(),
				"ip":         resolver.ClientIP(r),
				"request_id": requestID,
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

// Unwrap lets http.ResponseController reach the underlying connection, through
// which route budgets lift the write deadline.
func (s *statusWriter) Unwrap() http.ResponseWriter { return s.ResponseWriter }

func (s *statusWriter) status() int {
	if s.code == 0 {
		return http.StatusOK
	}
	return s.code
}
