package middleware

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/abusestate"
	"github.com/open-rails/openrails/internal/captcha"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/shared/iputil"
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
	merchant                           billing.MerchantID
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
	if !c.merchant.IsZero() {
		req = req.WithContext(merchant.WithID(req.Context(), c.merchant))
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
		applyRateLimitDecisionHTTP(w, r, next, EvaluateRateLimit(w, r, rateLimitSubjectsHTTP(r, nil), deps), deps.Captcha)
	})
}

// stillState is abuse state whose clock stands mid-window, so a test's
// requests never straddle a clock-aligned window's end.
func stillState(rdb *redis.Client) *abusestate.Store {
	s := abusestate.New(rdb)
	at := time.Date(2026, 1, 1, 12, 0, 30, 0, time.UTC)
	s.SetClock(func() time.Time { return at })
	return s
}

func newDeps(limits config.RateLimitsConfig, cfg *config.CaptchaConfig, v captcha.Verifier) RateLimitDeps {
	state := stillState(nil)
	return RateLimitDeps{Limits: &limits, Captcha: cfg, State: state, ChallengeStore: captcha.NewChallengeStore(state), Verifier: v}
}

func TestClassifyBucket(t *testing.T) {
	for _, tc := range []struct{ method, path, want string }{
		{"POST", "/v1/webhooks/stripe", "webhook"},
		{"POST", "/billing/v1/webhooks/stripe/acct_test", "webhook"},
		{"POST", "/v1/me/orders", "checkout"},
		{"POST", "/billing/v1/me/orders/ord_1/pay", "checkout"},
		{"POST", "/billing/v1/me/orders/{id}/confirm", "checkout"},
		{"POST", "/v1/me/orders/preview", ""},
		{"POST", "/v1/me/orders/ord_1/cancel", ""},
		{"GET", "/v1/me/orders/ord_1", ""},
		{"POST", "/v1/orders/ord_1/solana-pay", "checkout"},
		{"POST", "/v1/config", ""},
		{"POST", "/v1/app/orders", ""},
		{"POST", "/v1/me/payment-methods", "payment-methods"},
		{"POST", "/v1/customers/customer_123/checkout", ""},
		{"POST", "/v1/me/subscriptions/sub_123/cancel", "subscriptions"},
		{"delete", "/billing/v1/me/subscriptions/sub_123", "subscriptions"},
		{"GET", "/v1/me/subscriptions/sub_123", ""},
	} {
		require.Equal(t, tc.want, ClassifyBucket(tc.path, tc.method), "%s %s", tc.method, tc.path)
	}
}

// Limits are per bucket and per client IP, before any route authenticates
// its caller. The IP subject is the proxy-resolved client, never a spoofable
// header.
func TestRateLimitSubjects(t *testing.T) {
	const a, b, lb = "203.0.113.10", "203.0.113.11", "10.0.0.5"
	const user = "11111111-1111-1111-1111-111111111111"
	for _, tc := range []struct {
		name     string
		resolver *iputil.TrustedProxies
		calls    []call
	}{
		{"per ip", nil, []call{{path: "/v1/me/orders", ip: a, want: 200}, {path: "/v1/me/orders", ip: a, want: 429}, {path: "/v1/me/orders", ip: b, want: 200}}},
		{"buckets are independent", nil, []call{{path: "/v1/me/orders", ip: a, want: 200}, {path: "/v1/me/orders", ip: a, want: 429}, {method: "GET", path: "/v1/products", ip: a, want: 200}}},
		{"embedded prefix", nil, []call{{path: "/billing/v1/me/orders", ip: a, want: 200}, {path: "/v1/me/orders", ip: a, want: 429}}},
		{"a user header is no subject", nil, []call{{path: "/v1/me/orders", ip: a, user: user, want: 200}, {path: "/v1/me/orders", ip: b, user: user, want: 200}}},
		{"trusted proxy keys the client", iputil.ParseTrustedProxies([]string{"10.0.0.0/8"}), []call{
			{path: "/v1/me/orders", ip: lb, xff: a, want: 200}, {path: "/v1/me/orders", ip: lb, xff: a, want: 429}, {path: "/v1/me/orders", ip: lb, xff: b, want: 200},
		}},
		{"untrusted forwarded header is ignored", nil, []call{{path: "/v1/me/orders", ip: lb, xff: a, want: 200}, {path: "/v1/me/orders", ip: lb, xff: b, want: 429}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			limits := config.RateLimitsConfig{"checkout": {RequestsPerMinute: 1}}
			h := ChainHTTP(okHandler(), RateLimitHTTP(&limits, nil, nil, nil, tc.resolver))
			for _, c := range tc.calls {
				c.do(t, h)
			}
		})
	}

	limits := config.RateLimitsConfig{"checkout": {RequestsPerMinute: 1}}
	h := RateLimitHTTP(&limits, nil, nil, nil, nil)(okHandler())
	first := call{path: "/v1/me/orders", ip: a, want: 200}.do(t, h)
	require.Equal(t, "1", first.Header().Get("X-RateLimit-Limit"))
	require.Equal(t, "0", first.Header().Get("X-RateLimit-Remaining"))
	require.NotEmpty(t, first.Header().Get("X-RateLimit-Reset"))
	blocked := call{path: "/v1/me/orders", ip: a, want: 429, body: "Rate limit exceeded"}.do(t, h)
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

	req := httptest.NewRequest(http.MethodPost, "/v1/me/orders", nil)
	req.ContentLength = limit + 1
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	require.Equal(t, http.StatusRequestEntityTooLarge, w.Code)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/me/orders", nil))
	require.Equal(t, "9", w.Header().Get("X-RateLimit-Remaining"), "rejected before any counting")

	req = httptest.NewRequest(http.MethodPost, "/v1/me/orders", strings.NewReader(strings.Repeat("a", int(limit)+1)))
	req.ContentLength = -1
	h.ServeHTTP(httptest.NewRecorder(), req)
	var tooLarge *http.MaxBytesError
	require.ErrorAs(t, readErr, &tooLarge)

	req = httptest.NewRequest(http.MethodPost, "/v1/me/orders", strings.NewReader(strings.Repeat("a", int(limit))))
	h.ServeHTTP(httptest.NewRecorder(), req)
	require.NoError(t, readErr, "a body exactly at the cap is readable")
}

