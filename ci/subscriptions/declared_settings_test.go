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
// validated the same way; settings the API would refuse refuse the merchant
// document alone, named by the configuration probe, while the engine boots.
// Read from a file the configuration is the file's: edits are refused and a
// changed file is what the next start serves.
func TestDeclaredMerchantSettings(t *testing.T) {
	t.Parallel()
	w := prepareWorld(t, 12)
	ctx := t.Context()
	boot := func(slug string, settings billing.MerchantSettings, vault *vaulttest.Vault) (*openrails.Engine, *openrails.Client, error) {
		cfg := openrails.Config{
			Database: openrails.DatabaseConfig{Schema: w.schema, RiverSchema: w.schema}, ProviderWriteMode: openrails.ProviderWritesReadOnly,
			Merchants: map[string]openrails.MerchantDeclaration{slug: {DisplayName: "Declared", Settings: settings}},
		}
		if vault != nil {
			cfg.Vault = vault.Config()
		}
		bill, err := openrails.New(ctx, cfg, openrails.Deps{FXTransport: testFX.Transport(), Postgres: w.pool, Clock: w.clock})
		if err != nil {
			return nil, nil, err
		}
		shop, err := bill.MerchantBySlug(slug)
		return bill, shop, err
	}
	merchants := func() int {
		var n int
		require.NoError(t, w.pool.QueryRow(ctx, w.sql(`SELECT count(*) FROM billing.merchants`)).Scan(&n))
		return n
	}

	refused := func(settings billing.MerchantSettings, want string) {
		t.Helper()
		bill, _, err := boot("refused", settings, nil)
		require.NoError(t, err, "refused settings never fail boot")
		defer func() { _ = bill.Close(context.Background()) }()
		var probe error
		for _, p := range bill.Probes() {
			if p.Name == "openrails_configuration" {
				probe = p.Check(ctx)
			}
		}
		require.ErrorContains(t, probe, "merchants.refused.merchant: settings: ")
		require.ErrorContains(t, probe, want)
	}
	refused(billing.MerchantSettings{BillingPolicyBindings: []billing.BillingPolicyBinding{{PolicyName: "missing"}}}, "undeclared policy")
	refused(billing.MerchantSettings{Profile: &billing.MerchantProfile{LogoURL: "ftp://cdn.example/logo.png"}}, "profile.logo_url")
	countryRouting := []billing.PSPRoutingRule{{Match: billing.PSPRoutingMatch{Country: "US"}, Prefer: []string{"nmi"}}}
	refused(billing.MerchantSettings{PSPRouting: &countryRouting}, "match.country is not supported")
	require.Equal(t, 1, merchants(), "the merchant exists; only its document is refused")

	grace, floor, threshold := 7, int64(5_000_000), int64(50_000_000)
	routing := []billing.PSPRoutingRule{{Prefer: []string{"mobius"}}}
	declared := billing.MerchantSettings{
		Profile:             &billing.MerchantProfile{FromEmail: "billing@declared.example"},
		CollectionThreshold: &threshold, BillingPeriodBoundary: "calendar_month",
		ArrearsGraceDays: &grace, ArrearsDelinquencyFloor: &floor,
		PSPRouting:                        &routing,
		BillingPolicies:                   []billing.BillingPolicy{{Name: "line", Kind: "outstanding_cap", OutstandingCapAmount: 200_000_000}},
		BillingPolicyBindings:             []billing.BillingPolicyBinding{{PolicyName: "line"}},
		DelegatedInvokerWastedSpendLimits: []billing.BudgetWindow{{Key: "burst", WindowSeconds: 900, Limit: 5_000_000, Currency: "USD"}},
	}
	slug := "declared-" + uuid.NewString()[:8]
	bill, client, err := boot(slug, declared, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = bill.Close(context.Background()) })

	got, err := client.GetMerchantConfiguration(ctx)
	require.NoError(t, err)
	require.Equal(t, "Declared", got.DisplayName)
	s := got.Settings
	require.Equal(t, "billing@declared.example", s.Profile.FromEmail)
	require.Equal(t, threshold, *s.CollectionThreshold)
	require.Equal(t, "calendar_month", s.BillingPeriodBoundary)
	require.Equal(t, grace, *s.ArrearsGraceDays)
	require.Equal(t, floor, *s.ArrearsDelinquencyFloor)
	require.Equal(t, routing, *s.PSPRouting)
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
	require.NoError(t, bill.Close(ctx))
	restarted, again, err := boot(slug, declared, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = restarted.Close(context.Background()) })
	after, err := again.GetMerchantConfiguration(ctx)
	require.NoError(t, err)
	require.Equal(t, 3, *after.Settings.ArrearsGraceDays, "the declaration carried the change")
}

// With Vault holding the configuration what a file declares beside the slug
// is ignored and named at startup; the slug alone boots an empty
// configuration staff fill in, at the revision they read, and an edit naming
// a revision the configuration moved past is refused.
func TestVaultMerchantSettings(t *testing.T) {
	t.Parallel()
	vault := vaulttest.New(t)
	w := prepareWorld(t, 12)
	ctx := t.Context()
	boot := func(declared openrails.MerchantDeclaration) (*openrails.Engine, *openrails.Client, error) {
		bill, err := openrails.New(ctx, openrails.Config{
			Database: openrails.DatabaseConfig{Schema: w.schema, RiverSchema: w.schema}, ProviderWriteMode: openrails.ProviderWritesReadOnly,
			Merchants: map[string]openrails.MerchantDeclaration{declared.Slug: declared}, Vault: vault.Config(),
		}, openrails.Deps{Postgres: w.pool, Clock: w.clock})
		if err != nil {
			return nil, nil, err
		}
		t.Cleanup(func() { _ = bill.Close(context.Background()) })
		shop, err := bill.MerchantBySlug(declared.Slug)
		return bill, shop, err
	}
	slug := "vaulted-" + uuid.NewString()[:8]
	grace := 7
	declaring, _, err := boot(openrails.MerchantDeclaration{Slug: slug, DisplayName: "Declared", Settings: billing.MerchantSettings{ArrearsGraceDays: &grace}})
	require.NoError(t, err)
	for _, p := range declaring.Probes() {
		if p.Name == "openrails_configuration" {
			require.ErrorContains(t, p.Check(ctx), "merchants."+slug+`.declaration: merchant "`+slug+`" declares display_name, settings`)
		}
	}
	require.NoError(t, declaring.Close(ctx))

	bill, client, err := boot(openrails.MerchantDeclaration{Slug: slug})
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

	require.NoError(t, bill.Close(ctx))
	_, again, err := boot(openrails.MerchantDeclaration{Slug: slug})
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
	w.settings = billing.MerchantSettings{CollectionThreshold: &threshold}
	w.start()
	ctx := t.Context()
	got, err := w.client[remote].GetMerchantConfiguration(ctx)
	require.NoError(t, err)
	require.Equal(t, w.slug, got.DisplayName)
	require.Equal(t, threshold, *got.Settings.CollectionThreshold)
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
