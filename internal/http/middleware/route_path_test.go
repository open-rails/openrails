package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/captcha"
	"github.com/stretchr/testify/require"
)

func TestRoutePathPreservesRequestAndSelectsPolicy(t *testing.T) {
	limits := config.RateLimitsConfig{
		"checkout": {RequestsPerMinute: 1}, "payment": {RequestsPerMinute: 2},
		"webhook": {RequestsPerMinute: 3}, "default": {RequestsPerMinute: 60},
	}
	captchaCfg := &config.CaptchaConfig{SiteKey: "test-site", SecretKey: "test-secret"}
	for _, tc := range []struct {
		name, canonical, actual, bucket, limit string
		captcha                                bool
	}{
		{"checkout", "/billing/v1/customers/{customer_id}/checkout", "/api/pay/v1/customers/a%2Fb/checkout?x=1", "checkout", "checkout", true},
		{"payment methods", "/billing/v1/me/payment-methods", "/api/pay/v1/me/payment-methods", "payment-methods", "payment", true},
		{"webhook", "/billing/v1/webhooks/{provider}/{account_id}", "/api/pay/v1/webhooks/stripe/acct_test", "webhook", "webhook", false},
		{"captcha", "/billing/v1/captcha/status", "/api/pay/v1/captcha/status", "captcha", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, tc.actual, nil)
			originalURL, originalURI := *req.URL, req.RequestURI
			h := WithRoutePath(tc.canonical)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				require.Equal(t, originalURL, *r.URL, "auth must see the host URL, including RawPath")
				require.Equal(t, originalURI, r.RequestURI)
				limit, bucket := resolveRateLimitPolicy(&limits, r)
				require.Equal(t, tc.bucket, bucket)
				require.Same(t, limits[tc.limit], limit)
				require.Equal(t, tc.captcha, captchaShouldEnforce(captchaCfg, r, bucket))
			}))
			h.ServeHTTP(httptest.NewRecorder(), req)
			require.Equal(t, originalURL, *req.URL)
			require.Equal(t, originalURI, req.RequestURI)
		})
	}
}

func TestRoutePathCustomPrefixEnforcesCheckoutRateAndCaptcha(t *testing.T) {
	limits := config.RateLimitsConfig{"checkout": {RequestsPerMinute: 1}, "default": {RequestsPerMinute: 60}}
	captchaCfg := &config.CaptchaConfig{SiteKey: "test-site", SecretKey: "test-secret"}
	deps := RateLimitDeps{
		Limits: &limits, Captcha: captchaCfg, Store: NewRateLimitStore(),
		ChallengeStore: captcha.NewChallengeStore(nil), Verifier: &stubHTTPVerifier{},
	}
	h := WithRoutePath("/billing/v1/checkout")(rlHTTPHandlerWithDeps(deps, captchaCfg, okHTTPHandler()))
	for _, want := range []int{http.StatusOK, http.StatusTooManyRequests, http.StatusForbidden} {
		response := doHTTPPost(h, "/api/pay/v1/checkout", "203.0.113.88", "")
		require.Equal(t, want, response.Code, response.Body.String())
		if want == http.StatusForbidden {
			require.Equal(t, "true", response.Header().Get("X-Captcha-Required"))
		}
	}
}
