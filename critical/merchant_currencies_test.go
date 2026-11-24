//go:build integration

package critical_test

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/merchantdocs"
	"github.com/stretchr/testify/require"
)

var (
	goldUnit    = billing.MerchantCurrency{Code: "GOLD", Name: "Gold coins", Symbol: "🪙"}
	creditsUnit = billing.MerchantCurrency{Code: "CREDITS", Name: "Credits", Scale: 2}
)

// unitWorld declares GOLD (whole coins) and CREDITS (hundredths) in alpha's
// file configuration; alice is alpha's customer.
func unitWorld(t *testing.T, configure ...func(*world)) *authorityWorld {
	t.Helper()
	return newAuthorityWorld(t, append([]func(*world){func(w *world) {
		alpha := cfgMerchant(w.cfg, "alpha")
		alpha.Currencies = []billing.MerchantCurrency{goldUnit, creditsUnit}
		setCfgMerchant(&w.cfg, "alpha", alpha)
	}}, configure...)...)
}

func customerUnits(t *testing.T, a *authorityWorld, token string) []billing.MerchantCurrency {
	t.Helper()
	status, raw := a.request(t, http.MethodGet, "/v1/me/config", token, nil)
	require.Equal(t, http.StatusOK, status, string(raw))
	var doc billing.CustomerConfig
	require.NoError(t, json.Unmarshal(raw, &doc))
	require.NotNil(t, doc.Currencies, "an empty list is [], never null")
	return doc.Currencies
}