// Extreme rate-limit abuse escalates to a captcha that is global across the
// protected buckets; solving it clears the challenge and resets the counters.
func TestCaptchaEscalation(t *testing.T) {
	v := &stubVerifier{valid: "good"}
	limits := config.RateLimitsConfig{"checkout": {RequestsPerMinute: 1}, "payment": {RequestsPerMinute: 1}, "webhook": {RequestsPerMinute: 1}}
	h := engine(newDeps(limits, captchaOn, v), okHandler())
	const ip = "203.0.113.72"
	for _, c := range []call{
		{path: "/v1/me/orders", want: 200},
		{path: "/v1/me/orders", want: 429},
		{path: "/v1/me/orders", want: 403, body: `"site_key":"site-key"`},
		{path: "/v1/me/payment-methods", want: 403, body: "captcha_required"},
		{path: "/v1/webhooks/stripe", want: 200},
		{path: "/v1/webhooks/stripe", want: 429},
		{path: "/v1/me/payment-methods", token: "bad", want: 403, body: "captcha_invalid"},
		{path: "/v1/me/payment-methods", token: "good", want: 200},
		{path: "/v1/me/orders", want: 200},
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
	limits := config.RateLimitsConfig{"checkout": {RequestsPerMinute: 10}, "payment": {RequestsPerMinute: 60}}

	t.Run("solve clears every subject and resets its counters", func(t *testing.T) {
		deps := newDeps(limits, captchaOn, &stubVerifier{valid: "good"})
		for _, key := range []string{"ip:" + ip} {
			deps.ChallengeStore.MarkChallenged(ctx, key, time.Minute)
			deps.State.Count(ctx, rateLimitKey("checkout", key), 9, time.Minute)
		}
		call{path: "/v1/me/orders", ip: ip, user: user, token: "good", want: 200}.do(t, engine(deps, okHandler()))
		for _, key := range []string{"ip:" + ip} {
			require.False(t, deps.ChallengeStore.IsChallenged(ctx, key), key)
			count, _ := deps.State.Count(ctx, rateLimitKey("checkout", key), 0, time.Minute)
			require.EqualValues(t, 1, count)
		}
	})

	// A verifier error is invalid, never a pass.
	t.Run("verifier error fails closed", func(t *testing.T) {
		deps := newDeps(limits, captchaOn, &stubVerifier{err: errors.New("siteverify down")})
		deps.ChallengeStore.MarkChallenged(ctx, "ip:"+ip, time.Minute)
		call{path: "/v1/me/orders", ip: ip, token: "anything", want: 403, body: "captcha verification failed"}.do(t, engine(deps, okHandler()))
	})

	// Attack mode challenges everyone on the attacked merchant's card routes,
	// and one solve does not lift it. Other merchants, and every merchant or
	// API route, are never challenged.
	t.Run("card attack mode", func(t *testing.T) {
		attacked, other := billing.MerchantID(uuid.New()), billing.MerchantID(uuid.New())
		deps := newDeps(limits, captchaOn, &stubVerifier{valid: "good"})
		deps.ChallengeStore.MarkChallenged(ctx, captcha.CardAttackModeSubject(attacked.UUID()), time.Minute)
		h := engine(deps, okHandler())
		call{path: "/v1/me/orders", ip: ip, merchant: attacked, want: 403, body: "captcha_required"}.do(t, h)
		call{path: "/v1/me/orders", ip: ip, merchant: attacked, token: "good", want: 200}.do(t, h)
		call{path: "/v1/me/orders", ip: "198.51.100.3", merchant: attacked, want: 403}.do(t, h)
		call{method: "GET", path: "/v1/admin/findings", ip: "198.51.100.3", merchant: attacked, want: 200}.do(t, h)
		call{path: "/v1/me/orders", ip: "198.51.100.3", merchant: other, want: 200}.do(t, h)
		call{path: "/v1/me/orders", ip: "198.51.100.4", want: 200}.do(t, h)
	})

	t.Run("a challenged subject meets the captcha on card routes only", func(t *testing.T) {
		deps := newDeps(limits, captchaOn, &stubVerifier{valid: "good"})
		deps.ChallengeStore.MarkChallenged(ctx, "ip:"+ip, time.Minute)
		h := engine(deps, okHandler())
		call{path: "/v1/me/payment-methods", ip: ip, want: 403, body: "captcha_required"}.do(t, h)
		call{method: "GET", path: "/v1/admin/findings", ip: ip, want: 200}.do(t, h)
		call{path: "/v1/admin/admissions", ip: ip, want: 200}.do(t, h)
	})

	// Without a captcha to solve, neither a challenged subject nor a merchant's
	// attack flag refuses anyone here: the durable ledger blocks card attempts
	// per subject, so attack mode never becomes a blanket 429.
	t.Run("captcha disabled never refuses on a challenge", func(t *testing.T) {
		attacked := billing.MerchantID(uuid.New())
		deps := newDeps(limits, nil, nil)
		deps.ChallengeStore.MarkChallenged(ctx, "ip:"+ip, time.Minute)
		deps.ChallengeStore.MarkChallenged(ctx, captcha.CardAttackModeSubject(attacked.UUID()), time.Minute)
		h := engine(deps, okHandler())
		call{path: "/v1/me/orders", ip: ip, merchant: attacked, want: 200}.do(t, h)
		call{path: "/v1/me/payment-methods", ip: "198.51.100.3", merchant: attacked, want: 200}.do(t, h)
	})
}

func TestFixedWindowLimit(t *testing.T) {
	ctx := context.Background()
	state := stillState(nil)
	limit := &config.RateLimit{RequestsPerMinute: 10}
	for i := 1; i <= 11; i++ {
		res := allow(ctx, state, "ip:a", "checkout", effectiveLimit(limit))
		require.Equal(t, i <= 10, res.allowed, "request %d", i)
		require.Equal(t, max(10-i, 0), res.remaining)
	}
	require.True(t, allow(ctx, state, "ip:b", "checkout", effectiveLimit(&config.RateLimit{})).allowed, "a zero limit defaults to 60/min")
}

// A host-mounted prefix keeps the host URL for auth while policy classifies the
// canonical route.
func TestRoutePathSelectsPolicyWithoutRewritingTheRequest(t *testing.T) {
	limits := config.RateLimitsConfig{
		"checkout": {RequestsPerMinute: 1}, "payment": {RequestsPerMinute: 2},
		"webhook": {RequestsPerMinute: 3},
	}
	for _, tc := range []struct {
		canonical, actual, bucket, limit string
		captcha                          bool
	}{
		{"/billing/v1/me/orders/{id}/pay", "/api/pay/v1/me/orders/ord_a%2Fb/pay?x=1", "checkout", "checkout", true},
		{"/billing/v1/me/payment-methods", "/api/pay/v1/me/payment-methods", "payment-methods", "payment", true},
		{"/billing/v1/webhooks/{provider}/{account_id}", "/api/pay/v1/webhooks/stripe/acct_test", "webhook", "webhook", false},
	} {
		req := httptest.NewRequest(http.MethodPost, tc.actual, nil)
		originalURL, originalURI := *req.URL, req.RequestURI
		WithRoutePath(tc.canonical)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			require.Equal(t, originalURL, *r.URL)
			require.Equal(t, originalURI, r.RequestURI)
			limit, bucket := resolveRateLimitPolicy(&limits, r)
			require.Equal(t, tc.bucket, bucket)
			require.Same(t, limits[tc.limit], limit)
			require.Equal(t, tc.captcha, captcha.ShouldApply(captchaOn, bucket))
		})).ServeHTTP(httptest.NewRecorder(), req)
	}

	h := WithRoutePath("/billing/v1/me/orders")(engine(newDeps(limits, captchaOn, &stubVerifier{}), okHandler()))
	for _, want := range []int{200, 429, 403} {
		call{path: "/api/pay/v1/me/orders", ip: "203.0.113.88", want: want}.do(t, h)
	}
}
