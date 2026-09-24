package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/pkg/billingauth"
)

const adminUser = "11111111-1111-1111-1111-111111111111"

func testAdminLimiter(now *time.Time) (*AdminOperationLimiter, *[]AdminRateLimitEvent) {
	l := NewAdminOperationLimiter(nil)
	l.now = func() time.Time { return *now }
	var events []AdminRateLimitEvent
	l.sink = func(_ context.Context, e AdminRateLimitEvent) { events = append(events, e) }
	return l, &events
}

// Per-human-admin budgets: destructive actions share minute/hour/day windows;
// the first breach locks the admin out of every protected operation for an hour.
func TestAdminOperationLimits(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name      string
		op        AdminOperation
		limit     int
		step      time.Duration
		start     time.Time
		window    string
		breachCnt int64
	}{
		{"destructive minute", AdminOperationDestructive, 5, 0, time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC), "minute", 6},
		{"destructive hour", AdminOperationDestructive, 10, 2 * time.Minute, time.Date(2026, 8, 1, 12, 1, 0, 0, time.UTC), "hour", 11},
		{"destructive day", AdminOperationDestructive, 50, 12 * time.Minute, time.Date(2026, 8, 1, 0, 1, 0, 0, time.UTC), "day", 51},
		{"extend", AdminOperationExtend, 3, 0, time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC), "minute", 4},
		{"off channel", AdminOperationOffChannel, 10, 0, time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC), "minute", 11},
		{"grant", AdminOperationGrant, 10, 0, time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC), "minute", 11},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := tc.start
			l, _ := testAdminLimiter(&now)
			for i := 1; i <= tc.limit; i++ {
				require.True(t, l.evaluate(ctx, adminUser, tc.op).allowed, "request %d", i)
				now = now.Add(tc.step)
			}
			d := l.evaluate(ctx, adminUser, tc.op)
			require.False(t, d.allowed)
			require.False(t, d.wasLocked)
			require.Equal(t, tc.breachCnt, d.counts[tc.window])
			require.Equal(t, adminLockoutDuration, d.retryAfter)

			now = now.Add(adminLockoutDuration - time.Second)
			d = l.evaluate(ctx, adminUser, AdminOperationGrant)
			require.False(t, d.allowed, "the lockout spans operations")
			require.True(t, d.wasLocked)
			require.True(t, l.evaluate(ctx, "22222222-2222-2222-2222-222222222222", tc.op).allowed, "other admins are unaffected")
		})
	}

	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	l, _ := testAdminLimiter(&now)
	require.EqualValues(t, 1, l.evaluate(ctx, "ABCDEFAB-CDEF-4ABC-8DEF-ABCDEFABCDEF", AdminOperationDestructive).counts["minute"])
	require.EqualValues(t, 2, l.evaluate(ctx, " abcdefab-cdef-4abc-8def-abcdefabcdef ", AdminOperationDestructive).counts["minute"], "user ids are canonicalized")
	require.True(t, l.evaluate(ctx, adminUser, AdminOperation("unknown")).allowed)
}

func TestAdminRateLimitMW(t *testing.T) {
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	l, events := testAdminLimiter(&now)
	handled := 0
	serve := func(user string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		r := request.NewHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/merchant/subscriptions/sub_1/cancel", nil), nil)
		if user != "" {
			r.SetUserContext(billingauth.UserContext{UserID: user})
		}
		l.AdminRateLimitMW(AdminOperationDestructive)(func(*request.Request) { handled++ })(r)
		return w
	}

	for i := 1; i <= 6; i++ {
		w := serve(adminUser)
		if i <= 5 {
			require.Equal(t, http.StatusOK, w.Code)
			continue
		}
		require.Equal(t, http.StatusTooManyRequests, w.Code)
		require.Equal(t, "3600", w.Header().Get("Retry-After"))
		require.Contains(t, w.Body.String(), `"code":"rate_limit_exceeded"`)
	}
	require.Equal(t, 5, handled)
	kinds := map[string]int{}
	for _, e := range *events {
		kinds[e.Kind]++
		if e.Kind == "threshold" {
			require.Equal(t, map[string]int64{"minute": 4}, e.Counts, "the 80% alert fires once, on the crossing")
		}
	}
	require.Equal(t, map[string]int{"allowed": 5, "threshold": 1, "lockout": 1}, kinds)

	require.Equal(t, http.StatusTooManyRequests, serve(adminUser).Code)
	require.Equal(t, "blocked", (*events)[len(*events)-1].Kind)

	const actor = "22222222-2222-2222-2222-222222222222"
	require.NoError(t, l.Unlock(context.Background(), " "+adminUser+" ", actor))
	require.Equal(t, AdminRateLimitEvent{Kind: "unlocked", UserID: adminUser, ActorID: actor}, (*events)[len(*events)-1])
	require.Equal(t, http.StatusOK, serve(adminUser).Code)
	require.EqualValues(t, 1, (*events)[len(*events)-1].Counts["minute"], "unlock clears the counters too")
	require.Error(t, l.Unlock(context.Background(), "not-a-uuid", actor))

	// Service credentials carry no human user and keep their own admission.
	handled = 0
	for range 20 {
		require.Equal(t, http.StatusOK, serve("").Code)
	}
	require.Equal(t, 20, handled)
	require.Equal(t, http.StatusInternalServerError, serve("not-a-uuid").Code)
}
