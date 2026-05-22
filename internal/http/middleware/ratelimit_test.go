package middleware

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/captcha"
	"github.com/stretchr/testify/require"
)

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
			require.Equal(t, tt.want, classifyBucket(tt.path, tt.method))
		})
	}
}

func TestRateLimitEscalatesToCaptchaAfterExtremeHits(t *testing.T) {
	gin.SetMode(gin.TestMode)
	verifier := &stubCaptchaVerifier{validToken: "valid-token"}
	cfg := config.RateLimitsConfig{"checkout": {RequestsPerMinute: 1}, "default": {RequestsPerMinute: 60}}
	captchaCfg := &config.CaptchaConfig{
		Enabled:           true,
		Provider:          config.CaptchaProviderTurnstile,
		SiteKey:           "site-key",
		SecretKey:         "secret-key",
		ExtremeMultiplier: 3,
		ChallengeTTL:      "15m",
		ChallengeBuckets:  []string{"checkout"},
	}

	r := gin.New()
	r.Use(rateLimitWithDependencies(&cfg, captchaCfg, nil, NewRateLimitStore(), captcha.NewChallengeStore(nil), verifier))
	r.POST("/v1/checkout", func(c *gin.Context) { c.String(http.StatusOK, "ok") })

	require.Equal(t, http.StatusOK, performCaptchaTestRequest(r, "").Code)
	require.Equal(t, http.StatusTooManyRequests, performCaptchaTestRequest(r, "").Code)

	challenged := performCaptchaTestRequest(r, "")
	require.Equal(t, http.StatusForbidden, challenged.Code)
	require.Contains(t, challenged.Body.String(), "captcha_required")
	require.Equal(t, "true", challenged.Header().Get("X-Captcha-Required"))

	invalid := performCaptchaTestRequest(r, "bad-token")
	require.Equal(t, http.StatusForbidden, invalid.Code)
	require.Contains(t, invalid.Body.String(), "captcha_invalid")

	solved := performCaptchaTestRequest(r, "valid-token")
	require.Equal(t, http.StatusOK, solved.Code)
	require.Equal(t, 2, verifier.calls)
}

func TestRateLimitDoesNotCaptchaWebhookBucket(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg := config.RateLimitsConfig{"webhook": {RequestsPerMinute: 1}, "default": {RequestsPerMinute: 60}}
	challengeStore := captcha.NewChallengeStore(nil)
	require.NoError(t, challengeStore.MarkChallenged(context.Background(), "203.0.113.20", time.Minute))
	captchaCfg := &config.CaptchaConfig{
		Enabled:           true,
		Provider:          config.CaptchaProviderTurnstile,
		SiteKey:           "site-key",
		SecretKey:         "secret-key",
		ExtremeMultiplier: 2,
		ChallengeBuckets:  []string{"webhook"},
	}

	r := gin.New()
	r.Use(rateLimitWithDependencies(&cfg, captchaCfg, nil, NewRateLimitStore(), challengeStore, &stubCaptchaVerifier{validToken: "valid-token"}))
	r.POST("/v1/webhooks/stripe", func(c *gin.Context) { c.String(http.StatusOK, "ok") })

	require.Equal(t, http.StatusOK, performCaptchaWebhookTestRequest(r).Code)
	for i := 0; i < 3; i++ {
		w := performCaptchaWebhookTestRequest(r)
		require.Equal(t, http.StatusTooManyRequests, w.Code)
		require.NotContains(t, w.Body.String(), "captcha_required")
	}
}

func TestRateLimitCaptchaChallengeIsGlobalAcrossProtectedBuckets(t *testing.T) {
	gin.SetMode(gin.TestMode)
	verifier := &stubCaptchaVerifier{validToken: "valid-token"}
	cfg := config.RateLimitsConfig{
		"checkout": {RequestsPerMinute: 1},
		"payment":  {RequestsPerMinute: 1},
		"default":  {RequestsPerMinute: 60},
	}
	captchaCfg := &config.CaptchaConfig{
		Enabled:           true,
		Provider:          config.CaptchaProviderTurnstile,
		SiteKey:           "site-key",
		SecretKey:         "secret-key",
		ExtremeMultiplier: 3,
		ChallengeTTL:      "15m",
		ChallengeBuckets:  []string{"checkout", "payment-methods"},
	}

	r := gin.New()
	r.Use(rateLimitWithDependencies(&cfg, captchaCfg, nil, NewRateLimitStore(), captcha.NewChallengeStore(nil), verifier))
	r.POST("/v1/checkout", func(c *gin.Context) { c.String(http.StatusOK, "checkout") })
	r.POST("/v1/me/payment-methods", func(c *gin.Context) { c.String(http.StatusOK, "payment") })

	require.Equal(t, http.StatusOK, performCaptchaTestRequest(r, "", "/v1/checkout").Code)
	require.Equal(t, http.StatusTooManyRequests, performCaptchaTestRequest(r, "", "/v1/checkout").Code)
	require.Equal(t, http.StatusForbidden, performCaptchaTestRequest(r, "", "/v1/checkout").Code)

	challengedPayment := performCaptchaTestRequest(r, "", "/v1/me/payment-methods")
	require.Equal(t, http.StatusForbidden, challengedPayment.Code)
	require.Contains(t, challengedPayment.Body.String(), "captcha_required")

	solvedPayment := performCaptchaTestRequest(r, "valid-token", "/v1/me/payment-methods")
	require.Equal(t, http.StatusOK, solvedPayment.Code)

	require.Equal(t, http.StatusOK, performCaptchaTestRequest(r, "", "/v1/checkout").Code)
}

