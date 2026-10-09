//go:build e2e && integration

package subscriptions_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
)

// A declared merchant's settings are the configuration API's document: New
// applies them through the same validation, the configuration reads them back
// in the same shape, and a declaration the API would refuse refuses boot
// before any merchant exists.
func TestDeclaredMerchantSettings(t *testing.T) {
	t.Parallel()
	w := prepareWorld(t, 12)
	ctx := t.Context()
	boot := func(slug string, settings billing.MerchantSettings) (*openrails.Client, error) {
		return openrails.New(ctx, openrails.Config{
			Database: openrails.DatabaseConfig{Schema: w.schema, RiverSchema: w.schema}, TestMode: openrails.Sandbox, ProviderWriteMode: openrails.ProviderWritesReadOnly,
			Merchant: openrails.MerchantDeclaration{Slug: slug, DisplayName: "Declared", Settings: settings},
		}, openrails.Deps{FXTransport: testFX.Transport(), Postgres: w.pool, Clock: w.clock})
	}
	merchants := func() int {
		var n int
		require.NoError(t, w.pool.QueryRow(ctx, w.sql(`SELECT count(*) FROM billing.merchants`)).Scan(&n))
		return n
	}

	_, err := boot("refused", billing.MerchantSettings{BillingPolicyBindings: []billing.BillingPolicyBinding{{PolicyName: "missing"}}})
	require.ErrorContains(t, err, "undeclared policy")
	_, err = boot("refused", billing.MerchantSettings{Profile: &billing.MerchantProfile{LogoURL: "ftp://cdn.example/logo.png"}})
	require.ErrorContains(t, err, "profile.logo_url")
	require.Zero(t, merchants(), "a refused declaration creates no merchant")

	grace, floor, threshold := 7, int64(5_000_000), int64(50_000_000)
	routing := []billing.CheckoutRoutingRule{{Prefer: []string{"mobius"}}}
	declared := billing.MerchantSettings{
		Profile:                    &billing.MerchantProfile{FromEmail: "billing@declared.example"},
		InvoiceCollectionThreshold: &threshold, InvoiceBillingBoundary: "calendar_month",
		ArrearsGraceDays: &grace, ArrearsDelinquencyFloor: &floor,
		CheckoutRouting:                   &routing,
		BillingPolicies:                   []billing.BillingPolicy{{Name: "line", Kind: "outstanding_cap", OutstandingCapAmount: 200_000_000}},
		BillingPolicyBindings:             []billing.BillingPolicyBinding{{PolicyName: "line"}},
		DelegatedInvokerWastedSpendLimits: []billing.BudgetWindow{{Key: "burst", WindowSeconds: 900, Limit: 5_000_000, Currency: "USD"}},
	}
	slug := "declared-" + uuid.NewString()[:8]
	client, err := boot(slug, declared)
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close(context.Background()) })

	got, err := client.GetMerchantConfiguration(ctx)
	require.NoError(t, err)
	require.Equal(t, "Declared", got.DisplayName)
	s := got.Settings
	require.Equal(t, "billing@declared.example", s.Profile.FromEmail)
	require.Equal(t, "Declared", s.Profile.DisplayName, "the profile name defaults to the merchant's")
	require.Equal(t, threshold, *s.InvoiceCollectionThreshold)
	require.Equal(t, "calendar_month", s.InvoiceBillingBoundary)
	require.Equal(t, grace, *s.ArrearsGraceDays)
	require.Equal(t, floor, *s.ArrearsDelinquencyFloor)
	require.Equal(t, routing, *s.CheckoutRouting)
	require.Len(t, s.BillingPolicies, 1)
	require.Equal(t, int64(200_000_000), s.BillingPolicies[0].OutstandingCapAmount)
	require.Equal(t, []billing.BillingPolicyBinding{{PolicyName: "line"}}, s.BillingPolicyBindings)
	require.Equal(t, declared.DelegatedInvokerWastedSpendLimits, s.DelegatedInvokerWastedSpendLimits)

	// The configuration API changes the same document; a restart with the
	// same declaration does not reassert it over that change.
	grace = 3
	_, err = client.UpdateMerchantConfiguration(ctx, billing.UpdateMerchantConfigurationParams{
		IdempotencyKey: uuid.NewString(), ExpectedRevision: &got.Revision, Settings: &billing.MerchantSettings{ArrearsGraceDays: &grace},
	})
	require.NoError(t, err)
	require.NoError(t, client.Close(ctx))
	grace = 7
	again, err := boot(slug, declared)
	require.NoError(t, err)
	t.Cleanup(func() { _ = again.Close(context.Background()) })
	after, err := again.GetMerchantConfiguration(ctx)
	require.NoError(t, err)
	require.Equal(t, 3, *after.Settings.ArrearsGraceDays)
}
