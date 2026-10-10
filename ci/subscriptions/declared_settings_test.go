//go:build e2e && integration

package subscriptions_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/vaulttest"
)

func codeOf(err error) string {
	var refused *billing.StatusError
	if errors.As(err, &refused) {
		return refused.Code
	}
	return ""
}

// A declared merchant's settings are the configuration API's document,
// validated the same way; a declaration the API would refuse refuses boot
// before any merchant exists. Read from a file the configuration is the
// file's: edits are refused and a changed file is what the next start serves.
func TestDeclaredMerchantSettings(t *testing.T) {
	t.Parallel()
	w := prepareWorld(t, 12)
	ctx := t.Context()
	boot := func(slug string, settings billing.MerchantSettings, vault *vaulttest.Vault) (*openrails.Client, error) {
		cfg := openrails.Config{
			Database: openrails.DatabaseConfig{Schema: w.schema, RiverSchema: w.schema}, TestMode: openrails.Sandbox, ProviderWriteMode: openrails.ProviderWritesReadOnly,
			Merchant: openrails.MerchantDeclaration{Slug: slug, DisplayName: "Declared", Settings: settings},
		}
		if vault != nil {
			cfg.Vault = vault.Config()
		}
		return openrails.New(ctx, cfg, openrails.Deps{FXTransport: testFX.Transport(), Postgres: w.pool, Clock: w.clock})
	}
	merchants := func() int {
		var n int
		require.NoError(t, w.pool.QueryRow(ctx, w.sql(`SELECT count(*) FROM billing.merchants`)).Scan(&n))
		return n
	}

	_, err := boot("refused", billing.MerchantSettings{BillingPolicyBindings: []billing.BillingPolicyBinding{{PolicyName: "missing"}}}, nil)
	require.ErrorContains(t, err, "undeclared policy")
	_, err = boot("refused", billing.MerchantSettings{Profile: &billing.MerchantProfile{LogoURL: "ftp://cdn.example/logo.png"}}, nil)
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
	client, err := boot(slug, declared, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close(context.Background()) })

	got, err := client.GetMerchantConfiguration(ctx)
	require.NoError(t, err)
	require.Equal(t, "Declared", got.DisplayName)
	s := got.Settings
	require.Equal(t, "billing@declared.example", s.Profile.FromEmail)
	require.Equal(t, threshold, *s.InvoiceCollectionThreshold)
	require.Equal(t, "calendar_month", s.InvoiceBillingBoundary)
	require.Equal(t, grace, *s.ArrearsGraceDays)
	require.Equal(t, floor, *s.ArrearsDelinquencyFloor)
	require.Equal(t, routing, *s.CheckoutRouting)
	require.Len(t, s.BillingPolicies, 1)
	require.Equal(t, int64(200_000_000), s.BillingPolicies[0].OutstandingCapAmount)
	require.Equal(t, []billing.BillingPolicyBinding{{PolicyName: "line"}}, s.BillingPolicyBindings)
	require.Equal(t, declared.DelegatedInvokerWastedSpendLimits, s.DelegatedInvokerWastedSpendLimits)

	// The file is the configuration: its edit route is not mounted, and the
	// next start serves what the file says.
	grace = 3
	_, err = client.UpdateMerchantConfiguration(ctx, billing.UpdateMerchantConfigurationParams{
		ExpectedRevision: &got.Revision, Settings: &billing.MerchantSettings{ArrearsGraceDays: &grace},
	})
	require.Equal(t, "method_not_allowed", codeOf(err), "%v", err)
	require.NoError(t, client.Close(ctx))
	again, err := boot(slug, declared, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = again.Close(context.Background()) })
	after, err := again.GetMerchantConfiguration(ctx)
	require.NoError(t, err)
	require.Equal(t, 3, *after.Settings.ArrearsGraceDays, "the declaration carried the change")
}

