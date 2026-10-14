//go:build e2e && integration

package subscriptions_test

import (
	"bytes"
	"fmt"
	"testing"

	solanago "github.com/gagliardetto/solana-go"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/hosttools"
	"github.com/open-rails/openrails/internal/integrations/solana/subscriptions"
	"github.com/open-rails/openrails/internal/merchantbootstrap"
	"github.com/open-rails/openrails/internal/solanafake"
)

// On a fresh database, applying a catalog publishes the plan each recurring
// price of ours sells on and binds it: owned by the deployment's cranker,
// paying only into the merchant's treasury. Plans are immutable: re-applying
// binds the same plan, and new terms are a new price with its own plan.
// Publishing is a provider write: limited and readonly modes apply the price
// without one, warning by name, yet still bind a plan already on chain.
func TestCatalogApplyPublishesSolanaPlan(t *testing.T) {
	w := prepareWorld(t, 12)
	sol := withSolana(t, w)
	fake := sol.fake
	w.start()
	ctx := t.Context()
	product := "plans-" + uuid.NewString()[:8]
	file := func(title string, amount int64) []byte {
		return fmt.Appendf(nil, `schema_version: 1
products:
  %[1]s:
    display_name: %[2]s
    entitlements: ["%[1]s"]
    prices:
      monthly:
        currency: usd
        unit_amount: %[3]d
        billing_interval_hours: 720
        access_duration_hours: 720
      once:
        currency: usd
        unit_amount: 5000000
        billing_interval_hours: null
        access_duration_hours: 720
`, product, title, amount)
	}
	apply := func(raw []byte) (*billing.CatalogApplicationReceipt, error) {
		params, err := catalog.ParseApplicationYAML(raw)
		require.NoError(t, err)
		return w.client[embedded].ApplyCatalog(ctx, params, billing.ApplyCatalogParams{})
	}
	// cli is the operator's `openrails catalog apply` in provider write mode:
	// a one-off process over the same database and Solana node.
	cli := func(mode string, raw []byte) (*billing.CatalogApplicationReceipt, error) {
		cfg := &config.Config{ProviderWriteMode: mode, DB: &config.DBConfig{URL: w.dsn}, Database: config.DatabaseConfig{Schema: w.schema, RiverSchema: w.schema}}
		w.cfg(cfg)
		return hosttools.ApplyMerchantCatalog(ctx, hosttools.CatalogApplyOptions{Config: cfg, PGXPool: w.pool, Merchant: w.slug, Manifest: raw,
			MerchantManifest: &merchantbootstrap.BillingConfig{Merchants: map[string]openrails.MerchantDeclaration{w.slug: {DisplayName: w.slug, PSPs: w.psps}}}})
	}
	monthly := func() *billing.Price {
		price, err := priceByKey(ctx, w.client[remote], product, "monthly")
		require.NoError(t, err)
		return price
	}
	// planOf is the price's bound plan, read from the chain.
	planOf := func(price *billing.Price, amount uint64) solanago.PublicKey {
		t.Helper()
		require.NotNil(t, price.SolanaPlan, "price %s sells on a plan", price.ID)
		pda := solanago.MustPublicKeyFromBase58(*price.SolanaPlan)
		raw, ok := fake.Account(pda)
		require.True(t, ok, "price %s binds a plan on chain", price.ID)
		plan, err := subscriptions.DecodePlanAccount(raw)
		require.NoError(t, err)
		require.Equal(t, sol.cranker.PublicKey(), plan.Owner, "the deployment's cranker owns the plan")
		require.Equal(t, [4]solanago.PublicKey{sol.treasury}, plan.Destinations, "the treasury is the plan's only destination")
		require.Equal(t, solanafake.DevnetUSDCMint, plan.Mint.String(), "a devnet PSP publishes in devnet USDC")
		require.Equal(t, []uint64{amount, monthHours}, []uint64{plan.Amount, plan.PeriodHours})
		require.Equal(t, subscriptions.PlanStatusActive, plan.Status)
		return pda
	}

	first, err := apply(file("Plans", 23_000_000))
	require.NoError(t, err, "a fresh database publishes the plan")
	require.Equal(t, 2, first.PricesChanged)
	price := monthly()
	pda := planOf(price, 23_000_000)
	require.Equal(t, 1, fake.Transactions(pda), "one create_plan")
	once, err := priceByKey(ctx, w.client[embedded], product, "once")
	require.NoError(t, err)
	require.Nil(t, once.SolanaPlan, "a one-time price needs no plan")
	require.Equal(t, []string{"solana_pay"}, w.options(price.ID)["solana"].Accepts, "the bound plan sells")

	// Re-applying publishes nothing: the document replays, and another
	// document with the same terms keeps the bound plan.
	replay, err := apply(file("Plans", 23_000_000))
	require.NoError(t, err)
	require.True(t, replay.Replayed)
	renamed, err := apply(file("Plans renamed", 23_000_000))
	require.NoError(t, err)
	require.False(t, renamed.Replayed)
	require.Zero(t, renamed.PricesChanged)
	require.Equal(t, price.SolanaPlan, monthly().SolanaPlan)
	require.Equal(t, 1, fake.Transactions(pda))
	before, _ := fake.Account(pda)

	// Gated modes publish nothing: the new terms apply, sold without Solana,
	// and the warning names the price and the mode. Not recorded, a later
	// apply retries.
	for _, mode := range []string{config.ProviderWriteModeLimited, config.ProviderWriteModeReadOnly} {
		receipt, err := cli(mode, file("Plans", 29_000_000))
		require.NoError(t, err, mode)
		require.Len(t, receipt.Warnings, 1, mode)
		require.False(t, receipt.Warnings[0].Refused)
		require.Contains(t, receipt.Warnings[0].Message, fmt.Sprintf("price %q of product %q", "monthly", product))
		require.Contains(t, receipt.Warnings[0].Message, "provider_write_mode="+mode)
		require.NotEqual(t, price.ID, monthly().ID, mode)
		require.Nil(t, monthly().SolanaPlan, mode)
		require.NotContains(t, w.options(monthly().ID), "solana", mode)
	}

	// New terms are a new price with its own plan; the old plan is untouched
	// and stays bound to the archived price its subscribers hold.
	repriced, err := cli(config.ProviderWriteModeFull, file("Plans", 29_000_000))
	require.NoError(t, err, "the operator command publishes too")
	require.Empty(t, repriced.Warnings)
	require.Equal(t, 1, repriced.PricesChanged)
	current := monthly()
	require.NotEqual(t, price.ID, current.ID)
	newPDA := planOf(current, 29_000_000)
	require.NotEqual(t, pda, newPDA)
	after, _ := fake.Account(pda)
	require.True(t, bytes.Equal(before, after), "a plan is never mutated")
	require.Equal(t, 1, fake.Transactions(pda))
	old, err := w.client[remote].GetPrice(ctx, price.ID, billing.GetPriceParams{})
	require.NoError(t, err)
	require.True(t, old.Archived)
	require.Equal(t, pda.String(), *old.SolanaPlan)

	// Back to the old terms in limited mode: the archived price returns with
	// the plan it is bound to, verified without a write.
	_, err = cli(config.ProviderWriteModeLimited, file("Plans restored", 23_000_000))
	require.NoError(t, err)
	require.Equal(t, price.ID, monthly().ID)
	require.Equal(t, pda.String(), *monthly().SolanaPlan)
	require.Equal(t, 1, fake.Transactions(pda))

	// An apply that published, then failed to commit, left a plan on chain
	// and no binding: limited mode binds that plan without publishing again.
	_, err = w.pool.Exec(ctx, w.q(`DELETE FROM billing.price_psp_bindings WHERE plan_pda = $1`), newPDA.String())
	require.NoError(t, err)
	_, err = cli(config.ProviderWriteModeLimited, file("Plans recovered", 29_000_000))
	require.NoError(t, err)
	require.Equal(t, current.ID, monthly().ID)
	require.Equal(t, newPDA, planOf(monthly(), 29_000_000))
	require.Equal(t, 1, fake.Transactions(newPDA), "found, not published again")
}
