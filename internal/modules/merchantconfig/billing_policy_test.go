package merchantconfig

import (
	"testing"

	"github.com/open-rails/openrails/internal/db/models"
	"github.com/stretchr/testify/require"
)

// The two seed shapes must survive the validator unchanged — the API business's
// $200 debt line and the cloud business's $2k/month spend window.
func TestNormalizeBillingPolicy_SeedShapes(t *testing.T) {
	api, err := NormalizeBillingPolicy("api_line", models.BillingPolicy{
		Kind:                 models.BillingPolicyOutstandingCap,
		OutstandingCapAmount: 200_000_000,
	})
	require.NoError(t, err)
	require.Equal(t, models.BillingPolicyOutstandingCap, api.Kind)
	require.Equal(t, int64(200_000_000), api.OutstandingCapAmount)

	cloud, err := NormalizeBillingPolicy("cloud_window", models.BillingPolicy{
		Kind: models.BillingPolicyWindowSpendCap,
		SpendWindows: []models.BudgetWindowPolicy{
			{Key: "monthly", WindowSeconds: 30 * 24 * 3600, Limit: 2_000_000_000, Currency: "usd"},
		},
	})
	require.NoError(t, err)
	require.Len(t, cloud.SpendWindows, 1)
	require.Equal(t, "USD", cloud.SpendWindows[0].Currency, "currency is canonicalized")
}

// Each kind accepts only ITS limit. Storing the other kind's field would be a
// cap that looks enforced and is never read.
func TestNormalizeBillingPolicy_RefusesCrossKindLimits(t *testing.T) {
	_, err := NormalizeBillingPolicy("mixed", models.BillingPolicy{
		Kind:         models.BillingPolicyOutstandingCap,
		SpendWindows: []models.BudgetWindowPolicy{{Key: "m", WindowSeconds: 60, Limit: 1}},
	})
	require.ErrorContains(t, err, "spend_windows belong to kind window_spend_cap")

	_, err = NormalizeBillingPolicy("mixed", models.BillingPolicy{
		Kind:                 models.BillingPolicyWindowSpendCap,
		OutstandingCapAmount: 5,
		SpendWindows:         []models.BudgetWindowPolicy{{Key: "m", WindowSeconds: 60, Limit: 1}},
	})
	require.ErrorContains(t, err, "outstanding_cap_amount belongs to kind outstanding_cap")
}

func TestNormalizeBillingPolicy_KindErrors(t *testing.T) {
	_, err := NormalizeBillingPolicy("p", models.BillingPolicy{})
	require.ErrorContains(t, err, "kind is required")

	_, err = NormalizeBillingPolicy("p", models.BillingPolicy{Kind: "spend_cap"})
	require.ErrorContains(t, err, `unknown kind "spend_cap"`)

	// Representable, deliberately refused with a real answer rather than the
	// misleading "unknown kind" (or#897 PR 3 builds the rate measurement).
	_, err = NormalizeBillingPolicy("quota", models.BillingPolicy{Kind: models.BillingPolicyAccrualRateCap})
	require.ErrorContains(t, err, "not implemented yet")
	require.ErrorContains(t, err, "billing policy quota")
}

func TestNormalizeBillingPolicy_WindowValidation(t *testing.T) {
	base := func(ws []models.BudgetWindowPolicy) models.BillingPolicy {
		return models.BillingPolicy{Kind: models.BillingPolicyWindowSpendCap, SpendWindows: ws}
	}
	_, err := NormalizeBillingPolicy("p", base(nil))
	require.ErrorContains(t, err, "requires at least one spend_windows entry")

	_, err = NormalizeBillingPolicy("p", base([]models.BudgetWindowPolicy{{Key: "", WindowSeconds: 60, Limit: 1}}))
	require.ErrorContains(t, err, "spend_windows[0].key is required")

	_, err = NormalizeBillingPolicy("p", base([]models.BudgetWindowPolicy{{Key: "m", WindowSeconds: 0, Limit: 1}}))
	require.ErrorContains(t, err, "window_seconds must be positive")

	_, err = NormalizeBillingPolicy("p", base([]models.BudgetWindowPolicy{{Key: "m", WindowSeconds: 60, Limit: -1}}))
	require.ErrorContains(t, err, "limit must be non-negative")

	// A repeated key would silently collapse into one metered bucket, quietly
	// enforcing whichever entry landed last.
	_, err = NormalizeBillingPolicy("p", base([]models.BudgetWindowPolicy{
		{Key: "m", WindowSeconds: 60, Limit: 1},
		{Key: "m", WindowSeconds: 3600, Limit: 2},
	}))
	require.ErrorContains(t, err, `repeats key "m"`)

	_, err = NormalizeBillingPolicy("p", base([]models.BudgetWindowPolicy{{Key: "m", WindowSeconds: 60, Limit: 1, Currency: "dollars"}}))
	require.ErrorContains(t, err, "spend_windows[0].currency invalid")
}

// Wasted-spend grace is orthogonal to the capped quantity, so it rides on
// either kind.
func TestNormalizeBillingPolicy_BadSpendWindowsOnEitherKind(t *testing.T) {
	grace := []models.BudgetWindowPolicy{{Key: "burst", WindowSeconds: 900, Limit: 1_000_000}}
	for _, kind := range []models.BillingPolicyKind{models.BillingPolicyOutstandingCap, models.BillingPolicyWindowSpendCap} {
		p := models.BillingPolicy{Kind: kind, BadSpendWindows: grace}
		if kind == models.BillingPolicyWindowSpendCap {
			p.SpendWindows = []models.BudgetWindowPolicy{{Key: "m", WindowSeconds: 60, Limit: 1}}
		}
		out, err := NormalizeBillingPolicy("p", p)
		require.NoError(t, err, kind)
		require.Len(t, out.BadSpendWindows, 1, kind)
	}
}

func TestNormalizeBillingPolicyName(t *testing.T) {
	name, err := NormalizeBillingPolicyName("  api_line-2.0 ")
	require.NoError(t, err)
	require.Equal(t, "api_line-2.0", name)

	_, err = NormalizeBillingPolicyName("   ")
	require.ErrorContains(t, err, "name is required")

	_, err = NormalizeBillingPolicyName("api line")
	require.ErrorContains(t, err, "may use only letters")

	_, err = NormalizeBillingPolicyName(string(make([]byte, MaxBillingPolicyNameLength+1)))
	require.Error(t, err)
}

func TestNormalizeBillingPolicyBinding_Rungs(t *testing.T) {
	name, tier, rung, err := NormalizeBillingPolicyBinding("p", "", false)
	require.NoError(t, err)
	require.Equal(t, "p", name)
	require.Empty(t, tier)
	require.Equal(t, BindingRungDefault, rung)

	_, tier, rung, err = NormalizeBillingPolicyBinding("p", " gold ", false)
	require.NoError(t, err)
	require.Equal(t, "gold", tier)
	require.Equal(t, BindingRungTier, rung)

	_, tier, rung, err = NormalizeBillingPolicyBinding("p", "", true)
	require.NoError(t, err)
	require.Empty(t, tier)
	require.Equal(t, BindingRungCustomer, rung)

	// Most-specific-wins cannot rank a binding that is both.
	_, _, _, err = NormalizeBillingPolicyBinding("p", "gold", true)
	require.ErrorContains(t, err, "a customer OR a tier, not both")
}
