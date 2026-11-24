//go:build integration

package critical_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
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
	})

	t.Run("OR-J05-S08/a-unit-is-refused-where-money-is-taken", func(t *testing.T) {
		a := unitWorld(t)
		alice := billing.CustomerID(uuid.MustParse(a.alice.ID))
		_, err := a.alphaRemote.CreateCreditGrants(t.Context(), []billing.CreateCreditGrantParams{{CustomerID: alice, Currency: "GOLD", Amount: 400, Source: "critical", SourceID: "gold"}})
		refusal := requireCode(t, err, billing.CodeMerchantUnitUnsupported)
		require.Equal(t, "items[0].currency", *refusal.Param)
		grants, err := a.alphaRemote.ListCreditGrants(t.Context(), billing.CreditGrantListParams{CustomerID: alice})
		require.NoError(t, err)
		require.Empty(t, grants.Items, "nothing is credited in a unit")
		_, err = a.alphaRemote.GetBalance(t.Context(), alice, billing.GetBalanceParams{Currency: "credits"})
		requireCode(t, err, billing.CodeMerchantUnitUnsupported)
		_, err = a.alphaRemote.GetBalance(t.Context(), alice, billing.GetBalanceParams{Currency: "GOLDX"})
		requireCode(t, err, billing.CodeCurrencyUnsupported)

		product, err := a.alphaRemote.CreateProduct(t.Context(), billing.CreateProductParams{Key: "shop", DisplayName: "Shop", Ownership: catalog.OwnershipConsumable, Entitlements: []string{"shop:sword"}})
		require.NoError(t, err)
		_, err = a.alphaRemote.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: product.ID, Key: "sword", UnitAmount: 50, Currency: "GOLD"})
		refusal = requireCode(t, err, billing.CodeMerchantUnitUnsupported)
		require.Equal(t, "currency", *refusal.Param)
		prices, err := a.alphaRemote.ListPrices(t.Context(), billing.PriceListParams{ProductID: product.ID})
		require.NoError(t, err)
		require.Empty(t, prices.Items, "no price is denominated in a unit")

		for _, document := range []string{
			"schema_version: 1\nproducts:\n  sword: {display_name: Sword, prices: {gold: {amount: 50 GOLD}}}\n",
			"schema_version: 1\nproducts:\n  sword: {display_name: Sword, prices: {gold: {currency: GOLD, unit_amount: \"50\"}}}\n",
		} {
			_, err := catalog.ParseApplicationYAML([]byte(document))
			require.ErrorContains(t, err, `unknown currency "GOLD"`, "a catalog prices in OpenRails' registry alone")
		}
		for param, document := range map[string]string{
			"products.pack.credit_grant.currency":           "schema_version: 1\nproducts:\n  pack:\n    display_name: Gold pack\n    credit_grant: {currency: GOLD, amount: \"400\"}\n    prices: {pack: {amount: 20 USD}}\n",
			"products.metered.rate_cards[0].price.currency": "schema_version: 1\nmeters:\n  calls: {event_type: calls, value_property: n, aggregation: sum}\nproducts:\n  metered: {display_name: Metered, rate_cards: [{meter: calls, price: {model: dynamic, currency: GOLD}}]}\n",
		} {
			parsed, err := catalog.ParseApplicationYAML([]byte(document))
			require.NoError(t, err)
			_, err = a.alphaRemote.ApplyCatalog(t.Context(), parsed, billing.ApplyCatalogParams{})
			refusal := requireCode(t, err, billing.CodeMerchantUnitUnsupported)
			require.Equal(t, param, *refusal.Param)
		}
		products, err := a.alphaRemote.ListProducts(t.Context(), billing.ProductListParams{Keys: []string{"pack", "metered"}})
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
		_, err = a.betaRemote.CreateCreditGrants(t.Context(), []billing.CreateCreditGrantParams{{CustomerID: alice, Currency: "GOLD", Amount: 100, Source: "critical", SourceID: "gold"}})
		requireCode(t, err, billing.CodeMerchantUnitUnsupported)
		state, err := a.betaRemote.GetMerchantConfiguration(t.Context())
		require.NoError(t, err)
		require.Equal(t, []billing.MerchantCurrency{{Code: "GOLD", Name: "Beta gold", Scale: 2}}, state.Currencies)

		a.bindCustomers(t, "beta")
		require.Equal(t, []billing.MerchantCurrency{{Code: "GOLD", Name: "Beta gold", Symbol: "GOLD", Scale: 2}}, customerUnits(t, a, a.aliceToken),
			"beta's customers see beta's GOLD at its own scale, never alpha's units")
	})
}
