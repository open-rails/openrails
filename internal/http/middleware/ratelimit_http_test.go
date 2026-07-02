package middleware

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/captcha"
	"github.com/open-rails/openrails/pkg/billingauth"
)

// These tests cover the net/http rate-limit + captcha enforcement
// (RateLimitHTTP / EvaluateRateLimit) — since #670 the ONLY transport, serving
// both the standalone and embedded surfaces.

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

// --- ported from the retired gin adapter tests (#670): same engine, neutral
// transport. These cover behaviors the earlier neutral tests did not pin.

func TestClassifyBucketMatchesRegisteredRoutes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		path   string
		method string
		want   string
	}{
		{name: "webhook", path: "/v1/webhooks/stripe", method: http.MethodPost, want: "webhook"},
		{name: "embedded webhook", path: "/billing/v1/webhooks/mobius", method: http.MethodPost, want: "webhook"},
		{name: "merchant webhook", path: "/v1/merchants/acme/webhooks/stripe", method: http.MethodPost, want: "webhook"},
		{name: "embedded merchant webhook", path: "/billing/v1/merchants/acme/webhooks/stripe", method: http.MethodPost, want: "webhook"},
		{name: "captcha status", path: "/v1/captcha/status", method: http.MethodGet, want: "captcha"},
		{name: "embedded captcha client", path: "/billing/v1/captcha/client.js", method: http.MethodGet, want: "captcha"},
		{name: "checkout create", path: "/v1/checkout", method: http.MethodPost, want: "checkout"},
		{name: "checkout confirm", path: "/v1/checkout/checkout_123/confirm", method: http.MethodPost, want: "checkout"},
		{name: "payment method create", path: "/v1/me/payment-methods", method: http.MethodPost, want: "payment-methods"},
		{name: "payment method update", path: "/billing/v1/me/payment-methods/pm_123", method: http.MethodPut, want: "payment-methods"},
		{name: "subscription cancel", path: "/v1/me/subscriptions/sub_123/cancel", method: http.MethodPost, want: "subscriptions"},
		{name: "subscription payment method update", path: "/billing/v1/me/subscriptions/sub_123/payment-method", method: http.MethodPut, want: "subscriptions"},
		{name: "subscription read default", path: "/v1/me/subscriptions/sub_123", method: http.MethodGet, want: "default"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tt.want, ClassifyBucket(tt.path, tt.method))
		})
	}
}

func TestRateLimitHTTPDoesNotCaptchaWebhookBucket(t *testing.T) {
	t.Parallel()
	limits := config.RateLimitsConfig{"webhook": {RequestsPerMinute: 1}, "default": {RequestsPerMinute: 60}}
	challengeStore := captcha.NewChallengeStore(nil)
	require.NoError(t, challengeStore.MarkChallenged(context.Background(), "ip:203.0.113.20", time.Minute))
	captchaCfg := &config.CaptchaConfig{
		Provider:  config.CaptchaProviderTurnstile,
		SiteKey:   "site-key",
		SecretKey: "secret-key",
	}
	deps := RateLimitDeps{
		Limits:         &limits,
		Captcha:        captchaCfg,
		Store:          NewRateLimitStore(),
		ChallengeStore: challengeStore,
		Verifier:       &stubHTTPVerifier{validToken: "valid-token"},
	}
	h := rlHTTPHandlerWithDeps(deps, captchaCfg, okHTTPHandler())

	require.Equal(t, http.StatusOK, doHTTPPost(h, "/v1/webhooks/stripe", "203.0.113.20", "").Code)
	for i := 0; i < 3; i++ {
		w := doHTTPPost(h, "/v1/webhooks/stripe", "203.0.113.20", "")
		require.Equal(t, http.StatusTooManyRequests, w.Code)
		require.NotContains(t, w.Body.String(), "captcha_required")
	}
}

func TestRateLimitHTTPCaptchaChallengeIsGlobalAcrossProtectedBuckets(t *testing.T) {
	t.Parallel()
	verifier := &stubHTTPVerifier{validToken: "valid-token"}
	limits := config.RateLimitsConfig{
		"checkout": {RequestsPerMinute: 1},
		"payment":  {RequestsPerMinute: 1},
		"default":  {RequestsPerMinute: 60},
	}
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

	const ip = "203.0.113.10"
	require.Equal(t, http.StatusOK, doHTTPPost(h, "/v1/checkout", ip, "").Code)
	require.Equal(t, http.StatusTooManyRequests, doHTTPPost(h, "/v1/checkout", ip, "").Code)
	require.Equal(t, http.StatusForbidden, doHTTPPost(h, "/v1/checkout", ip, "").Code)

	challengedPayment := doHTTPPost(h, "/v1/me/payment-methods", ip, "")
	require.Equal(t, http.StatusForbidden, challengedPayment.Code)
	require.Contains(t, challengedPayment.Body.String(), "captcha_required")

	solvedPayment := doHTTPPost(h, "/v1/me/payment-methods", ip, "valid-token")
	require.Equal(t, http.StatusOK, solvedPayment.Code)

	require.Equal(t, http.StatusOK, doHTTPPost(h, "/v1/checkout", ip, "").Code)
}

