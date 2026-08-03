//go:build integration

package middleware

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/dbtest"
)

func TestAdminOperationLimiterRedisSharesLockoutAcrossInstances(t *testing.T) {
	rdb, ctx := dbtest.SharedRedisClient(t)
	now := time.Date(2026, time.August, 1, 12, 0, 0, 0, time.UTC)
	first := NewAdminOperationLimiter(rdb)
	first.now = func() time.Time { return now }
	second := NewAdminOperationLimiter(rdb)
	second.now = func() time.Time { return now }

	for i := 0; i < 5; i++ {
		require.True(t, first.evaluate(ctx, adminRateLimitTestUser, AdminOperationDestructive).allowed)
	}
	require.False(t, first.evaluate(ctx, adminRateLimitTestUser, AdminOperationDestructive).allowed)
	blocked := second.evaluate(ctx, adminRateLimitTestUser, AdminOperationGrant)
	require.False(t, blocked.allowed)
	require.True(t, blocked.wasLocked)

	require.NoError(t, second.Unlock(context.Background(), adminRateLimitTestUser, "root-operator"))
	require.True(t, first.evaluate(ctx, adminRateLimitTestUser, AdminOperationDestructive).allowed)
}
