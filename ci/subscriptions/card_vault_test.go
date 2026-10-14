//go:build e2e && integration

package subscriptions_test

import (
	"context"
	"io"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/engine"
	"github.com/open-rails/openrails/internal/merchantarchive"
	"github.com/open-rails/openrails/internal/vaulttest"
)

const platformTenant = "bt-tenant-e2e"

func withPlatformVault(c *config.Config) {
	c.PlatformVault = &config.PlatformVaultConfig{Kind: "basis_theory", TenantID: platformTenant}
}

// platformIdentities are the merchant's custodian rows keyed platform.
func (w *world) platformIdentities() []struct{ id, kind, environment, account string } {
	w.t.Helper()
	rows, err := w.pool.Query(w.t.Context(), w.sql(`SELECT id::text, kind, environment, account_id FROM billing.custodians WHERE merchant_id = $1 AND key = 'platform'`),
		w.client[embedded].MerchantID().UUID())
	require.NoError(w.t, err)
	defer rows.Close()
	var out []struct{ id, kind, environment, account string }
	for rows.Next() {
		var r struct{ id, kind, environment, account string }
		require.NoError(w.t, rows.Scan(&r.id, &r.kind, &r.environment, &r.account))
		out = append(out, r)
	}
	require.NoError(w.t, rows.Err())
	return out
}

func setVault(vault string, charge ...string) billing.UpdateMerchantConfigurationParams {
	return billing.UpdateMerchantConfigurationParams{MerchantSettings: &billing.MerchantSettings{CardVault: vault, ChargePSPs: charge}}
}

