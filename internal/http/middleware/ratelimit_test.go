package middleware

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/captcha"
	"github.com/open-rails/openrails/internal/shared/iputil"
	"github.com/open-rails/openrails/pkg/billingauth"
)

type stubVerifier struct {
	valid string
	err   error
	calls int
}

func (s *stubVerifier) Verify(_ context.Context, req captcha.VerifyRequest) (*captcha.VerifyResult, error) {
	s.calls++
	if s.err != nil {
		return nil, s.err
	}
	return &captcha.VerifyResult{Success: req.Token == s.valid}, nil
}

var captchaOn = &config.CaptchaConfig{SiteKey: "site-key", SecretKey: "secret-key"}

func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
}

type call struct {
	method, path, ip, xff, user, token string
	want                               int
	body                               string
}

func (c call) do(t *testing.T, h http.Handler) *httptest.ResponseRecorder {
	t.Helper()
	method := c.method
	if method == "" {
		method = http.MethodPost
	}
	req := httptest.NewRequest(method, c.path, nil)
	req.RemoteAddr = c.ip + ":1234"
	if c.xff != "" {
		req.Header.Set("X-Forwarded-For", c.xff)
	}
	if c.user != "" {
		req.Header.Set("X-Test-User", c.user)
	}
	if c.token != "" {
		req.Header.Set(captcha.TokenHeader, c.token)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	require.Equal(t, c.want, w.Code, "%s %s: %s", method, c.path, w.Body.String())
	if c.body != "" {
		require.Contains(t, w.Body.String(), c.body)
	}
	return w
}

// engine mounts the rate-limit engine as RateLimitHTTP does, with injectable
// deps so tests can supply a captcha verifier and inspect the stores.
func engine(deps RateLimitDeps, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u := r.Header.Get("X-Test-User"); u != "" {
			r = r.WithContext(billingauth.SetUserContext(r.Context(), billingauth.UserContext{UserID: u}))
		}
		applyRateLimitDecisionHTTP(w, r, next, EvaluateRateLimit(w, r, rateLimitSubjectsHTTP(r, nil), deps), deps.Captcha)
	})
}

func newDeps(limits config.RateLimitsConfig, cfg *config.CaptchaConfig, v captcha.Verifier) RateLimitDeps {
	return RateLimitDeps{Limits: &limits, Captcha: cfg, Store: NewRateLimitStore(), ChallengeStore: captcha.NewChallengeStore(nil), Verifier: v}
}

func TestClassifyBucket(t *testing.T) {
	for _, tc := range []struct{ method, path, want string }{
		{"POST", "/v1/webhooks/stripe", "webhook"},
		{"POST", "/billing/v1/webhooks/stripe/acct_test", "webhook"},
		{"GET", "/billing/v1/captcha/client.js", "captcha"},
		{"POST", "/v1/checkout", "checkout"},
		{"POST", "/v1/checkout/checkout_123/confirm", "checkout"},
		{"POST", "/v1/customers/customer_123/checkout", "checkout"},
		{"POST", "/billing/v1/customers/customer_123/checkout/checkout_123/confirm", "checkout"},
		{"GET", "/v1/me/checkout/checkout_123", "default"},
		{"POST", "/v1/checkout-config", "default"},
		{"POST", "/v1/customers/customer_123/checkout-settings", "default"},
		{"POST", "/v1/customers//checkout", "default"},
		{"POST", "/v1/me/payment-methods", "payment-methods"},
		{"PUT", "/billing/v1/customers/customer_123/payment-methods/pm_123", "payment-methods"},
		{"POST", "/v1/customers/customer_123/payment-methods-export", "default"},
		{"POST", "/v1/me/subscriptions/sub_123/cancel", "subscriptions"},
		{"delete", "/billing/v1/me/subscriptions/sub_123", "subscriptions"},
		{"GET", "/v1/me/subscriptions/sub_123", "default"},
	} {
		require.Equal(t, tc.want, ClassifyBucket(tc.path, tc.method), "%s %s", tc.method, tc.path)
	}
}

