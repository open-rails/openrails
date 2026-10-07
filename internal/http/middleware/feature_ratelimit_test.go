package middleware

import (
	"context"
	"errors"
	"net"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/billingauth"
	"github.com/open-rails/openrails/internal/config"
)

// Feature requests use the same HTTP limiter as checkout. Neither a missing
// Redis nor a Redis error makes repeated requests unlimited, and reaching one
// feature's limit does not consume another feature's allowance.
func TestFeatureRateLimitsWithOptionalRedis(t *testing.T) {
	for _, unavailable := range []bool{false, true} {
		name := "without redis"
		if unavailable {
			name = "redis unavailable"
		}
		t.Run(name, func(t *testing.T) {
			var rdb *redis.Client
			var dials atomic.Int64
			if unavailable {
				rdb = redis.NewClient(&redis.Options{
					MaxRetries: -1,
					Dialer: func(context.Context, string, string) (net.Conn, error) {
						dials.Add(1)
						return nil, errors.New("test redis unavailable")
					},
				})
				t.Cleanup(func() { require.NoError(t, rdb.Close()) })
			}
			limits := config.RateLimitsConfig{
				"metrics-ask":        {RequestsPerMinute: 2},
				"catalog-ask":        {RequestsPerMinute: 2},
				"dashboard-generate": {RequestsPerMinute: 2},
				"checkout":           {RequestsPerMinute: 2},
			}
			calls := 0
			h := RateLimitHTTP(&limits, captchaOn, rdb, nil, nil)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls++
				w.WriteHeader(http.StatusOK)
			}))
			for _, path := range []string{
				"/v1/merchant/metrics/ask",
				"/v1/merchant/catalog/ask",
				"/v1/merchant/dashboard/widgets/generate",
			} {
				before := calls
				for i, prefix := range []string{"", "/billing"} {
					rec := call{path: prefix + path, ip: "203.0.113.95", want: http.StatusOK}.do(t, h)
					require.Equal(t, "2", rec.Header().Get("X-RateLimit-Limit"))
					require.Equal(t, before+i+1, calls)
				}
				// Repeated excess merchant requests remain ordinary 429s even
				// when browser CAPTCHA escalation is configured.
				for range 6 {
					rec := call{path: path, ip: "203.0.113.95", want: http.StatusTooManyRequests, body: "rate_limit_exceeded"}.do(t, h)
					require.NotEmpty(t, rec.Header().Get("Retry-After"))
					require.Empty(t, rec.Header().Get("X-Captcha-Required"))
					require.Equal(t, "0", rec.Header().Get("X-RateLimit-Remaining"))
				}
				require.Equal(t, before+2, calls, "blocked requests never reach the feature")
			}
			// Feature limits neither consume the checkout budget nor become
			// a blanket limit on the merchant's non-model endpoints.
			call{path: "/v1/me/checkout-sessions", ip: "203.0.113.95", want: http.StatusOK}.do(t, h)
			call{path: "/v1/me/checkout-sessions", ip: "203.0.113.95", want: http.StatusOK}.do(t, h)
			for range 3 {
				call{path: "/v1/merchant/metrics/query", ip: "203.0.113.95", want: http.StatusOK}.do(t, h)
			}
			if unavailable {
				require.Positive(t, dials.Load(), "exercise Redis errors, not only the nil-Redis path")
			}
		})
	}
}

func TestFeatureRateLimitsUseNormalSubjects(t *testing.T) {
	limits := config.RateLimitsConfig{"metrics-ask": {RequestsPerMinute: 2}}
	auth := billingauth.AuthenticatorFunc(func(_ context.Context, r *http.Request) (billingauth.UserContext, error) {
		return billingauth.UserContext{UserID: r.Header.Get("X-Test-User")}, nil
	})
	h := ChainHTTP(okHandler(), HTTPMiddleware(billingauth.Optional(auth)), RateLimitHTTP(&limits, nil, nil, nil, nil))
	userA, userB, userC := uuid.NewString(), uuid.NewString(), uuid.NewString()
	merchantA, merchantB := billing.MerchantID(uuid.New()), billing.MerchantID(uuid.New())
	for _, c := range []call{
		{ip: "203.0.113.10", user: userA, merchant: merchantA, want: http.StatusOK},
		{ip: "203.0.113.11", user: userA, merchant: merchantB, want: http.StatusOK},
		{ip: "203.0.113.12", user: userA, merchant: merchantA, want: http.StatusTooManyRequests},
		// The blocked request used one count for this address; another user
		// gets its remaining count, but a third user cannot evade the IP cap.
		{ip: "203.0.113.12", user: userB, merchant: merchantA, want: http.StatusOK},
		{ip: "203.0.113.12", user: userC, merchant: merchantB, want: http.StatusTooManyRequests},
	} {
		c.path = "/v1/merchant/metrics/ask"
		c.do(t, h)
	}
}
