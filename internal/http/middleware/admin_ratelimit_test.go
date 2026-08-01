package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/http/router"
	"github.com/open-rails/openrails/pkg/billingauth"
)

const adminRateLimitTestUser = "11111111-1111-1111-1111-111111111111"

func newTestAdminLimiter(now *time.Time) (*AdminOperationLimiter, *[]AdminRateLimitEvent) {
	limiter := NewAdminOperationLimiter(nil)
	limiter.now = func() time.Time { return *now }
	events := make([]AdminRateLimitEvent, 0)
	limiter.sink = func(_ context.Context, event AdminRateLimitEvent) {
		events = append(events, event)
	}
	return limiter, &events
}

func TestAdminOperationLimiterDestructiveMinuteLimitAndLockout(t *testing.T) {
	now := time.Date(2026, time.August, 1, 12, 0, 0, 0, time.UTC)
	limiter, _ := newTestAdminLimiter(&now)

	for i := 1; i <= 5; i++ {
		decision := limiter.evaluate(context.Background(), adminRateLimitTestUser, AdminOperationDestructive)
		require.True(t, decision.allowed, "request %d", i)
		require.Equal(t, int64(i), decision.counts["minute"])
	}

	decision := limiter.evaluate(context.Background(), adminRateLimitTestUser, AdminOperationDestructive)
	require.False(t, decision.allowed)
	require.False(t, decision.wasLocked)
	require.Equal(t, adminLockoutDuration, decision.retryAfter)

	decision = limiter.evaluate(context.Background(), adminRateLimitTestUser, AdminOperationGrant)
	require.False(t, decision.allowed, "lockout applies across protected operations")
	require.True(t, decision.wasLocked)

}

func TestAdminOperationLimiterCanonicalizesUserID(t *testing.T) {
	now := time.Date(2026, time.August, 1, 12, 0, 0, 0, time.UTC)
	limiter, _ := newTestAdminLimiter(&now)
	uppercase := "ABCDEFAB-CDEF-4ABC-8DEF-ABCDEFABCDEF"
	lowercase := "abcdefab-cdef-4abc-8def-abcdefabcdef"

	first := limiter.evaluate(context.Background(), uppercase, AdminOperationDestructive)
	second := limiter.evaluate(context.Background(), lowercase, AdminOperationDestructive)

	require.Equal(t, int64(1), first.counts["minute"])
	require.Equal(t, int64(2), second.counts["minute"])
}

func TestAdminOperationLimiterHourlyAndDailyWindows(t *testing.T) {
	t.Run("hour", func(t *testing.T) {
		now := time.Date(2026, time.August, 1, 12, 1, 0, 0, time.UTC)
		limiter, _ := newTestAdminLimiter(&now)
		for i := 0; i < 10; i++ {
			decision := limiter.evaluate(context.Background(), adminRateLimitTestUser, AdminOperationDestructive)
			require.True(t, decision.allowed, "request %d", i+1)
			now = now.Add(2 * time.Minute)
		}
		decision := limiter.evaluate(context.Background(), adminRateLimitTestUser, AdminOperationDestructive)
		require.False(t, decision.allowed)
		require.Equal(t, int64(11), decision.counts["hour"])
	})

	t.Run("day", func(t *testing.T) {
		now := time.Date(2026, time.August, 1, 0, 1, 0, 0, time.UTC)
		limiter, _ := newTestAdminLimiter(&now)
		for hour := 0; hour < 10; hour++ {
			for requestNumber := 0; requestNumber < 5; requestNumber++ {
				decision := limiter.evaluate(context.Background(), adminRateLimitTestUser, AdminOperationDestructive)
				require.True(t, decision.allowed, "hour %d request %d", hour, requestNumber+1)
			}
			now = now.Add(time.Hour)
		}
		decision := limiter.evaluate(context.Background(), adminRateLimitTestUser, AdminOperationDestructive)
		require.False(t, decision.allowed)
		require.Equal(t, int64(51), decision.counts["day"])
	})
}