func TestMerchantCurrencies(t *testing.T) {
	t.Run("OR-J05-S06/declared-units-are-served-to-the-merchant-at-their-scale", func(t *testing.T) {
		a := unitWorld(t)
		require.Equal(t, []billing.MerchantCurrency{
			{Code: "CREDITS", Name: "Credits", Symbol: "CREDITS", Scale: 2},
			{Code: "GOLD", Name: "Gold coins", Symbol: "🪙", Scale: 0},
		}, customerUnits(t, a, a.aliceToken), "each unit with its scale; the symbol defaults to the code")
		status, raw := a.request(t, http.MethodGet, "/v1/me/config", a.aliceToken, nil)
		require.Equal(t, http.StatusOK, status)
		require.Contains(t, string(raw), `{"code":"GOLD","name":"Gold coins","symbol":"🪙","scale":0}`, "scale 0 is stated, not omitted")

		status, raw = a.request(t, http.MethodGet, "/v1/config", "", nil)
		require.Equal(t, http.StatusOK, status, string(raw))
		var public billing.PublicConfig
		require.NoError(t, json.Unmarshal(raw, &public))
		require.Equal(t, catalog.Currencies(), public.Currencies, "the deployment registry names no merchant's units")
		usd := false
		for _, c := range public.Currencies {
			require.NotContains(t, []catalog.Currency{"GOLD", "CREDITS"}, c.Code)
			usd = usd || c.Code == "USD" && c.Decimals == 6
		}
		require.True(t, usd)

		state, err := a.alphaRemote.GetMerchantConfiguration(t.Context())
		require.NoError(t, err)
		require.Equal(t, []billing.MerchantCurrency{goldUnit, creditsUnit}, state.Currencies, "staff read the declaration as written")
	})

	t.Run("OR-J05-S07/an-invalid-or-colliding-unit-is-refused-at-load-and-on-edit", func(t *testing.T) {
		w := newWorld(t, func(w *world) {
			alpha := cfgMerchant(w.cfg, "alpha")
			alpha.Currencies = []billing.MerchantCurrency{goldUnit}
			setCfgMerchant(&w.cfg, "alpha", alpha)
			beta := cfgMerchant(w.cfg, "beta")
			beta.Currencies = []billing.MerchantCurrency{{Code: "USDC", Name: "Coins"}}
			setCfgMerchant(&w.cfg, "beta", beta)
			setCfgMerchant(&w.cfg, "gamma", billing.MerchantDeclaration{MerchantDocument: billing.MerchantDocument{DisplayName: "Gamma",
				MerchantSettings: billing.MerchantSettings{Currencies: []billing.MerchantCurrency{{Code: "GEM", Name: "Gems"}}}}})
			setCfgMerchant(&w.cfg, "delta", billing.MerchantDeclaration{MerchantDocument: billing.MerchantDocument{DisplayName: "Delta",
				MerchantSettings: billing.MerchantSettings{Currencies: []billing.MerchantCurrency{goldUnit},
					UsageBilling: &billing.UsageBillingSettings{Currencies: []billing.UsageBillingCurrency{{Currency: "GOLD", CreditLimit: 1000}}}}}})
		})
		state := configurationState(t, w.engine)
		require.ErrorContains(t, state, "merchants.beta.merchant: settings: currencies[0]: code USDC is a currency in OpenRails' registry")
		require.ErrorContains(t, state, `merchants.gamma.merchant: settings: currencies[0]: code "GEM" must be 4 to 12`)
		require.ErrorContains(t, state, "merchants.delta.merchant: settings: usage_billing.currencies[0].currency: GOLD is a merchant's own unit")
		require.NotContains(t, state.Error(), "merchants.alpha.", "a valid declaration serves")
		served, err := w.alpha.GetMerchantConfiguration(t.Context())
		require.NoError(t, err)
		require.Equal(t, []billing.MerchantCurrency{goldUnit}, served.Currencies)

		a, _ := newPSPWorld(t)
		edited, err := a.alphaRemote.UpdateMerchantConfiguration(t.Context(), billing.UpdateMerchantConfigurationParams{DisplayName: ptr("Alpha"),
			MerchantSettings: &billing.MerchantSettings{Currencies: []billing.MerchantCurrency{goldUnit}}})
		require.NoError(t, err)
		require.Equal(t, []billing.MerchantCurrency{goldUnit}, edited.Currencies)
		require.Equal(t, []billing.MerchantCurrency{{Code: "GOLD", Name: "Gold coins", Symbol: "🪙"}}, customerUnits(t, a, a.aliceToken), "a Vault edit serves the unit")
		// Once a balance counts it, a unit is neither removed nor rescaled.
		_, err = a.alphaRemote.CreateCreditGrants(t.Context(), []billing.CreateCreditGrantParams{{CustomerID: billing.CustomerID(uuid.MustParse(a.alice.ID)), Currency: "GOLD", Amount: 400, Source: "critical", SourceID: "gold"}})
		require.NoError(t, err)
		for name, currencies := range map[string][]billing.MerchantCurrency{
			"rescaled": {{Code: "GOLD", Name: "Gold coins", Symbol: "🪙", Scale: 2}},
			"removed":  {creditsUnit},
		} {
			_, err := a.alphaRemote.UpdateMerchantConfiguration(t.Context(), billing.UpdateMerchantConfigurationParams{MerchantSettings: &billing.MerchantSettings{Currencies: currencies}})
			refusal := requireCode(t, err, billing.CodeInvalidParam)
			require.Equal(t, "settings.currencies", *refusal.Param, name)
		}
		for name, invalid := range map[string][]billing.MerchantCurrency{
			"registry":  {{Code: "USD", Name: "Dollars"}},
			"iso-space": {{Code: "GEM", Name: "Gems"}},
			"lowercase": {{Code: "gold", Name: "Gold"}},
			"scale":     {{Code: "GOLD", Name: "Gold", Scale: 7}},
			"twice":     {goldUnit, goldUnit},
			"money":     {{Code: "GOLD", Name: "Gold", Symbol: "$"}},
			"nameless":  {{Code: "GOLD"}},
		} {
			_, err := a.alphaRemote.UpdateMerchantConfiguration(t.Context(), billing.UpdateMerchantConfigurationParams{MerchantSettings: &billing.MerchantSettings{Currencies: invalid}})
			requireCode(t, err, billing.CodeInvalidParam)
			require.ErrorContains(t, err, "currencies", name)
		}
		for param, settings := range map[string]billing.MerchantSettings{
			"usage_billing.currencies[0].currency":    {UsageBilling: &billing.UsageBillingSettings{Currencies: []billing.UsageBillingCurrency{{Currency: "GOLD", CreditLimit: 1000}}}},
			"psp_routing[0].match.currency":           {PSPRouting: &[]billing.PSPRoutingRule{{Match: billing.PSPRoutingMatch{Currency: "gold"}, Prefer: []string{"stripe"}}}},
			"balance_notices.low_amounts[0].currency": {BalanceNotices: &billing.BalanceNoticeSettings{LowAmounts: []billing.BalanceNoticeAmount{{Currency: "GOLD", Amount: 50}}}},
		} {
			_, err := a.alphaRemote.UpdateMerchantConfiguration(t.Context(), billing.UpdateMerchantConfigurationParams{MerchantSettings: &settings})
			refusal := requireCode(t, err, billing.CodeMerchantUnitUnsupported)
			require.NotNil(t, refusal.Param)
			require.Equal(t, param, *refusal.Param)
		}
		after, err := a.alphaRemote.GetMerchantConfiguration(t.Context())
		require.NoError(t, err)
		require.Equal(t, edited.Revision, after.Revision, "a refused edit writes nothing")
		require.Equal(t, []billing.MerchantCurrency{goldUnit}, after.Currencies)
		require.Nil(t, after.UsageBilling)

		// A Vault edit made outside OpenRails that removes the counted unit
		// keeps serving its declaration, with a finding.
		path := a.cfg.Vault.KVMount + "/data/openrails/merchants/" + a.alpha.MerchantID().UUID().String() + "/merchant"
		stored, err := a.deps.Vault.Logical().ReadWithContext(t.Context(), path)
		require.NoError(t, err)
		data := stored.Data["data"].(map[string]any)
		data["currencies"] = []any{map[string]any{"code": "CREDITS", "name": "Credits", "scale": 2}}
		_, err = a.deps.Vault.Logical().WriteWithContext(t.Context(), path, map[string]any{"data": data})
		require.NoError(t, err)
		_, err = a.pool.Exec(t.Context(), "SELECT pg_notify($1, $2)", merchantdocs.Channel(a.schema), a.alpha.MerchantID().UUID().String())
		require.NoError(t, err)
		require.Eventually(t, func() bool {
			findings, err := a.alphaRemote.ListFindings(t.Context(), billing.FindingListParams{Type: merchantdocs.RefusedDocumentFindingType})
			if err != nil {
				return false
			}
			for _, f := range findings.Items {
				if f.SubjectKey == "currencies" && (f.Status == "reconcile_required" || f.Status == "requires_review") {
					return true
				}
			}
			return false
		}, 20*time.Second, 50*time.Millisecond, "the operator is told the unit is still counted")
		units := customerUnits(t, a, a.aliceToken)
		require.Contains(t, units, billing.MerchantCurrency{Code: "GOLD", Name: "Gold coins", Symbol: "🪙"}, "GOLD keeps the declaration its balances are counted in")
		gold, err := a.alphaRemote.GetBalance(t.Context(), billing.CustomerID(uuid.MustParse(a.alice.ID)), billing.GetBalanceParams{Currency: "GOLD"})
		require.NoError(t, err)
		require.EqualValues(t, 400, gold.BalanceAmount)
	})

	t.Run("OR-J05-S08/a-unit-is-never-owed-never-a-packs-price-and-only-the-merchants-own", func(t *testing.T) {
		a := unitWorld(t)
		alice := billing.CustomerID(uuid.MustParse(a.alice.ID))
		_, err := a.alphaRemote.CreateCreditGrants(t.Context(), []billing.CreateCreditGrantParams{{CustomerID: alice, Currency: "GEMS", Amount: 400, Source: "critical", SourceID: "gems"}})
		refusal := requireCode(t, err, billing.CodeCurrencyUnsupported)
		require.Equal(t, "items[0].currency", *refusal.Param, "a unit the merchant does not declare is an unknown currency")
		_, err = a.alphaRemote.GetBalance(t.Context(), alice, billing.GetBalanceParams{Currency: "GEMS"})
		requireCode(t, err, billing.CodeCurrencyUnsupported)
		_, err = a.alphaRemote.UpdateCustomer(t.Context(), alice, billing.UpdateCustomerParams{CreditLimits: []billing.CreditLimit{{Currency: "GOLD", Amount: 100}}})
		requireCode(t, err, billing.CodeCurrencyUnsupported)
		_, err = a.alphaRemote.CreateCreditGrants(t.Context(), []billing.CreateCreditGrantParams{{CustomerID: alice, Currency: "GOLD", Amount: 400, Source: "critical", SourceID: "gold"}})
		require.NoError(t, err, "a declared unit is granted")

		shop, err := a.alphaRemote.CreateProduct(t.Context(), billing.CreateProductParams{Key: "shop", DisplayName: "Shop", Ownership: catalog.OwnershipConsumable, Entitlements: []string{"shop:sword"}})
		require.NoError(t, err)
		_, err = a.alphaRemote.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: shop.ID, Key: "monthly", UnitAmount: 50, Currency: "GOLD", BillingIntervalHours: ptr(720)})
		requireCode(t, err, billing.CodeInvalidParam)
		_, err = a.alphaRemote.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: shop.ID, Key: "gems", UnitAmount: 50, Currency: "GEMS"})
		refusal = requireCode(t, err, billing.CodeCurrencyUnsupported)
		require.Equal(t, "currency", *refusal.Param)
		_, err = a.alphaRemote.CreateProduct(t.Context(), billing.CreateProductParams{Key: "deposit", DisplayName: "Deposit",
			CreditGrant: &catalog.CreditGrantSpec{Currency: "GOLD", FromPayment: true}})
		requireCode(t, err, billing.CodeInvalidParam)
		pack, err := a.alphaRemote.CreateProduct(t.Context(), billing.CreateProductParams{Key: "pack", DisplayName: "Gold pack",
			CreditGrant: &catalog.CreditGrantSpec{Currency: "GOLD", Amount: ptr(int64(400))}})
		require.NoError(t, err)
		_, err = a.alphaRemote.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: pack.ID, Key: "in-gold", UnitAmount: 50, Currency: "GOLD"})
		requireCode(t, err, billing.CodeInvalidParam)
		prices, err := a.alphaRemote.ListPrices(t.Context(), billing.PriceListParams{ProductID: pack.ID})
		require.NoError(t, err)
		require.Empty(t, prices.Items, "a pack is bought with money, never with a unit")

		_, err = catalog.ParseApplicationYAML([]byte("schema_version: 1\nproducts:\n  sword: {display_name: Sword, prices: {gold: {amount: 50 GOLD}}}\n"))
		require.ErrorContains(t, err, "GOLD is a merchant's unit", "a unit's scale is its merchant's: the amount shorthand cannot read it")
		for param, document := range map[string]string{
			"products.sword.prices.gems.currency":           "schema_version: 1\nproducts:\n  sword: {display_name: Sword, prices: {gems: {currency: GEMS, unit_amount: \"50\"}}}\n",
			"products.pack.credit_grant.currency":           "schema_version: 1\nproducts:\n  pack:\n    display_name: Gem pack\n    credit_grant: {currency: GEMS, amount: \"400\"}\n    prices: {pack: {amount: 20 USD}}\n",
			"products.metered.rate_cards[0].price.currency": "schema_version: 1\nmeters:\n  calls: {event_type: calls, value_property: n, aggregation: sum}\nproducts:\n  metered: {display_name: Metered, rate_cards: [{meter: calls, price: {model: dynamic, currency: GEMS}}]}\n",
		} {
			parsed, err := catalog.ParseApplicationYAML([]byte(document))
			require.NoError(t, err)
			_, err = a.alphaRemote.ApplyCatalog(t.Context(), parsed, billing.ApplyCatalogParams{})
			refusal := requireCode(t, err, billing.CodeCurrencyUnsupported)
			require.Equal(t, param, *refusal.Param)
		}
		products, err := a.alphaRemote.ListProducts(t.Context(), billing.ProductListParams{Keys: []string{"sword", "metered"}})
		require.NoError(t, err)
		require.Empty(t, products.Items, "a refused document applies nothing")
	})

	t.Run("OR-J05-S09/another-merchant-does-not-see-a-merchants-units", func(t *testing.T) {
		a := unitWorld(t, func(w *world) {
			beta := cfgMerchant(w.cfg, "beta")
			beta.Currencies = []billing.MerchantCurrency{{Code: "GOLD", Name: "Beta gold", Scale: 2}}
			setCfgMerchant(&w.cfg, "beta", beta)
		})
		alice := billing.CustomerID(uuid.MustParse(a.alice.ID))
		_, err := a.betaRemote.CreateCreditGrants(t.Context(), []billing.CreateCreditGrantParams{{CustomerID: alice, Currency: "CREDITS", Amount: 100, Source: "critical", SourceID: "credits"}})
		requireCode(t, err, billing.CodeCurrencyUnsupported)
		_, err = a.betaRemote.CreateCreditGrants(t.Context(), []billing.CreateCreditGrantParams{{CustomerID: alice, Currency: "GOLD", Amount: 150, Source: "critical", SourceID: "gold"}})
		require.NoError(t, err)
		_, err = a.alphaRemote.CreateCreditGrants(t.Context(), []billing.CreateCreditGrantParams{{CustomerID: alice, Currency: "GOLD", Amount: 7, Source: "critical", SourceID: "gold"}})
		require.NoError(t, err)
		betaGold, err := a.betaRemote.GetBalance(t.Context(), alice, billing.GetBalanceParams{Currency: "GOLD"})
		require.NoError(t, err)
		require.EqualValues(t, 150, betaGold.BalanceAmount, "1.50 of beta's gold")
		require.Equal(t, &billing.MerchantCurrency{Code: "GOLD", Name: "Beta gold", Symbol: "GOLD", Scale: 2}, betaGold.Unit)
		alphaGold, err := a.alphaRemote.GetBalance(t.Context(), alice, billing.GetBalanceParams{Currency: "GOLD"})
		require.NoError(t, err)
		require.EqualValues(t, 7, alphaGold.BalanceAmount, "alpha's gold is its own balance")
		require.Equal(t, 0, alphaGold.Unit.Scale)
		state, err := a.betaRemote.GetMerchantConfiguration(t.Context())
		require.NoError(t, err)
		require.Equal(t, []billing.MerchantCurrency{{Code: "GOLD", Name: "Beta gold", Scale: 2}}, state.Currencies)

		a.bindCustomers(t, "beta")
		require.Equal(t, []billing.MerchantCurrency{{Code: "GOLD", Name: "Beta gold", Symbol: "GOLD", Scale: 2}}, customerUnits(t, a, a.aliceToken),
			"beta's customers see beta's GOLD at its own scale, never alpha's units")
	})

	t.Run("OR-J05-S10/a-restart-on-a-file-that-rescales-a-counted-unit-keeps-its-recorded-scale", func(t *testing.T) {
		a := unitWorld(t)
		alice := billing.CustomerID(uuid.MustParse(a.alice.ID))
		_, err := a.alpha.CreateCreditGrants(t.Context(), []billing.CreateCreditGrantParams{{CustomerID: alice, Currency: "GOLD", Amount: 400, Source: "critical", SourceID: "gold"}})
		require.NoError(t, err)
		// The file now declares GOLD in hundredths, and the process restarts:
		// nothing served before is left to compare it with but the units
		// book's record.
		alpha := cfgMerchant(a.cfg, "alpha")
		alpha.Currencies = []billing.MerchantCurrency{{Code: "GOLD", Name: "Gold bars", Symbol: "🪙", Scale: 2}, creditsUnit}
		setCfgMerchant(&a.cfg, "alpha", alpha)
		a.restart(t)
		require.Eventually(t, func() bool {
			findings, err := a.alpha.ListFindings(t.Context(), billing.FindingListParams{Type: merchantdocs.RefusedDocumentFindingType})
			if err != nil {
				return false
			}
			for _, f := range findings.Items {
				if f.SubjectKey == "currencies" && (f.Status == "reconcile_required" || f.Status == "requires_review") {
					return true
				}
			}
			return false
		}, 20*time.Second, 50*time.Millisecond, "the operator is told GOLD keeps its scale")
		gold, err := a.alpha.GetBalance(t.Context(), alice, billing.GetBalanceParams{Currency: "GOLD"})
		require.NoError(t, err)
		require.EqualValues(t, 400, gold.BalanceAmount)
		require.Equal(t, &billing.MerchantCurrency{Code: "GOLD", Name: "Gold bars", Symbol: "🪙", Scale: 0}, gold.Unit, "400 whole coins, never 4.00")
		require.Contains(t, customerUnits(t, a, a.aliceToken), billing.MerchantCurrency{Code: "GOLD", Name: "Gold bars", Symbol: "🪙", Scale: 0})
	})

	t.Run("OR-J05-S11/a-catalog-that-names-a-unit-counts-it", func(t *testing.T) {
		a := unitWorld(t)
		potion, err := a.alpha.CreateProduct(t.Context(), billing.CreateProductParams{Key: "potion", DisplayName: "Potion", Ownership: catalog.OwnershipConsumable})
		require.NoError(t, err)
		_, err = a.alpha.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: potion.ID, Key: "gold", UnitAmount: 50, Currency: "GOLD"})
		require.NoError(t, err)
		require.Equal(t, 0, countedScale(t, a.world, a.alpha.MerchantID(), "GOLD"), "a price in GOLD counts it, though no balance holds any")

		// A restart on a file that rescales it keeps the price's scale.
		alpha := cfgMerchant(a.cfg, "alpha")
		alpha.Currencies = []billing.MerchantCurrency{{Code: "GOLD", Name: "Gold bars", Scale: 2}, creditsUnit}
		setCfgMerchant(&a.cfg, "alpha", alpha)
		a.restart(t)
		require.Contains(t, customerUnits(t, a, a.aliceToken), billing.MerchantCurrency{Code: "GOLD", Name: "Gold bars", Symbol: "GOLD", Scale: 0}, "50 whole coins, never 0.50")
	})

	t.Run("OR-J05-S12/a-units-first-count-reads-its-declaration-where-it-is-stored", func(t *testing.T) {
		a, _ := newPSPWorld(t)
		_, err := a.alphaRemote.UpdateMerchantConfiguration(t.Context(), billing.UpdateMerchantConfigurationParams{DisplayName: ptr("Alpha"),
			MerchantSettings: &billing.MerchantSettings{Currencies: []billing.MerchantCurrency{goldUnit}}})
		require.NoError(t, err)
		// Another replica declares GOLD in hundredths and its announcement
		// is lost: this one still serves whole coins.
		path := a.cfg.Vault.KVMount + "/data/openrails/merchants/" + a.alpha.MerchantID().UUID().String() + "/merchant"
		stored, err := a.deps.Vault.Logical().ReadWithContext(t.Context(), path)
		require.NoError(t, err)
		data := stored.Data["data"].(map[string]any)
		data["currencies"] = []any{map[string]any{"code": "GOLD", "name": "Gold coins", "symbol": "🪙", "scale": 2}}
		_, err = a.deps.Vault.Logical().WriteWithContext(t.Context(), path, map[string]any{"data": data})
		require.NoError(t, err)
		served, err := a.alphaRemote.GetMerchantConfiguration(t.Context())
		require.NoError(t, err)
		require.Equal(t, []billing.MerchantCurrency{goldUnit}, served.Currencies, "this replica's copy is stale")

		_, err = a.alphaRemote.CreateCreditGrants(t.Context(), []billing.CreateCreditGrantParams{{CustomerID: billing.CustomerID(uuid.MustParse(a.alice.ID)), Currency: "GOLD", Amount: 400, Source: "critical", SourceID: "gold"}})
		require.NoError(t, err)
		require.Equal(t, 2, countedScale(t, a.world, a.alpha.MerchantID(), "GOLD"), "the units book counts GOLD as Vault declares it, not as a stale copy does")
	})
}

// countedScale is the scale the units book recorded unit at for merchant.
func countedScale(t *testing.T, w *world, merchant billing.MerchantID, unit string) int {
	t.Helper()
	var scale int
	require.NoError(t, w.pool.QueryRow(t.Context(), "SELECT scale FROM "+pgx.Identifier{w.schema, "ledger_units"}.Sanitize()+" WHERE merchant_id = $1 AND unit = $2",
		merchant.UUID(), unit).Scan(&scale))
	return scale
}
