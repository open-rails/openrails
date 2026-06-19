package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/captcha"
	"github.com/open-rails/openrails/pkg/billingauth"
)

// These tests cover the gin-free net/http rate-limit + captcha enforcement
// (RateLimitHTTP / EvaluateRateLimit) that the EMBEDDED surface runs, mirroring the
// gin-surface coverage in ginmw/ratelimit_test.go.

type stubHTTPVerifier struct {
	validToken string
	calls      int
}

func (s *stubHTTPVerifier) Verify(_ context.Context, req captcha.VerifyRequest) (*captcha.VerifyResult, error) {
	s.calls++
	return &captcha.VerifyResult{Success: req.Token == s.validToken}, nil
}

func okHTTPHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
}

func doHTTPPost(h http.Handler, path, ip, token string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, nil)
	req.RemoteAddr = ip + ":1234"
	if token != "" {
		req.Header.Set(captcha.TokenHeader, token)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

// rlHTTPHandlerWithDeps mounts the engine with injectable deps (so the captcha
// stub verifier can be supplied), the same way RateLimitHTTP wires it internally.
func rlHTTPHandlerWithDeps(deps RateLimitDeps, captchaCfg *config.CaptchaConfig, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		subjects := rateLimitSubjectsHTTP(r)
		decision := EvaluateRateLimit(w, r, subjects, deps)
		applyRateLimitDecisionHTTP(w, r, next, decision, captchaCfg)
	})
}

func TestRateLimitHTTPBlocksOverLimit(t *testing.T) {
	t.Parallel()
	limits := config.RateLimitsConfig{"checkout": {RequestsPerMinute: 1}, "default": {RequestsPerMinute: 60}}
	h := RateLimitHTTP(&limits, nil, nil, captcha.NewChallengeStore(nil))(okHTTPHandler())

	require.Equal(t, http.StatusOK, doHTTPPost(h, "/v1/checkout", "203.0.113.70", "").Code)
	blocked := doHTTPPost(h, "/v1/checkout", "203.0.113.70", "")
	require.Equal(t, http.StatusTooManyRequests, blocked.Code)
	require.NotEmpty(t, blocked.Header().Get("Retry-After"))
	require.Equal(t, "1", blocked.Header().Get("X-RateLimit-Limit"))
}

func TestRateLimitHTTPRejectsOversizedContentLength(t *testing.T) {
	t.Parallel()
	limits := config.RateLimitsConfig{"checkout": {RequestsPerMinute: 10}}
	h := RateLimitHTTP(&limits, nil, nil, captcha.NewChallengeStore(nil))(okHTTPHandler())

	req := httptest.NewRequest(http.MethodPost, "/v1/checkout", nil)
	req.RemoteAddr = "203.0.113.71:1234"
	req.ContentLength = BucketMaxContentLength["checkout"] + 1
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	require.Equal(t, http.StatusRequestEntityTooLarge, w.Code)
}

func TestRateLimitHTTPNormalizesEmbeddedPrefix(t *testing.T) {
	t.Parallel()
	limits := config.RateLimitsConfig{"checkout": {RequestsPerMinute: 1}, "default": {RequestsPerMinute: 60}}
	h := RateLimitHTTP(&limits, nil, nil, captcha.NewChallengeStore(nil))(okHTTPHandler())

	// The embedded surface serves under /billing/v1/*; the bucket classifier must
	// strip the /billing prefix so the checkout limit still applies.
	require.Equal(t, http.StatusOK, doHTTPPost(h, "/billing/v1/checkout", "203.0.113.73", "").Code)
	require.Equal(t, http.StatusTooManyRequests, doHTTPPost(h, "/billing/v1/checkout", "203.0.113.73", "").Code)
}

func TestRateLimitHTTPEscalatesToCaptcha(t *testing.T) {
	t.Parallel()
	verifier := &stubHTTPVerifier{validToken: "valid-token"}
	limits := config.RateLimitsConfig{"checkout": {RequestsPerMinute: 1}, "default": {RequestsPerMinute: 60}}
	captchaCfg := &config.CaptchaConfig{
		Provider:  config.CaptchaProviderTurnstile,
		SiteKey:   "site-key",
		SecretKey: "secret-key",
	}
	deps := RateLimitDeps{
		Limits:         &limits,
		Captcha:        captchaCfg,
		Store:          NewRateLimitStore(),
		ChallengeStore: captcha.NewChallengeStore(nil),
		Verifier:       verifier,
	}
	h := rlHTTPHandlerWithDeps(deps, captchaCfg, okHTTPHandler())

	require.Equal(t, http.StatusOK, doHTTPPost(h, "/v1/checkout", "203.0.113.72", "").Code)
	require.Equal(t, http.StatusTooManyRequests, doHTTPPost(h, "/v1/checkout", "203.0.113.72", "").Code)

	challenged := doHTTPPost(h, "/v1/checkout", "203.0.113.72", "")
	require.Equal(t, http.StatusForbidden, challenged.Code)
	require.Contains(t, challenged.Body.String(), "captcha_required")
	require.Equal(t, "true", challenged.Header().Get("X-Captcha-Required"))

	invalid := doHTTPPost(h, "/v1/checkout", "203.0.113.72", "bad-token")
	require.Equal(t, http.StatusForbidden, invalid.Code)
	require.Contains(t, invalid.Body.String(), "captcha_invalid")

	solved := doHTTPPost(h, "/v1/checkout", "203.0.113.72", "valid-token")
	require.Equal(t, http.StatusOK, solved.Code)
	require.Equal(t, 2, verifier.calls)
}

func TestRateLimitHTTPLimitsByUserAcrossIPs(t *testing.T) {
	t.Parallel()
	const userID = "11111111-1111-1111-1111-111111111111"
	limits := config.RateLimitsConfig{"checkout": {RequestsPerMinute: 1}}
	auth := billingauth.AuthenticatorFunc(func(_ context.Context, _ *http.Request) (billingauth.UserContext, error) {
		return billingauth.UserContext{UserID: userID}, nil
	})
	// billingauth.Optional pins the user so the limiter keys per-user; a second
	// request from a DIFFERENT IP but the same user is still blocked.
	chain := ChainHTTP(okHTTPHandler(),
		HTTPMiddleware(billingauth.Optional(auth)),
		RateLimitHTTP(&limits, nil, nil, captcha.NewChallengeStore(nil)),
	)

	require.Equal(t, http.StatusOK, doHTTPPost(chain, "/v1/checkout", "203.0.113.80", "").Code)
	require.Equal(t, http.StatusTooManyRequests, doHTTPPost(chain, "/v1/checkout", "203.0.113.81", "").Code)
}
