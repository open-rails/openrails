package hostconfig

import (
	"path/filepath"
	"reflect"
	"testing"

	"github.com/open-rails/openrails/internal/config"
	"github.com/stretchr/testify/require"
)

func TestUserActionRateLimitConfiguration(t *testing.T) {
	bootEnv(t)
	path := writeFile(t, filepath.Join(t.TempDir(), "rate.yaml"), `user_action_rate_limit:
  minimum_customers_per_hour: 10
  paid_customers_per_hourly_customer: 400
  minimum_customers_per_day: 40
  paid_customers_per_daily_customer: 100
`)
	got, err := Load(path)
	require.NoError(t, err)
	require.Equal(t, config.UserActionRateLimitConfig{MinimumCustomersPerHour: 10, PaidCustomersPerHourlyCustomer: 400, MinimumCustomersPerDay: 40, PaidCustomersPerDailyCustomer: 100}, got.UserActionRateLimit)
	expected := map[string]bool{"minimum_customers_per_hour": true, "paid_customers_per_hourly_customer": true, "minimum_customers_per_day": true, "paid_customers_per_daily_customer": true}
	typ := reflect.TypeFor[config.UserActionRateLimitConfig]()
	require.Equal(t, len(expected), typ.NumField(), "document every real setting")
	for i := 0; i < typ.NumField(); i++ {
		require.True(t, expected[typ.Field(i).Tag.Get("yaml")])
	}
	for _, body := range []string{"minimum_per_minute: 1", "unknown: 1", "minimum_customers_per_hour: -1"} {
		path := writeFile(t, filepath.Join(t.TempDir(), "bad.yaml"), "user_action_rate_limit:\n  "+body+"\n")
		_, err := Load(path)
		require.Error(t, err)
	}
}