// With Vault holding the configuration a file declaring any of it is refused
// at startup, naming what it declares; the slug alone boots an empty
// configuration staff fill in, at the revision they read, and an edit naming
// a revision the configuration moved past is refused.
func TestVaultMerchantSettings(t *testing.T) {
	t.Parallel()
	vault := vaulttest.New(t)
	w := prepareWorld(t, 12)
	ctx := t.Context()
	boot := func(declared openrails.MerchantDeclaration) (*openrails.Client, error) {
		client, err := openrails.New(ctx, openrails.Config{
			Database: openrails.DatabaseConfig{Schema: w.schema, RiverSchema: w.schema}, TestMode: openrails.Sandbox, ProviderWriteMode: openrails.ProviderWritesReadOnly,
			Merchant: declared, Vault: vault.Config(),
		}, openrails.Deps{Postgres: w.pool, Clock: w.clock})
		if err == nil {
			t.Cleanup(func() { _ = client.Close(context.Background()) })
		}
		return client, err
	}
	slug := "vaulted-" + uuid.NewString()[:8]
	grace := 7
	_, err := boot(openrails.MerchantDeclaration{Slug: slug, DisplayName: "Declared", Settings: billing.MerchantSettings{ArrearsGraceDays: &grace}})
	require.ErrorContains(t, err, `merchant "`+slug+`" declares display_name, settings`)
	var merchants int
	require.NoError(t, w.pool.QueryRow(ctx, w.sql(`SELECT count(*) FROM billing.merchants`)).Scan(&merchants))
	require.Zero(t, merchants, "a refused file creates no merchant")

	client, err := boot(openrails.MerchantDeclaration{Slug: slug})
	require.NoError(t, err)
	got, err := client.GetMerchantConfiguration(ctx)
	require.NoError(t, err)
	require.Zero(t, got.Revision, "Vault holds nothing for the merchant yet")
	require.Nil(t, got.Settings.ArrearsGraceDays)

	name := "Vaulted Shop"
	updated, err := client.UpdateMerchantConfiguration(ctx, billing.UpdateMerchantConfigurationParams{
		ExpectedRevision: &got.Revision, Settings: &billing.MerchantSettings{ArrearsGraceDays: &grace}, DisplayName: &name,
	})
	require.NoError(t, err)
	require.Positive(t, updated.Revision)
	require.Equal(t, 7, *updated.Settings.ArrearsGraceDays)
	require.Equal(t, "Vaulted Shop", updated.DisplayName)

	stale := 14
	_, err = client.UpdateMerchantConfiguration(ctx, billing.UpdateMerchantConfigurationParams{
		ExpectedRevision: &got.Revision, Settings: &billing.MerchantSettings{ArrearsGraceDays: &stale},
	})
	require.Equal(t, "revision_mismatch", codeOf(err), "%v", err)
	require.ErrorIs(t, err, billing.ErrConflict)

	require.NoError(t, client.Close(ctx))
	again, err := boot(openrails.MerchantDeclaration{Slug: slug})
	require.NoError(t, err)
	after, err := again.GetMerchantConfiguration(ctx)
	require.NoError(t, err)
	require.Equal(t, updated.Revision, after.Revision, "Vault holds the edit across a restart")
	require.Equal(t, 7, *after.Settings.ArrearsGraceDays)
}

// A merchant whose documents an operator wrote straight into Vault, at the
// documented paths and shapes, serves from them: its name, settings and
// PSPs, and a purchase through one.
func TestVaultDocumentsServe(t *testing.T) {
	t.Parallel()
	threshold := int64(25_000_000)
	w := prepareWorld(t, 12)
	w.vault = vaulttest.New(t)
	w.settings = billing.MerchantSettings{InvoiceCollectionThreshold: &threshold}
	w.start()
	ctx := t.Context()
	got, err := w.client[remote].GetMerchantConfiguration(ctx)
	require.NoError(t, err)
	require.Equal(t, w.slug, got.DisplayName)
	require.Equal(t, threshold, *got.Settings.InvoiceCollectionThreshold)
	psps, err := w.client[remote].ListPSPs(ctx, billing.PSPListParams{})
	require.NoError(t, err)
	var keys []string
	for _, psp := range psps.Items {
		keys = append(keys, psp.Key)
	}
	require.ElementsMatch(t, []string{"stripe", "nmi", "ccbill"}, keys)
	e := enroll(t, w, "nmi", remote)
	require.True(t, e.c.entitled(e.ent))
}
