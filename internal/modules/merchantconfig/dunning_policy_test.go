package merchantconfig

import (
	"testing"
	"time"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/modules/collection"
	"github.com/stretchr/testify/require"
)

func TestDunningPolicyDurationBounds(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		policy billing.DunningPolicy
	}{
		{"negative cycle", billing.DunningPolicy{Tiers: []billing.DunningTier{{MaxCycleHours: -1}, {}}}},
		{"overflow cycle", billing.DunningPolicy{Tiers: []billing.DunningTier{{MaxCycleHours: catalog.MaxDurationHours + 1}, {}}}},
		{"wrapped cycle", billing.DunningPolicy{Tiers: []billing.DunningTier{{MaxCycleHours: 5124096}, {}}}},
		{"overflow offset", billing.DunningPolicy{Tiers: []billing.DunningTier{{MaxCycleHours: 96}, {RetryAfterHours: []int{catalog.MaxDurationHours + 1}}}}},
		{"overflow window", billing.DunningPolicy{Tiers: []billing.DunningTier{{MaxCycleHours: 96}, {RetryAfterHours: []int{catalog.MaxDurationHours}}}}},
		{"wrapped minutes", billing.DunningPolicy{TransientRetryMinutes: []int{307445735}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Normalize("merchant", billing.MerchantSettings{DunningPolicy: &tc.policy})
			require.ErrorContains(t, err, "dunning_policy")
		})
	}
	// The largest valid whole-hour window still fits inside its shortest cycle.
	declared := billing.DunningPolicy{Tiers: []billing.DunningTier{{MaxCycleHours: catalog.MaxDurationHours}, {RetryAfterHours: []int{catalog.MaxDurationHours - 25}}}, TransientRetryMinutes: []int{1, 120}}
	cfg, err := Normalize("merchant", billing.MerchantSettings{DunningPolicy: &declared})
	require.NoError(t, err)
	require.Equal(t, &declared, cfg.Config.DunningPolicy)
	policy, err := collection.PolicyOf(cfg.Config.DunningPolicy)
	require.NoError(t, err)
	window, err := policy.Window(catalog.MaxDurationHours)
	require.NoError(t, err)
	require.Equal(t, time.Duration(catalog.MaxDurationHours-1)*time.Hour, window)
}
