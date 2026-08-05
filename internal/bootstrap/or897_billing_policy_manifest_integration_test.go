//go:build integration

package bootstrap

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// or#897: the mode-1 manifest and the mode-2 config API run the SAME validator
// (merchantconfig.NormalizeBillingPolicy), so a manifest that boots cannot carry
// a policy the API would have refused. These pin the manifest half.
func TestReconcileMerchantManifestAppliesBillingPolicies(t *testing.T) {
	ctx := context.Background()
	pool := newMerchantManifestTestPool(t)
	cp := newMerchantManifestControlPlane(t, pool)
	manifest := cozyArtMerchantManifest()
	mt := manifest.Merchants["cozy-art"]
	mt.BillingPolicies = map[string]BillingPolicyConfig{
		"api_line": {Kind: "outstanding_cap", OutstandingCap: 200_000_000},
		"cloud_monthly": {
			Kind:         "window_spend_cap",
			SpendWindows: []BudgetWindowConfig{{Key: "monthly", Window: "720h", Limit: 2_000_000_000}},
		},
	}
	mt.BillingPolicyBindings = []BillingPolicyBindingConfig{
		{Policy: "api_line"},
		{Policy: "cloud_monthly", Tier: "cloud"},
	}
	manifest.Merchants["cozy-art"] = mt

	require.NoError(t, ReconcileMerchantManifestData(ctx, sandboxModeReconcileConfig(), cp, manifest, MerchantManifestReconcileOptions{Insert: true}))

	var merchantID string
	require.NoError(t, pool.QueryRow(ctx, `SELECT id::text FROM openrails.merchants WHERE slug = 'cozy-art'`).Scan(&merchantID))

	// The window duration is stored in SECONDS, and the kind's limit lands in the
	// kind's own field.
	var kind string
	var cap int64
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT policy ->> 'kind', (policy ->> 'outstanding_cap_amount')::bigint
		FROM openrails.billing_policies
		WHERE merchant_id = $1::uuid AND name = 'api_line'
	`, merchantID).Scan(&kind, &cap))
	require.Equal(t, "outstanding_cap", kind)
	require.EqualValues(t, 200_000_000, cap)

	var windowSeconds, limit int64
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT (policy #>> '{spend_windows,0,window_seconds}')::bigint,
		       (policy #>> '{spend_windows,0,limit}')::bigint
		FROM openrails.billing_policies
		WHERE merchant_id = $1::uuid AND name = 'cloud_monthly'
	`, merchantID).Scan(&windowSeconds, &limit))
	require.EqualValues(t, 720*3600, windowSeconds, "720h must reach the store as seconds")
	require.EqualValues(t, 2_000_000_000, limit)

	// Both rungs are bound, the default with a NULL tier.
	var defaultPolicy, tierPolicy string
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT policy_name FROM openrails.billing_policy_bindings
		WHERE merchant_id = $1::uuid AND tier IS NULL AND customer_id IS NULL
	`, merchantID).Scan(&defaultPolicy))
	require.Equal(t, "api_line", defaultPolicy)
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT policy_name FROM openrails.billing_policy_bindings
		WHERE merchant_id = $1::uuid AND tier = 'cloud'
	`, merchantID).Scan(&tierPolicy))
	require.Equal(t, "cloud_monthly", tierPolicy)

	// Re-applying the same manifest is idempotent (declarative, not append-only).
	require.NoError(t, ReconcileMerchantManifestData(ctx, sandboxModeReconcileConfig(), cp, manifest, MerchantManifestReconcileOptions{Insert: true}))
	var policies, bindings int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM openrails.billing_policies WHERE merchant_id = $1::uuid`, merchantID).Scan(&policies))
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM openrails.billing_policy_bindings WHERE merchant_id = $1::uuid`, merchantID).Scan(&bindings))
	require.Equal(t, 2, policies)
	require.Equal(t, 2, bindings)
}

// A manifest that declares a policy the API would refuse must refuse to boot,
// with the SAME message — that is what "one validator" buys.
func TestReconcileMerchantManifestRefusesInvalidBillingPolicy(t *testing.T) {
	ctx := context.Background()
	pool := newMerchantManifestTestPool(t)
	cp := newMerchantManifestControlPlane(t, pool)

	for _, tc := range []struct {
		name   string
		policy BillingPolicyConfig
		want   string
	}{
		{"pr3 kind", BillingPolicyConfig{Kind: "accrual_rate_cap"}, "not implemented yet"},
		{"unknown kind", BillingPolicyConfig{Kind: "spend_cap"}, `unknown kind "spend_cap"`},
		{"missing kind", BillingPolicyConfig{}, "kind is required"},
		{"cross-kind limit", BillingPolicyConfig{
			Kind:         "outstanding_cap",
			SpendWindows: []BudgetWindowConfig{{Key: "m", Window: "1h", Limit: 1}},
		}, "spend_windows belong to kind window_spend_cap"},
		{"unparseable window", BillingPolicyConfig{
			Kind:         "window_spend_cap",
			SpendWindows: []BudgetWindowConfig{{Key: "m", Window: "one month", Limit: 1}},
		}, "window:"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			manifest := cozyArtMerchantManifest()
			mt := manifest.Merchants["cozy-art"]
			mt.BillingPolicies = map[string]BillingPolicyConfig{"bad": tc.policy}
			manifest.Merchants["cozy-art"] = mt
			err := ReconcileMerchantManifestData(ctx, sandboxModeReconcileConfig(), cp, manifest, MerchantManifestReconcileOptions{Insert: true})
			require.ErrorContains(t, err, tc.want)
		})
	}

	// Nothing from a refused manifest was written.
	var count int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM openrails.billing_policies`).Scan(&count))
	require.Zero(t, count, "a manifest that fails validation must install no policy at all")
}