// Limits are per bucket and per subject (IP and user); either tripping blocks.
// #746: the IP subject is the proxy-resolved client, never a spoofable header.
func TestRateLimitSubjects(t *testing.T) {
	const a, b, lb = "203.0.113.10", "203.0.113.11", "10.0.0.5"
	const user = "11111111-1111-1111-1111-111111111111"
	for _, tc := range []struct {
		name     string
		resolver *iputil.TrustedProxies
		calls    []call
	}{
		{"per ip", nil, []call{{path: "/v1/checkout", ip: a, want: 200}, {path: "/v1/checkout", ip: a, want: 429}, {path: "/v1/checkout", ip: b, want: 200}}},
		{"buckets are independent", nil, []call{{path: "/v1/checkout", ip: a, want: 200}, {path: "/v1/checkout", ip: a, want: 429}, {method: "GET", path: "/v1/products", ip: a, want: 200}}},
		{"embedded prefix", nil, []call{{path: "/billing/v1/checkout", ip: a, want: 200}, {path: "/v1/checkout", ip: a, want: 429}}},
		{"per user across ips", nil, []call{{path: "/v1/checkout", ip: a, user: user, want: 200}, {path: "/v1/checkout", ip: b, user: user, want: 429}}},
		{"trusted proxy keys the client", iputil.ParseTrustedProxies([]string{"10.0.0.0/8"}), []call{
			{path: "/v1/checkout", ip: lb, xff: a, want: 200}, {path: "/v1/checkout", ip: lb, xff: a, want: 429}, {path: "/v1/checkout", ip: lb, xff: b, want: 200},
		}},
		{"untrusted forwarded header is ignored", nil, []call{{path: "/v1/checkout", ip: lb, xff: a, want: 200}, {path: "/v1/checkout", ip: lb, xff: b, want: 429}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			limits := config.RateLimitsConfig{"checkout": {RequestsPerMinute: 1}, "default": {RequestsPerMinute: 60}}
			auth := billingauth.AuthenticatorFunc(func(_ context.Context, r *http.Request) (billingauth.UserContext, error) {
				if u := r.Header.Get("X-Test-User"); u != "" {
					return billingauth.UserContext{UserID: u}, nil
				}
				return billingauth.UserContext{}, billingauth.ErrUnauthenticated
			})
			h := ChainHTTP(okHandler(), HTTPMiddleware(billingauth.Optional(auth)), RateLimitHTTP(&limits, nil, nil, nil, tc.resolver))
			for _, c := range tc.calls {
				c.do(t, h)
			}
		})
	}

	limits := config.RateLimitsConfig{"checkout": {RequestsPerMinute: 1}}
	h := RateLimitHTTP(&limits, nil, nil, nil, nil)(okHandler())
	first := call{path: "/v1/checkout", ip: a, want: 200}.do(t, h)
	require.Equal(t, "1", first.Header().Get("X-RateLimit-Limit"))
	require.Equal(t, "0", first.Header().Get("X-RateLimit-Remaining"))
	require.NotEmpty(t, first.Header().Get("X-RateLimit-Reset"))
	blocked := call{path: "/v1/checkout", ip: a, want: 429, body: "Rate limit exceeded"}.do(t, h)
	require.NotEmpty(t, blocked.Header().Get("Retry-After"))
	require.Nil(t, RateLimitHTTP(nil, nil, nil, nil, nil)(nil), "no limits config mounts nothing")
}

