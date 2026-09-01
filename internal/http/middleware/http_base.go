package middleware

import (
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

// BodyLimitHTTP applies the global body-size cap uniformly, including to
// webhook routes (the per-rail caps in internal/http/handlers/webhook.go bind
// tighter than this backstop). MaxBytesReader only limits the body; signature
// verification still reads the raw bytes up to the cap, so legitimate webhook
// payloads are unaffected.
func BodyLimitHTTP(maxBytes int64) HTTPMiddleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if maxBytes > 0 && r.Body != nil {
				r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
			}
			next.ServeHTTP(w, r)
		})
	}
}

// BrowserTierRoutes tracks which route patterns belong to the permissive-CORS
// browser tier (#765): checkout, self-service, and customer-treasury routes
// register their pattern here as they mount; PermissiveCORSHTTP consults it to
// decide whether an inbound request qualifies for the static `*` policy.
//
// Matching is by PATH, independent of method: an OPTIONS preflight is never
// itself a registered route method (routes register GET/POST/etc, never
// OPTIONS), so membership can't be tested by asking the real serving mux "do
// you have a handler for OPTIONS <path>" — it never does. Registering each
// browser-tier pattern here a second time, method-agnostic, lets Match answer
// "is this path browser tier" for ANY method, preflight included, using the
// exact same pattern syntax (incl. wildcards) the real mux was given.
type BrowserTierRoutes struct {
	mux  *http.ServeMux
	seen map[string]bool
}

// NewBrowserTierRoutes returns an empty registry (matches nothing until
// routes are Added).
func NewBrowserTierRoutes() *BrowserTierRoutes {
	return &BrowserTierRoutes{mux: http.NewServeMux(), seen: make(map[string]bool)}
}

// Add registers pattern as browser tier. pattern may be a bare ServeMux path
// ("/v1/me/{id}") or a "METHOD path" pattern (the method prefix, if present,
// is stripped — CORS eligibility never depends on method). Idempotent: the
// same path may be added once per method without panicking on the underlying
// mux's duplicate-registration check.
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

// AllRequests is a browser-tier matcher that matches unconditionally — for
// handlers whose ENTIRE mounted surface is already browser tier by
// construction (e.g. the embedded self-service handler, which serves only
// /me and /customers), so no per-pattern registry is needed.
func AllRequests(*http.Request) bool { return true }

// PermissiveCORSHTTP is the #765 static browser-tier CORS policy: bearer JWTs,
// never ambient cookies, authorize every request this engine accepts, so an
// origin allow-list protects nothing here — a stolen token is replayed from
// curl, where CORS doesn't exist. Every browser-tier request (per match, e.g.
// BrowserTierRoutes.Match) gets `Access-Control-Allow-Origin: *`; every other
// request gets NO CORS headers at all, so a browser refuses cross-origin
// script access to it by default (the free, correct posture for surfaces only
// bearer-JWT curl/service callers use, never a browser page's fetch/XHR).
// Access-Control-Allow-Credentials is NEVER set — OpenRails never uses
// cookies, and a wildcard origin with credentials is invalid CORS besides.
func PermissiveCORSHTTP(match func(*http.Request) bool) HTTPMiddleware {
	const (
		allowHeaders  = "Origin,Content-Length,Content-Type,Authorization,X-Request-ID,X-Forwarded-For,X-Real-IP,Idempotency-Key,X-E2E-Run-ID,X-Captcha-Token,Accept-Language"
		allowMethods  = "GET,POST,PUT,DELETE,OPTIONS"
		exposeHeaders = "X-Request-ID,X-RateLimit-Remaining,X-RateLimit-Reset,X-Captcha-Required"
	)
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

// Unwrap lets http.ResponseController reach the underlying connection
// (route budgets lift the write deadline through it, xs-007 row 37).
func (s *statusWriter) Unwrap() http.ResponseWriter { return s.ResponseWriter }

func (s *statusWriter) status() int {
	if s.code == 0 {
		return http.StatusOK
	}
	return s.code
}

// ResolveMerchantHTTP is the net/http analogue of ResolveMerchant. It pins the
// engine's configured merchant onto the request context BEFORE any
// merchant-owned DB access, so MerchantDBConnMW pins the connection to the
// correct merchant (issue #223/#227). An OpenRails engine is bound to a
// single merchant — there is NO default merchant (#336).
//
// resolve is called ON EVERY REQUEST, never once at construction time
// (#744): an embedded engine's bound merchant is set post-boot
// (UpsertMerchantConfig binds after New — embed/provision.go), and an HTTP
// handler built from the same Runtime may already be mounted and serving
// requests before that bind happens. Pass a live accessor — typically the
// Runtime's own method value, e.g. `middleware.ResolveMerchantHTTP(rt.ConfiguredMerchant)`
// — never a value snapshotted at handler-construction time, or mounting
// before the bind pins every request to the zero merchant forever.
//
// If resolve is nil or returns zero, NOTHING is pinned: downstream
// merchant.Require fails, so a missing merchant is a hard error rather than a
// silent default.
func ResolveMerchantHTTP(resolve func() merchant.ID) HTTPMiddleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var configured merchant.ID
			if resolve != nil {
				configured = resolve()
			}
			if configured.IsZero() {
				next.ServeHTTP(w, r)
				return
			}
			ctx := merchant.WithID(r.Context(), configured)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// StaticMerchant adapts a fixed merchant.ID into the resolver ResolveMerchantHTTP
// expects. Production code should prefer a live accessor (Runtime.ConfiguredMerchant);
// this exists for callers with a genuinely fixed id — chiefly tests that pin one
// merchant for the lifetime of a test server.
func StaticMerchant(id merchant.ID) func() merchant.ID {
	return func() merchant.ID { return id }
}

// ResolveMerchantFromHostHTTP resolves the merchant owning the request's Host
// header (#734) and, when resolved, pins it BOTH as the ordinary "configured
// merchant" (merchant.WithID — the same key ResolveMerchantHTTP sets, so
// unauthenticated merchant-scoped routes such as public catalog reads work
// per-Host in a multi-merchant deployment) AND as merchant.WithHostMerchant, a
// marker the control plane's JWT-issuer resolution (internal/controlplane)
// reads to enforce Host-merchant == issuer-merchant, fail closed on mismatch.
//
// resolve is called ON EVERY REQUEST (live per call, #734: no boot-time host
// map — a merchant registered after this process started resolves on its very
// next request, on every process sharing the database). resolve == nil, or an
// unresolvable Host (unknown/disabled/ambiguous merchant), is a NO-OP: this
// middleware only ever narrows context, never blocks a request outright, so
// health/platform routes and Hosts with no configured merchant keep working
// exactly as before. A deployment that configures no Host resolver never calls
// this middleware at all — single-merchant self-hosters see no behavior change
// without opting in.
func ResolveMerchantFromHostHTTP(resolve merchant.HostResolver) HTTPMiddleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if resolve == nil {
				next.ServeHTTP(w, r)
				return
			}
			mid, err := resolve(r.Context(), r.Host)
			if err != nil || mid.IsZero() {
				next.ServeHTTP(w, r)
				return
			}
			ctx := merchant.WithID(r.Context(), mid)
			ctx = merchant.WithHostMerchant(ctx, mid)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}
