package subscriptions

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/modules/collection"
)

func TestEngineRenewalGraceIsBoundedByThePeriod(t *testing.T) {
	day := 24 * time.Hour
	for _, tc := range []struct {
		period, grace time.Duration
	}{
		{time.Hour, 6 * time.Minute},
		{day, 144 * time.Minute},
		{7 * day, 16*time.Hour + 48*time.Minute},
		{720 * time.Hour, day},
		{90 * day, day},
		{365 * day, day},
	} {
		grace, err := EngineRenewalGrace(tc.period)
		require.NoError(t, err)
		require.Equal(t, tc.grace, grace, "period %s", tc.period)
		require.LessOrEqual(t, grace*engineGraceFraction, tc.period, "period %s: unpaid access is at most a tenth of the paid period", tc.period)
		require.LessOrEqual(t, grace, engineGraceCap)
	}
	for _, period := range []time.Duration{0, -time.Hour} {
		_, err := EngineRenewalGrace(period)
		require.ErrorIs(t, err, collection.ErrUnknownCycle)
	}
	deadline, err := SubscriptionCollectionPayload{AcceptedAt: time.Unix(0, 0), Renewal: RenewalTerms{PeriodStart: time.Unix(0, 0), PeriodEnd: time.Unix(3600, 0)}}.AuthenticationDeadline()
	require.NoError(t, err)
	require.Equal(t, time.Unix(360, 0), deadline, "an hourly renewal's challenge waits six minutes, not a day")
}