// Oversized payloads are shed before counting: declared lengths with 413, and
// chunked bodies by a MaxBytesReader the handler hits on read.
func TestRateLimitPayloadCaps(t *testing.T) {
	deps := newDeps(config.RateLimitsConfig{"checkout": {RequestsPerMinute: 10}}, nil, nil)
	var readErr error
	h := engine(deps, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, readErr = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	limit := BucketMaxContentLength["checkout"]

	req := httptest.NewRequest(http.MethodPost, "/v1/checkout", nil)
	req.ContentLength = limit + 1
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	require.Equal(t, http.StatusRequestEntityTooLarge, w.Code)
	require.Empty(t, deps.Store.Snapshot(), "rejected before any counting")

	req = httptest.NewRequest(http.MethodPost, "/v1/checkout", strings.NewReader(strings.Repeat("a", int(limit)+1)))
	req.ContentLength = -1
	h.ServeHTTP(httptest.NewRecorder(), req)
	var tooLarge *http.MaxBytesError
	require.ErrorAs(t, readErr, &tooLarge)

	req = httptest.NewRequest(http.MethodPost, "/v1/checkout", strings.NewReader(strings.Repeat("a", int(limit))))
	h.ServeHTTP(httptest.NewRecorder(), req)
	require.NoError(t, readErr, "a body exactly at the cap is readable")
}

// Extreme rate-limit abuse escalates to a captcha that is global across the
// protected buckets; solving it clears the challenge and resets the counters.
func TestCaptchaEscalation(t *testing.T) {
	v := &stubVerifier{valid: "good"}
	limits := config.RateLimitsConfig{"checkout": {RequestsPerMinute: 1}, "payment": {RequestsPerMinute: 1}, "webhook": {RequestsPerMinute: 1}, "default": {RequestsPerMinute: 1}}
	h := engine(newDeps(limits, captchaOn, v), okHandler())
	const ip = "203.0.113.72"
	for _, c := range []call{
		{path: "/v1/checkout", want: 200},
		{path: "/v1/checkout", want: 429},
		{path: "/v1/checkout", want: 403, body: `"site_key":"site-key"`},
		{path: "/v1/me/payment-methods", want: 403, body: "captcha_required"},
		{path: "/v1/webhooks/stripe", want: 200},
		{path: "/v1/webhooks/stripe", want: 429},
		{method: "GET", path: "/v1/captcha/status", want: 200},
		{method: "GET", path: "/billing/v1/captcha/client.js", want: 200},
		{path: "/v1/me/payment-methods", token: "bad", want: 403, body: "captcha_invalid"},
		{path: "/v1/me/payment-methods", token: "good", want: 200},
		{path: "/v1/checkout", want: 200},
	} {
		c.ip = ip
		w := c.do(t, h)
		if c.want == 403 {
			require.Equal(t, "true", w.Header().Get("X-Captcha-Required"))
		}
		if strings.HasPrefix(c.path, "/v1/webhooks") {
			require.NotContains(t, w.Body.String(), "captcha")
		}
	}
	require.Equal(t, 2, v.calls)
}

func TestCaptchaChallenges(t *testing.T) {
	ctx := context.Background()
	const ip, user = "203.0.113.47", "22222222-2222-2222-2222-222222222222"
	limits := config.RateLimitsConfig{"checkout": {RequestsPerMinute: 10}, "default": {RequestsPerMinute: 60}}

	t.Run("solve clears every subject and resets its counters", func(t *testing.T) {
		deps := newDeps(limits, captchaOn, &stubVerifier{valid: "good"})
		for _, key := range []string{"ip:" + ip, "user:" + user} {
			require.NoError(t, deps.ChallengeStore.MarkChallenged(ctx, key, time.Minute))
			deps.Store.SeedCounter("checkout", key, 9, time.Now().Add(time.Minute))
		}
		call{path: "/v1/checkout", ip: ip, user: user, token: "good", want: 200}.do(t, engine(deps, okHandler()))
		for _, key := range []string{"ip:" + ip, "user:" + user} {
			challenged, err := deps.ChallengeStore.IsChallenged(ctx, key)
			require.NoError(t, err)
			require.False(t, challenged, key)
			require.Equal(t, 1, deps.Store.Snapshot()["checkout:"+key])
		}
	})

	// FC-13: a verifier error is invalid, never a pass.
	t.Run("verifier error fails closed", func(t *testing.T) {
		deps := newDeps(limits, captchaOn, &stubVerifier{err: errors.New("siteverify down")})
		require.NoError(t, deps.ChallengeStore.MarkChallenged(ctx, "ip:"+ip, time.Minute))
		call{path: "/v1/checkout", ip: ip, token: "anything", want: 403, body: "captcha verification failed"}.do(t, engine(deps, okHandler()))
	})

	// #371: attack mode challenges everyone, and one solve does not lift it.
	t.Run("card attack mode", func(t *testing.T) {
		deps := newDeps(limits, captchaOn, &stubVerifier{valid: "good"})
		require.NoError(t, deps.ChallengeStore.MarkChallenged(ctx, captcha.CardAttackModeSubject, time.Minute))
		h := engine(deps, okHandler())
		call{path: "/v1/checkout", ip: ip, want: 403, body: "captcha_required"}.do(t, h)
		call{path: "/v1/checkout", ip: ip, token: "good", want: 200}.do(t, h)
		call{path: "/v1/checkout", ip: "198.51.100.3", want: 403}.do(t, h)
	})

	// Without a captcha to solve, a card-abuse block is a refusal on card buckets.
	t.Run("captcha disabled blocks card buckets", func(t *testing.T) {
		deps := newDeps(limits, nil, nil)
		require.NoError(t, deps.ChallengeStore.MarkChallenged(ctx, "ip:"+ip, time.Minute))
		h := engine(deps, okHandler())
		w := call{path: "/v1/checkout", ip: ip, want: 429}.do(t, h)
		require.Equal(t, "900", w.Header().Get("Retry-After"))
		call{path: "/v1/me/payment-methods", ip: ip, want: 429}.do(t, h)
		call{method: "GET", path: "/v1/products", ip: ip, want: 200}.do(t, h)
		call{path: "/v1/checkout", ip: "198.51.100.3", want: 200}.do(t, h)
	})
}

func TestRateLimitStoreIsBounded(t *testing.T) {
	limit := &config.RateLimit{RequestsPerMinute: 10}
	fill := func(s *RateLimitStore) {
		for i := 0; len(s.counters) < maxInMemoryRateLimitCounters; i++ {
			s.counters[fmt.Sprintf("checkout:ip:198.51.%d.%d", i/256, i%256)] = &inMemoryCounter{count: 1, reset: time.Now().Add(time.Hour)}
		}
	}

	s := NewRateLimitStore()
	s.counters["expired"] = &inMemoryCounter{count: 1, reset: time.Now().Add(-time.Minute)}
	fill(s)
	require.True(t, s.Allow("ip:203.0.113.30", "checkout", limit).allowed)
	require.NotContains(t, s.counters, "expired")
	require.LessOrEqual(t, len(s.counters), maxInMemoryRateLimitCounters)

	// Reusing a live counter never evicts another subject.
	s = NewRateLimitStore()
	s.counters["checkout:ip:203.0.113.32"] = &inMemoryCounter{count: 1, reset: time.Now().Add(time.Hour)}
	fill(s)
	require.True(t, s.Allow("ip:203.0.113.32", "checkout", limit).allowed)
	require.Len(t, s.counters, maxInMemoryRateLimitCounters)
	require.Equal(t, 2, s.counters["checkout:ip:203.0.113.32"].count)

	s = NewRateLimitStore()
	for i := 1; i <= 11; i++ {
		res := s.Allow("ip:a", "checkout", limit)
		require.Equal(t, i <= 10, res.allowed, "request %d", i)
		require.Equal(t, max(10-i, 0), res.remaining)
	}
	require.True(t, s.Allow("ip:a", "default", &config.RateLimit{}).allowed, "a zero limit defaults to 60/min")
}

// A host-mounted prefix keeps the host URL for auth while policy classifies the
// canonical route.
func TestRoutePathSelectsPolicyWithoutRewritingTheRequest(t *testing.T) {
	limits := config.RateLimitsConfig{
		"checkout": {RequestsPerMinute: 1}, "payment": {RequestsPerMinute: 2},
		"webhook": {RequestsPerMinute: 3}, "default": {RequestsPerMinute: 60},
	}
	for _, tc := range []struct {
		canonical, actual, bucket, limit string
		captcha                          bool
	}{
		{"/billing/v1/customers/{customer_id}/checkout", "/api/pay/v1/customers/a%2Fb/checkout?x=1", "checkout", "checkout", true},
		{"/billing/v1/me/payment-methods", "/api/pay/v1/me/payment-methods", "payment-methods", "payment", true},
		{"/billing/v1/webhooks/{provider}/{account_id}", "/api/pay/v1/webhooks/stripe/acct_test", "webhook", "webhook", false},
		{"/billing/v1/captcha/status", "/api/pay/v1/captcha/status", "captcha", "", false},
	} {
		req := httptest.NewRequest(http.MethodPost, tc.actual, nil)
		originalURL, originalURI := *req.URL, req.RequestURI
		WithRoutePath(tc.canonical)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			require.Equal(t, originalURL, *r.URL)
			require.Equal(t, originalURI, r.RequestURI)
			limit, bucket := resolveRateLimitPolicy(&limits, r)
			require.Equal(t, tc.bucket, bucket)
			require.Same(t, limits[tc.limit], limit)
			require.Equal(t, tc.captcha, captchaShouldEnforce(captchaOn, r, bucket))
		})).ServeHTTP(httptest.NewRecorder(), req)
	}

	h := WithRoutePath("/billing/v1/checkout")(engine(newDeps(limits, captchaOn, &stubVerifier{}), okHandler()))
	for _, want := range []int{200, 429, 403} {
		call{path: "/api/pay/v1/checkout", ip: "203.0.113.88", want: want}.do(t, h)
	}
}