func TestRateLimitHTTPDoesNotLimitCaptchaStatusOrClientScript(t *testing.T) {
	t.Parallel()
	limits := config.RateLimitsConfig{"default": {RequestsPerMinute: 1}}
	captchaCfg := &config.CaptchaConfig{Provider: config.CaptchaProviderTurnstile, SiteKey: "site-key", SecretKey: "secret-key"}
	h := RateLimitHTTP(&limits, captchaCfg, nil, captcha.NewChallengeStore(nil))(okHTTPHandler())

	for _, path := range []string{"/v1/captcha/status", "/v1/captcha/client.js"} {
		for i := 0; i < 3; i++ {
			req := httptest.NewRequest(http.MethodGet, path, nil)
			req.RemoteAddr = "203.0.113.10:1234"
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)
			require.Equal(t, http.StatusOK, w.Code, path)
		}
	}
}

func TestRateLimitHTTPCaptchaSolveClearsIPAndUserSubjects(t *testing.T) {
	t.Parallel()
	verifier := &stubHTTPVerifier{validToken: "valid-token"}
	limits := config.RateLimitsConfig{"checkout": {RequestsPerMinute: 10}}
	captchaCfg := &config.CaptchaConfig{
		Provider:  config.CaptchaProviderTurnstile,
		SiteKey:   "site-key",
		SecretKey: "secret-key",
	}
	challengeStore := captcha.NewChallengeStore(nil)
	require.NoError(t, challengeStore.MarkChallenged(context.Background(), "ip:203.0.113.47", time.Minute))
	require.NoError(t, challengeStore.MarkChallenged(context.Background(), "user:user_1", time.Minute))
	store := NewRateLimitStore()
	store.SeedCounter("checkout", "ip:203.0.113.47", 9, time.Now().Add(time.Minute))
	store.SeedCounter("checkout", "user:user_1", 9, time.Now().Add(time.Minute))

	deps := RateLimitDeps{
		Limits:         &limits,
		Captcha:        captchaCfg,
		Store:          store,
		ChallengeStore: challengeStore,
		Verifier:       verifier,
	}
	h := rlHTTPHandlerWithDeps(deps, captchaCfg, okHTTPHandler())

	req := httptest.NewRequest(http.MethodPost, "/v1/checkout", nil)
	req.RemoteAddr = "203.0.113.47:1234"
	req.Header.Set(captcha.TokenHeader, "valid-token")
	req = req.WithContext(billingauth.SetUserContext(req.Context(), billingauth.UserContext{UserID: "user_1"}))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, 1, verifier.calls)

	ipChallenged, err := challengeStore.IsChallenged(context.Background(), "ip:203.0.113.47")
	require.NoError(t, err)
	require.False(t, ipChallenged)
	userChallenged, err := challengeStore.IsChallenged(context.Background(), "user:user_1")
	require.NoError(t, err)
	require.False(t, userChallenged)
	snapshot := store.Snapshot()
	require.Equal(t, 1, snapshot["checkout:ip:203.0.113.47"])
	require.Equal(t, 1, snapshot["checkout:user:user_1"])
}

func TestRateLimitHTTPRejectsOversizedChunkedBody(t *testing.T) {
	t.Parallel()
	limits := config.RateLimitsConfig{"checkout": {RequestsPerMinute: 10}}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, err := io.ReadAll(r.Body)
		require.Error(t, err)
		w.WriteHeader(http.StatusRequestEntityTooLarge)
	})
	h := RateLimitHTTP(&limits, nil, nil, captcha.NewChallengeStore(nil))(next)

	req := httptest.NewRequest(http.MethodPost, "/v1/checkout", strings.NewReader(strings.Repeat("a", int(BucketMaxContentLength["checkout"]+1))))
	req.RemoteAddr = "203.0.113.61:1234"
	req.ContentLength = -1
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	// Chunked requests are capped by MaxBytesReader even without Content-Length.
	require.Equal(t, http.StatusRequestEntityTooLarge, w.Code)
}
