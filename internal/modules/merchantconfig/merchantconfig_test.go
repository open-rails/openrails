package merchantconfig

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db/models"
)

func window(key string, seconds, limit int64) models.BudgetWindowPolicy {
	return models.BudgetWindowPolicy{Key: key, WindowSeconds: seconds, Limit: limit}
}

// A policy either normalizes to exactly what will be enforced or is refused; a
// field no enforcer reads for its kind is a cap that looks real and is not.
func TestNormalizeBillingPolicyRefusesUnenforceableShapes(t *testing.T) {
	neg := -1
	one := []models.BudgetWindowPolicy{window("m", 60, 1)}
	spend := func(ws ...models.BudgetWindowPolicy) models.BillingPolicy {
		return models.BillingPolicy{Kind: models.BillingPolicyWindowSpendCap, SpendWindows: ws}
	}
	for _, tc := range []struct {
		p    models.BillingPolicy
		want []string
	}{
		{models.BillingPolicy{}, []string{"kind is required"}},
		{models.BillingPolicy{Kind: "spend_cap"}, []string{`unknown kind "spend_cap"`}},
		{models.BillingPolicy{Kind: models.BillingPolicyOutstandingCap, SpendWindows: one}, []string{"spend_windows belong to kind window_spend_cap"}},
		{models.BillingPolicy{Kind: models.BillingPolicyWindowSpendCap, OutstandingCapAmount: 5, SpendWindows: one}, []string{"outstanding_cap_amount belongs to kind outstanding_cap"}},
		{models.BillingPolicy{Kind: models.BillingPolicyOutstandingCap, AccrualRateCapPerHour: 1}, []string{"belong to kind accrual_rate_cap"}},
		{models.BillingPolicy{Kind: models.BillingPolicyAccrualRateCap}, []string{"requires a positive accrual_rate_cap_per_hour", "billing policy p"}},
		{models.BillingPolicy{Kind: models.BillingPolicyAccrualRateCap, AccrualRateCapPerHour: 1, AccrualRateWindowSeconds: 5}, []string{"measures noise, not a rate"}},
		{spend(), []string{"requires at least one spend_windows entry"}},
		{spend(window("", 60, 1)), []string{"spend_windows[0].key is required"}},
		{spend(window("m", 0, 1)), []string{"window_seconds must be positive"}},
		{spend(window("m", 60, -1)), []string{"limit must be non-negative"}},
		// A repeated key collapses into one metered bucket, enforcing whichever landed last.
		{spend(window("m", 60, 1), window("m", 3600, 2)), []string{`repeats key "m"`}},
		{spend(models.BudgetWindowPolicy{Key: "m", WindowSeconds: 60, Limit: 1, Currency: "dollars"}), []string{"spend_windows[0].currency invalid"}},
		{models.BillingPolicy{Kind: models.BillingPolicyOutstandingCap, DelinquencyGraceDays: &neg}, []string{"delinquency_grace_days must be non-negative"}},
		// Statement periods must tile a payer's lifetime, and rebinding is a live lever.
		{models.BillingPolicy{Kind: models.BillingPolicyOutstandingCap, CollectionCycleBoundary: "calendar_month"},
			[]string{"cannot be per-policy", "tile its lifetime", "invoice.billing_period_boundary"}},
	} {
		_, err := NormalizeBillingPolicy("p", tc.p)
		for _, w := range tc.want {
			require.ErrorContains(t, err, w)
		}
	}
}

func TestNormalizeBillingPolicyAcceptsEachKind(t *testing.T) {
	api, err := NormalizeBillingPolicy("api_line", models.BillingPolicy{Kind: models.BillingPolicyOutstandingCap, OutstandingCapAmount: 200_000_000})
	require.NoError(t, err)
	require.Equal(t, int64(200_000_000), api.OutstandingCapAmount)

	cloud, err := NormalizeBillingPolicy("cloud_window", models.BillingPolicy{
		Kind:         models.BillingPolicyWindowSpendCap,
		SpendWindows: []models.BudgetWindowPolicy{{Key: "monthly", WindowSeconds: 30 * 24 * 3600, Limit: 2_000_000_000, Currency: "usd"}},
	})
	require.NoError(t, err)
	require.Len(t, cloud.SpendWindows, 1)
	require.Equal(t, "USD", cloud.SpendWindows[0].Currency)

	quota, err := NormalizeBillingPolicy("quota", models.BillingPolicy{Kind: models.BillingPolicyAccrualRateCap, AccrualRateCapPerHour: 2_000_000, AccrualRateWindowSeconds: 900})
	require.NoError(t, err)
	require.EqualValues(t, 2_000_000, quota.AccrualRateCapPerHour)
	require.EqualValues(t, 900, quota.AccrualRateWindowSeconds)

	quota, err = NormalizeBillingPolicy("quota", models.BillingPolicy{Kind: models.BillingPolicyAccrualRateCap, AccrualRateCapPerHour: 1})
	require.NoError(t, err)
	require.EqualValues(t, models.DefaultAccrualRateWindowSeconds, quota.RateWindowSeconds(), "undeclared window means one hour")

	// Bad-spend grace, collection and delinquency are orthogonal to the capped quantity.
	threshold, grace, floor := int64(50_000_000), 7, int64(1_000_000)
	for _, kind := range []models.BillingPolicyKind{models.BillingPolicyOutstandingCap, models.BillingPolicyWindowSpendCap, models.BillingPolicyAccrualRateCap} {
		p := models.BillingPolicy{
			Kind:                      kind,
			BadSpendWindows:           []models.BudgetWindowPolicy{window("burst", 900, 1_000_000)},
			CollectionThresholdAmount: &threshold,
			DelinquencyGraceDays:      &grace,
			DelinquencyAmountFloor:    &floor,
		}
		switch kind {
		case models.BillingPolicyWindowSpendCap:
			p.SpendWindows = []models.BudgetWindowPolicy{window("m", 60, 1)}
		case models.BillingPolicyAccrualRateCap:
			p.AccrualRateCapPerHour = 1
		}
		out, err := NormalizeBillingPolicy("p", p)
		require.NoError(t, err, kind)
		require.Len(t, out.BadSpendWindows, 1, kind)
		require.NotNil(t, out.CollectionThresholdAmount, kind)
		require.NotNil(t, out.DelinquencyGraceDays, kind)
		require.NotNil(t, out.DelinquencyAmountFloor, kind)
	}
}