func TestRateLimitCaptchaChallengeAppliesToDefaultAPIBucket(t *testing.T) {
	gin.SetMode(gin.TestMode)
	challengeStore := captcha.NewChallengeStore(nil)
	require.NoError(t, challengeStore.MarkChallenged(context.Background(), "203.0.113.10", time.Minute))
	cfg := config.RateLimitsConfig{"default": {RequestsPerMinute: 60}}
	captchaCfg := &config.CaptchaConfig{
		Enabled:          true,
		Provider:         config.CaptchaProviderTurnstile,
		SiteKey:          "site-key",
		SecretKey:        "secret-key",
		ChallengeBuckets: []string{"checkout"},
	}

	r := gin.New()
	r.Use(rateLimitWithDependencies(&cfg, captchaCfg, nil, NewRateLimitStore(), challengeStore, &stubCaptchaVerifier{validToken: "valid-token"}))
	r.POST("/v1/stripe/portal", func(c *gin.Context) { c.String(http.StatusOK, "portal") })

	challenged := performCaptchaTestRequest(r, "", "/v1/stripe/portal")
	require.Equal(t, http.StatusForbidden, challenged.Code)
	require.Contains(t, challenged.Body.String(), "captcha_required")
}

func TestRateLimitDoesNotLimitCaptchaStatusOrClientScript(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg := config.RateLimitsConfig{"default": {RequestsPerMinute: 1}}
	captchaCfg := &config.CaptchaConfig{Enabled: true, Provider: config.CaptchaProviderTurnstile, SiteKey: "site-key", SecretKey: "secret-key"}

	r := gin.New()
	r.Use(rateLimitWithDependencies(&cfg, captchaCfg, nil, NewRateLimitStore(), captcha.NewChallengeStore(nil), &stubCaptchaVerifier{validToken: "valid-token"}))
	r.GET("/v1/captcha/status", func(c *gin.Context) { c.String(http.StatusOK, "status") })
	r.GET("/v1/captcha/client.js", func(c *gin.Context) { c.String(http.StatusOK, "client") })

	for i := 0; i < 3; i++ {
		req := httptest.NewRequest(http.MethodGet, "/v1/captcha/status", nil)
		req.RemoteAddr = "203.0.113.10:1234"
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		require.Equal(t, http.StatusOK, w.Code)
	}
	for i := 0; i < 3; i++ {
		req := httptest.NewRequest(http.MethodGet, "/v1/captcha/client.js", nil)
		req.RemoteAddr = "203.0.113.10:1234"
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		require.Equal(t, http.StatusOK, w.Code)
	}
}

func TestRateLimitStorePrunesExpiredCounters(t *testing.T) {
	t.Parallel()

	store := NewRateLimitStore()
	store.counters["expired"] = &inMemoryCounter{count: 1, reset: time.Now().Add(-time.Minute)}

	result := store.Allow("203.0.113.30", "checkout", &config.RateLimit{RequestsPerMinute: 10})
	require.True(t, result.allowed)
	require.NotContains(t, store.counters, "expired")
}

func TestRateLimitStoreBoundsCounters(t *testing.T) {
	t.Parallel()

	store := NewRateLimitStore()
	for i := 0; i < maxInMemoryRateLimitCounters; i++ {
		store.counters[fmt.Sprintf("checkout:203.0.113.%d", i)] = &inMemoryCounter{count: 1, reset: time.Now().Add(time.Hour)}
	}

	result := store.Allow("203.0.113.31", "checkout", &config.RateLimit{RequestsPerMinute: 10})
	require.True(t, result.allowed)
	require.LessOrEqual(t, len(store.counters), maxInMemoryRateLimitCounters)
}

func TestRateLimitStoreDoesNotEvictWhenReusingExistingCounter(t *testing.T) {
	t.Parallel()

	store := NewRateLimitStore()
	ip := "203.0.113.32"
	key := "checkout:" + ip
	store.counters[key] = &inMemoryCounter{count: 1, reset: time.Now().Add(time.Hour)}
	for i := 1; len(store.counters) < maxInMemoryRateLimitCounters; i++ {
		store.counters[fmt.Sprintf("checkout:198.51.100.%d", i)] = &inMemoryCounter{count: 1, reset: time.Now().Add(time.Hour)}
	}

	result := store.Allow(ip, "checkout", &config.RateLimit{RequestsPerMinute: 10})
	require.True(t, result.allowed)
	require.Len(t, store.counters, maxInMemoryRateLimitCounters)
	require.Contains(t, store.counters, key)
	require.Equal(t, 2, store.counters[key].count)
}

type stubCaptchaVerifier struct {
	validToken string
	calls      int
}

func (s *stubCaptchaVerifier) Verify(ctx context.Context, req captcha.VerifyRequest) (*captcha.VerifyResult, error) {
	s.calls++
	return &captcha.VerifyResult{Success: req.Token == s.validToken}, nil
}

func performCaptchaTestRequest(r http.Handler, token string, paths ...string) *httptest.ResponseRecorder {
	path := "/v1/checkout"
	if len(paths) > 0 {
		path = paths[0]
	}
	req := httptest.NewRequest(http.MethodPost, path, nil)
	req.RemoteAddr = "203.0.113.10:1234"
	if token != "" {
		req.Header.Set(captcha.TokenHeader, token)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func performCaptchaWebhookTestRequest(r http.Handler) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/v1/webhooks/stripe", nil)
	req.RemoteAddr = "203.0.113.20:1234"
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}