// A merchant saves new cards in the platform vault only where every PSP new
// card charges go to can charge them; choosing it records the merchant's
// identity in the vault once, which platform cards are held by.
func TestPlatformCardVault(t *testing.T) {
	t.Parallel()
	w := newVaultWorld(t, withPlatformVault)
	ctx := t.Context()
	client := w.client[remote]
	mid := w.client[embedded].MerchantID().UUID()

	got, err := client.GetMerchantConfiguration(ctx)
	require.NoError(t, err)
	require.Empty(t, got.MerchantSettings.CardVault, "absent: each PSP keeps the cards it takes")
	require.True(t, got.PlatformVault.Available)

	_, err = client.UpdateMerchantConfiguration(ctx, setVault(billing.CardVaultPlatform))
	require.Equal(t, "platform_vault_unreachable", codeOf(err), "Stripe's own fields take the card")
	require.ErrorContains(t, err, "stripe")
	require.Empty(t, w.platformIdentities())

	_, err = client.UpdatePSP(ctx, w.psp["stripe"], billing.UpdatePSPParams{Archived: true, AllowLast: true})
	require.NoError(t, err)
	got, err = client.UpdateMerchantConfiguration(ctx, setVault(billing.CardVaultPlatform))
	require.NoError(t, err)
	require.Equal(t, billing.CardVaultPlatform, got.MerchantSettings.CardVault)
	identities := w.platformIdentities()
	require.Len(t, identities, 1)
	identity := identities[0]
	require.Equal(t, "platform", identity.kind)
	require.Equal(t, "test", identity.environment)
	require.Equal(t, platformTenant+"/"+mid.String(), identity.account)

	grace := 3
	_, err = client.UpdateMerchantConfiguration(ctx, billing.UpdateMerchantConfigurationParams{MerchantSettings: &billing.MerchantSettings{CardVault: billing.CardVaultPlatform, ArrearsGraceDays: &grace}})
	require.NoError(t, err)
	w.declare = func(psps map[string]openrails.PSPConfig) { delete(psps, "stripe") }
	w.restart()
	client = w.client[remote]
	require.Equal(t, identities, w.platformIdentities(), "choosing again and reloading record no second identity")

	// A PSP that would take new card charges it cannot make is refused before
	// any write; once charge_psps leaves it out, it is a PSP like any other.
	stripeTwo := billing.CreatePSPParams{Key: "stripe-two", Rail: billing.RailStripe, Environment: "test", PSPAccount: billing.PSPAccount{AccountID: "acct_e2etwo"},
		PublishableKey: "pk_test_two", Credentials: map[string]string{"secret_key": "sk_test_acct_e2etwo", "webhook_signing_secret": "whsec_two"}}
	_, err = client.CreatePSP(ctx, stripeTwo)
	require.Equal(t, "platform_vault_unreachable", codeOf(err))
	_, err = client.UpdateMerchantConfiguration(ctx, setVault(billing.CardVaultPlatform, "nmi"))
	require.NoError(t, err)
	_, err = client.CreatePSP(ctx, stripeTwo)
	require.NoError(t, err, "stripe-two takes no new card charge")
	_, err = client.UpdateMerchantConfiguration(ctx, setVault(billing.CardVaultPlatform, "nmi", "stripe-two"))
	require.Equal(t, "platform_vault_unreachable", codeOf(err))
	// Nor is a PSP document written into Vault directly whose own server
	// would take the card.
	w.editDoc("psps/nmi", func(doc map[string]any) { doc["card_entry"] = "server" })
	rt := engine.Graph(w.rt).Runtime
	served, err := rt.MerchantConfig.Get(ctx, rt.DeclaredMerchants()[0])
	require.NoError(t, err)
	require.Contains(t, served.Rejected["psps/nmi"], "platform vault")
	require.NotContains(t, served.PSPs["nmi"].Value.Settings, "card_entry")
	require.Equal(t, billing.CardVaultPlatform, served.Merchant.Value.MerchantSettings.CardVault)

	// A platform card is held by the merchant's identity; without it the
	// custody checks refuse the row.
	customer := uuid.New()
	_, err = w.pool.Exec(ctx, w.sql(`INSERT INTO billing.customers (merchant_id, id) VALUES ($1, $2)`), mid, customer)
	require.NoError(t, err)
	insertCard := func(custodianID *string) (string, error) {
		var id string
		err := w.pool.QueryRow(ctx, w.sql(`INSERT INTO billing.payment_methods (merchant_id, customer_id, rail, custodian, custodian_id, rail_method_ref, charge_via, status)
			VALUES ($1, $2, 'nmi', 'platform', $3::uuid, $4, 'pan_proxy', 'active') RETURNING id::text`), mid, customer, custodianID, "bt-token-"+uuid.NewString()).Scan(&id)
		return id, err
	}
	_, err = insertCard(nil)
	var pgErr *pgconn.PgError
	require.ErrorAs(t, err, &pgErr)
	require.Equal(t, "payment_methods_custodian_identity_check", pgErr.ConstraintName)
	card, err := insertCard(&identity.id)
	require.NoError(t, err)
	_, err = w.pool.Exec(ctx, w.sql(`INSERT INTO billing.payment_method_versions (merchant_id, customer_id, payment_method_id, source, kind, event_ref, custodian_id, rail_method_ref, effective_at)
		SELECT merchant_id, customer_id, id, 'customer_save', 'saved', 'created', custodian_id, rail_method_ref, now() FROM billing.payment_methods WHERE merchant_id = $1 AND id = $2`), mid, card)
	require.NoError(t, err, "the card version's holder is the platform identity")

	// The archive never carries a card the platform vault still holds.
	err = client.ExportBillingArchive(ctx, io.Discard)
	require.Equal(t, "billing_archive_platform_cards", codeOf(err))
	var refused *merchantarchive.Error
	require.ErrorAs(t, w.exportArchive(), &refused)
	require.Equal(t, "platform_cards", refused.Code)
	_, err = w.pool.Exec(ctx, w.sql(`UPDATE billing.payment_methods SET status = 'removed' WHERE merchant_id = $1 AND id = $2`), mid, card)
	require.NoError(t, err)
	require.NoError(t, w.exportArchive(), "a removed card no longer holds the archive")
}

