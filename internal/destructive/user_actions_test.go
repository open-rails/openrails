package destructive

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/config"
	"github.com/stretchr/testify/require"
)

func TestUserActionRatePolicy(t *testing.T) {
	p, err := config.NormalizeUserActionRateLimit(config.UserActionRateLimitConfig{})
	require.NoError(t, err)
	require.EqualValues(t, 5, p.MinimumCustomersPerHour)
	require.EqualValues(t, 400, p.PaidCustomersPerHourlyCustomer)
	require.EqualValues(t, 10, p.MinimumCustomersPerDay)
	require.EqualValues(t, 100, p.PaidCustomersPerDailyCustomer)
	l := NewUserActionLimiter(nil, p)
	require.EqualValues(t, 40, l.limit(4000))
	require.EqualValues(t, 10, max(p.MinimumCustomersPerHour, ceil(4000, p.PaidCustomersPerHourlyCustomer)))
	for _, bad := range []config.UserActionRateLimitConfig{{MinimumCustomersPerHour: -1}, {PaidCustomersPerHourlyCustomer: -1}, {MinimumCustomersPerDay: -1}, {PaidCustomersPerDailyCustomer: -1}} {
		_, err := config.NormalizeUserActionRateLimit(bad)
		require.Error(t, err)
	}
}

func TestUserActionLimiterFailsClosedWithoutStore(t *testing.T) {
	var absent *UserActionLimiter
	for _, limiter := range []*UserActionLimiter{absent, NewUserActionLimiter(nil, config.UserActionRateLimitConfig{})} {
		require.Error(t, limiter.Reserve(context.Background(), DeletePaymentMethod, "method-delete:fixture", uuid.New()))
	}
}