func TestAdminOperationLimiterExpensiveOperationPolicies(t *testing.T) {
	tests := []struct {
		name      string
		operation AdminOperation
		limit     int
	}{
		{name: "extend", operation: AdminOperationExtend, limit: 3},
		{name: "off channel", operation: AdminOperationOffChannel, limit: 10},
		{name: "admin grant", operation: AdminOperationGrant, limit: 10},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Date(2026, time.August, 1, 12, 0, 0, 0, time.UTC)
			limiter, _ := newTestAdminLimiter(&now)
			for i := 0; i < tc.limit; i++ {
				require.True(t, limiter.evaluate(context.Background(), adminRateLimitTestUser, tc.operation).allowed)
			}
			require.False(t, limiter.evaluate(context.Background(), adminRateLimitTestUser, tc.operation).allowed)
		})
	}
}

func TestAdminOperationLimiterThresholdAuditAndHTTPResponse(t *testing.T) {
	now := time.Date(2026, time.August, 1, 12, 0, 0, 0, time.UTC)
	limiter, events := newTestAdminLimiter(&now)
	handled := 0
	handler := router.Chain(func(_ *request.Request) { handled++ }, []router.Middleware{
		func(next router.Handler) router.Handler {
			return func(r *request.Request) {
				r.SetUserContext(billingauth.UserContext{UserID: adminRateLimitTestUser})
				next(r)
			}
		},
		limiter.AdminRateLimitMW(AdminOperationDestructive),
	})

	for i := 0; i < 6; i++ {
		recorder := httptest.NewRecorder()
		httpRequest := httptest.NewRequest(http.MethodPost, "/v1/merchant/subscriptions/sub_1/cancel", nil)
		handler(request.NewHTTP(recorder, httpRequest, nil))
		if i < 5 {
			require.Equal(t, http.StatusOK, recorder.Code)
			continue
		}
		require.Equal(t, http.StatusTooManyRequests, recorder.Code)
		require.Equal(t, "3600", recorder.Header().Get("Retry-After"))
		require.Contains(t, recorder.Body.String(), `"code":"rate_limit_exceeded"`)
	}
	require.Equal(t, 5, handled)

	var thresholdEvents, lockoutEvents int
	for _, event := range *events {
		switch event.Kind {
		case "threshold":
			thresholdEvents++
			require.Equal(t, map[string]int64{"minute": 4}, event.Counts)
		case "lockout":
			lockoutEvents++
		}
	}
	require.Equal(t, 1, thresholdEvents, "80%% alert fires once on the crossing")
	require.Equal(t, 1, lockoutEvents)
}

func TestAdminOperationLimiterUnlockClearsLockAndCounters(t *testing.T) {
	now := time.Date(2026, time.August, 1, 12, 0, 0, 0, time.UTC)
	limiter, events := newTestAdminLimiter(&now)
	for i := 0; i < 6; i++ {
		_ = limiter.evaluate(context.Background(), adminRateLimitTestUser, AdminOperationDestructive)
	}

	actorID := "22222222-2222-2222-2222-222222222222"
	require.NoError(t, limiter.Unlock(context.Background(), adminRateLimitTestUser, actorID))
	decision := limiter.evaluate(context.Background(), adminRateLimitTestUser, AdminOperationDestructive)
	require.True(t, decision.allowed)
	require.Equal(t, int64(1), decision.counts["minute"])
	require.Equal(t, AdminRateLimitEvent{
		Kind:    "unlocked",
		UserID:  adminRateLimitTestUser,
		ActorID: actorID,
	}, (*events)[len(*events)-1])
}

func TestAdminOperationLimiterServicePrincipalIsNotUserBucketed(t *testing.T) {
	now := time.Date(2026, time.August, 1, 12, 0, 0, 0, time.UTC)
	limiter, _ := newTestAdminLimiter(&now)
	handled := 0
	handler := limiter.AdminRateLimitMW(AdminOperationDestructive)(func(_ *request.Request) { handled++ })

	for i := 0; i < 20; i++ {
		recorder := httptest.NewRecorder()
		handler(request.NewHTTP(recorder, httptest.NewRequest(http.MethodPost, "/operation", nil), nil))
		require.Equal(t, http.StatusOK, recorder.Code)
	}
	require.Equal(t, 20, handled)
}