// Without a platform vault for the deployment's posture, card_vault platform
// is refused: over the API, in a declaration (the merchant document alone),
// and in Vault directly.
func TestPlatformCardVaultNeedsAPlatformVault(t *testing.T) {
	t.Parallel()
	w := newVaultWorld(t)
	ctx := t.Context()
	client := w.client[remote]

	got, err := client.GetMerchantConfiguration(ctx)
	require.NoError(t, err)
	require.False(t, got.PlatformVault.Available)
	_, err = client.UpdateMerchantConfiguration(ctx, setVault(billing.CardVaultPlatform))
	require.Equal(t, "platform_vault_unavailable", codeOf(err))
	got, err = client.UpdateMerchantConfiguration(ctx, setVault("nmi"))
	require.NoError(t, err)
	require.Equal(t, "nmi", got.MerchantSettings.CardVault)

	w.editDoc("merchant", func(doc map[string]any) { doc["card_vault"] = "platform" })
	rt := engine.Graph(w.rt).Runtime
	served, err := rt.MerchantConfig.Get(ctx, rt.DeclaredMerchants()[0])
	require.NoError(t, err)
	require.Contains(t, served.Rejected["merchant"], "platform vault")
	require.Equal(t, "nmi", served.Merchant.Value.MerchantSettings.CardVault, "the previous document keeps serving")
	require.Empty(t, w.platformIdentities())

	// Boot serves without what it refuses, and names it.
	refused := func(cfg openrails.Config) error {
		cfg.Database = openrails.DatabaseConfig{Schema: w.schema, RiverSchema: w.schema}
		cfg.ProviderWriteMode = openrails.ProviderWritesReadOnly
		bill, err := openrails.New(ctx, cfg, openrails.Deps{FXTransport: testFX.Transport(), Postgres: w.pool, Clock: w.clock})
		require.NoError(t, err, "a card vault the deployment cannot serve never fails boot")
		defer func() { _ = bill.Close(context.Background()) }()
		for _, p := range bill.Probes() {
			if p.Name == "openrails_configuration" {
				return p.Check(ctx)
			}
		}
		return nil
	}
	declared := func(vault string) map[string]openrails.MerchantDeclaration {
		return map[string]openrails.MerchantDeclaration{"vault-" + uuid.NewString()[:8]: {DisplayName: "Vault", MerchantSettings: billing.MerchantSettings{CardVault: vault}}}
	}
	require.ErrorContains(t, refused(openrails.Config{Merchants: declared(billing.CardVaultPlatform)}), "no platform vault")
	require.ErrorContains(t, refused(openrails.Config{Merchants: declared("nowhere")}), "card_vault names no live card PSP")
	require.ErrorContains(t, refused(openrails.Config{PlatformVault: &openrails.PlatformVaultConfig{Kind: "basis_theory", TenantID: platformTenant}}), "vault.kv_mount")
	vault := vaulttest.New(t)
	require.ErrorContains(t, refused(openrails.Config{Vault: vault.Config(), PlatformVault: &openrails.PlatformVaultConfig{Kind: "hyperswitch", TenantID: platformTenant}}), "platform_vault.kind")
}

// A card in a PSP's own vault charges only through that PSP's account: the
// merchant's charge PSPs must agree with its card vault, or the change is
// refused for this merchant alone, over the API and in Vault directly. The
// card vault's PSP alone then takes new card charges.
func TestCardVaultRefusesChargePSPsThatCannotChargeIt(t *testing.T) {
	t.Parallel()
	w := newVaultWorld(t)
	ctx := t.Context()
	client := w.client[remote]

	for _, refused := range []struct {
		settings billing.UpdateMerchantConfigurationParams
		code     string
	}{
		{setVault("nmi", "stripe"), "charge_psp_incompatible"},
		{setVault("nowhere"), "card_vault_unknown"},
		{setVault("nmi", "nowhere"), "charge_psp_unknown"},
		{setVault("ccbill"), "card_vault_unknown"},
	} {
		_, err := client.UpdateMerchantConfiguration(ctx, refused.settings)
		require.Equal(t, refused.code, codeOf(err), "%+v", refused.settings.MerchantSettings)
	}
	got, err := client.UpdateMerchantConfiguration(ctx, setVault("nmi", "nmi"))
	require.NoError(t, err)
	require.Equal(t, []string{"nmi"}, got.MerchantSettings.ChargePSPs)

	price := w.membership("content:vault", 9_990_000)
	options := w.options(price.ID)
	require.Contains(t, options, "nmi")
	require.NotContains(t, options, "stripe", "only the charge PSPs take a new card")

	w.editDoc("merchant", func(doc map[string]any) { doc["card_vault"], doc["charge_psps"] = "stripe", []any{"nmi"} })
	rt := engine.Graph(w.rt).Runtime
	served, err := rt.MerchantConfig.Get(ctx, rt.DeclaredMerchants()[0])
	require.NoError(t, err)
	require.Contains(t, served.Rejected["merchant"], "cannot charge cards vaulted at stripe")
	require.Equal(t, "nmi", served.Merchant.Value.MerchantSettings.CardVault, "the previous document keeps serving")
	require.Contains(t, w.options(price.ID), "nmi", "the merchant still sells")
}
