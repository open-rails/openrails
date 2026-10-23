package riverjobs

import (
	"testing"
	"time"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
	"github.com/stretchr/testify/require"
)

func TestUsageOrderDisabledCollectionDoesNotAccumulateHourlyScans(t *testing.T) {
	merchantID := billing.MerchantID{}
	for _, mode := range []string{config.ProviderWriteModeLimited, config.ProviderWriteModeReadOnly} {
		for _, tc := range []struct {
			name string
			args UsageOrderArgs
			wait time.Duration
		}{
			{"hourly scan", UsageOrderArgs{Collect: true}, 0},
			{"monthly slot", UsageOrderArgs{Collect: true, UseMonthlyFloor: true}, time.Hour},
			{"explicit merchant", UsageOrderArgs{Collect: true, MerchantID: &merchantID}, time.Minute},
			{"explicit merchant monthly", UsageOrderArgs{Collect: true, UseMonthlyFloor: true, MerchantID: &merchantID}, time.Minute},
			{"combined phases", UsageOrderArgs{Collect: true, FinalizePreviousMonth: true}, time.Minute},
		} {
			t.Run(mode+"/"+tc.name, func(t *testing.T) {
				worker := UsageOrderWorker{Money: &money.MoneyService{}, Config: &config.Config{ProviderWriteMode: mode}}
				err := worker.Work(t.Context(), &river.Job[UsageOrderArgs]{Args: tc.args})
				if tc.wait == 0 {
					require.NoError(t, err, "the next hourly/startup scan covers outstanding orders")
					return
				}
				var snooze *river.JobSnoozeError
				require.ErrorAs(t, err, &snooze)
				require.Equal(t, tc.wait, snooze.Duration)
			})
		}
	}
}

func TestUsageOrderEnabledCollectionLeavesDisabledGate(t *testing.T) {
	// An invalid explicit merchant proves enabled collection reaches its normal
	// validation, rather than completing or snoozing at the disabled-write gate.
	merchantID := billing.MerchantID{}
	worker := UsageOrderWorker{Money: &money.MoneyService{}, Config: &config.Config{ProviderWriteMode: config.ProviderWriteModeFull}}
	err := worker.Work(t.Context(), &river.Job[UsageOrderArgs]{Args: UsageOrderArgs{Collect: true, MerchantID: &merchantID}})
	require.EqualError(t, err, "a usage-order pass requires a nonzero merchant ID")
}

func TestLegacyPeriodicUsageCannotBypassHostOptIn(t *testing.T) {
	for _, args := range []UsageOrderArgs{{Collect: true}, {UseMonthlyFloor: true, Collect: true}, {FinalizePreviousMonth: true}} {
		w := UsageOrderWorker{Money: &money.MoneyService{}, Config: &config.Config{ProviderWriteMode: config.ProviderWriteModeFull}}
		require.NoError(t, w.Work(t.Context(), &river.Job[UsageOrderArgs]{JobRow: &rivertype.JobRow{Metadata: []byte(`{"periodic":true}`)}, Args: args}))
	}
}