func TestNormalizeBillingPolicyNameAndBinding(t *testing.T) {
	name, err := NormalizeBillingPolicyName("  api_line-2.0 ")
	require.NoError(t, err)
	require.Equal(t, "api_line-2.0", name)
	for in, want := range map[string]string{
		"   ":      "name is required",
		"api line": "may use only letters",
		strings.Repeat("a", MaxBillingPolicyNameLength+1): "exceeds",
	} {
		_, err := NormalizeBillingPolicyName(in)
		require.ErrorContains(t, err, want)
	}

	for _, tc := range []struct {
		tier     string
		customer bool
		wantTier string
		rung     BillingPolicyBindingRung
	}{
		{"", false, "", BindingRungDefault},
		{" gold ", false, "gold", BindingRungTier},
		{"", true, "", BindingRungCustomer},
	} {
		name, tier, rung, err := NormalizeBillingPolicyBinding("p", tc.tier, tc.customer)
		require.NoError(t, err)
		require.Equal(t, "p", name)
		require.Equal(t, tc.wantTier, tier)
		require.Equal(t, tc.rung, rung)
	}
	// Most-specific-wins cannot rank a binding that is both.
	_, _, _, err = NormalizeBillingPolicyBinding("p", "gold", true)
	require.ErrorContains(t, err, "a customer OR a tier, not both")
}

func TestNormalizeCheckoutRouting(t *testing.T) {
	out, err := NormalizeCheckoutRouting(nil)
	require.NoError(t, err)
	require.Nil(t, out, "no rules is no policy")

	out, err = NormalizeCheckoutRouting([]models.CheckoutRoutingRule{
		{Match: models.CheckoutRoutingMatch{Currency: " usd ", Country: "us", Mode: " Subscription "}, Prefer: []string{" Mobius ", "CCBill"}},
		{Prefer: []string{"stripe"}},
	})
	require.NoError(t, err)
	require.Equal(t, models.CheckoutRoutingMatch{Currency: "USD", Country: "US", Mode: "subscription"}, out[0].Match)
	require.Equal(t, []string{"mobius", "ccbill"}, out[0].Prefer)
	require.True(t, out[1].Match.IsCatchAll())

	match := func(m models.CheckoutRoutingMatch) []models.CheckoutRoutingRule {
		return []models.CheckoutRoutingRule{{Match: m, Prefer: []string{"mobius"}}}
	}
	for want, rules := range map[string][]models.CheckoutRoutingRule{
		"must name at least one PSP": {{Prefer: nil}},
		`repeats "mobius"`:           {{Prefer: []string{"mobius", "mobius"}}},
		"unreachable":                {{Prefer: []string{"mobius"}}, {Match: models.CheckoutRoutingMatch{Currency: "eur"}, Prefer: []string{"ccbill"}}},
		"ISO-4217":                   match(models.CheckoutRoutingMatch{Currency: "dollars"}),
		"ISO-3166-1":                 match(models.CheckoutRoutingMatch{Country: "USA"}),
		"one_off or subscription":    match(models.CheckoutRoutingMatch{Mode: "recurring"}),
	} {
		_, err := NormalizeCheckoutRouting(rules)
		require.ErrorContains(t, err, want)
	}
}

func TestNormalizeProviderRefundAccess(t *testing.T) {
	for _, v := range []string{"", "revoke_on_full", "revoke_on_any", "keep"} {
		got, err := NormalizeProviderRefundAccess(v)
		require.NoError(t, err)
		require.Equal(t, v, got)
	}
	_, err := NormalizeProviderRefundAccess("revoke")
	require.ErrorContains(t, err, "provider_refund_access must be")
}
