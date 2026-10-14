//go:build e2e && integration

package subscriptions_test

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"

	solanago "github.com/gagliardetto/solana-go"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/solanafake"
)

// An order offers a PSP iff it is armed and its rail can make that kind of
// new sale: a card PSP takes saved and new cards, Stripe also its hosted page
// and Solana a wallet for one-time lines.

// applyCatalog applies one product with the given prices map YAML (keys at
// two spaces) through the merchant catalog application, as a host does at
// boot. {key} in prices names the product.
func (w *world) applyCatalog(prices string) (string, error) {
	w.t.Helper()
	key, _, err := w.applyCatalogWith(prices, "")
	return key, err
}

// applyCatalogWith applies one product with its prices and provider prices
// (maps YAML, keys at two spaces) and returns the receipt.
func (w *world) applyCatalogWith(prices, providerPrices string) (string, *billing.CatalogApplicationReceipt, error) {
	w.t.Helper()
	client := w.client[embedded]
	key := "rails-" + uuid.NewString()[:8]
	block := func(name, body string) string {
		if body == "" {
			return ""
		}
		lines := strings.Split(strings.TrimRight(strings.ReplaceAll(body, "{key}", key), "\n"), "\n")
		return fmt.Sprintf("    %s:\n    %s\n", name, strings.Join(lines, "\n    "))
	}
	doc := fmt.Sprintf("schema_version: 1\nproducts:\n  %s:\n    display_name: Rails\n    entitlements: [\"%s\"]\n%s%s", key, key, block("prices", prices), block("provider_prices", providerPrices))
	params, err := catalog.ParseApplicationYAML([]byte(doc))
	require.NoError(w.t, err)
	receipt, err := client.ApplyCatalog(w.t.Context(), params, billing.ApplyCatalogParams{})
	return key, receipt, err
}

// options are an order's payment options for price, by rail, as the
// merchant's preview reads them.
func (w *world) options(price billing.PriceID) map[string]billing.OrderPaymentOption {
	w.t.Helper()
	preview, err := w.client[remote].PreviewOrder(w.t.Context(), billing.PreviewOrderParams{CustomerID: w.newCustomer().cid(), Lines: []billing.OrderLineParams{{PriceID: price}}})
	require.NoError(w.t, err)
	out := map[string]billing.OrderPaymentOption{}
	for _, option := range preview.PaymentOptions {
		out[option.Rail] = option
	}
	return out
}

// solanaDeployment is a loopback devnet node, the deployment's cranker and
// the merchant's treasury its Solana PSP pays into. crankers, when set, are
// the listed crankers instead of cranker alone.
type solanaDeployment struct {
	fake     *solanafake.Node
	cranker  solanago.PrivateKey
	crankers []solanago.PrivateKey
	treasury solanago.PublicKey
}

// plan publishes a USDC plan the cranker owns, paying into the treasury.
func (s *solanaDeployment) plan(t *testing.T, id, amount uint64) solanago.PublicKey {
	t.Helper()
	plan, err := s.fake.Plan(s.cranker.PublicKey(), id, solanafake.DevnetUSDCMint, amount, monthHours, s.treasury)
	require.NoError(t, err)
	return plan
}

// withSolana declares a devnet Solana PSP paying into a treasury, and the
// deployment's cranker owning its plans on a loopback node.
func withSolana(t *testing.T, w *world) *solanaDeployment {
	s := &solanaDeployment{fake: solanafake.New(), cranker: solanago.NewWallet().PrivateKey, treasury: solanago.NewWallet().PublicKey()}
	t.Cleanup(s.fake.Close)
	s.fake.Fund(s.treasury, solanago.MustPublicKeyFromBase58(solanafake.DevnetUSDCMint), 0)
	previous := w.cfg
	w.cfg = func(cfg *config.Config) {
		if previous != nil {
			previous(cfg)
		}
		cfg.ProviderSandbox = &config.ProviderSandboxConfig{SolanaRPCURL: s.fake.URL()}
		listed := s.crankers
		if listed == nil {
			listed = []solanago.PrivateKey{s.cranker}
		}
		cfg.Solana = &config.SolanaConfig{}
		for _, key := range listed {
			cfg.Solana.Crankers = append(cfg.Solana.Crankers, config.SolanaCrankerConfig{Mode: config.CrankerLocalKeypair, PrivateKey: key.String()})
		}
		cfg.PublicBillingBaseURL = "https://e2e.test" + mountPrefix
	}
	w.declare = func(psps map[string]openrails.PSPConfig) {
		psps["solana"] = openrails.SolanaPSP{Environment: openrails.EnvironmentDevnet, Treasury: s.treasury.String()}.PSPConfig()
	}
	return s
}

// A recurring price sells on the Solana plan the apply publishes for it from
// its terms; a one-time one by direct transfer.
func TestPurchaseOffersSolanaWhenConfigured(t *testing.T) {
	w := prepareWorld(t, 12)
	withSolana(t, w)
	w.start()

	key, receipt, err := w.applyCatalogWith(`  "{key}-monthly":
    amount: 23 USD
    billing_interval: 30 days
    access_duration: 30 days
  "{key}-once":
    amount: 5 USD
    billing_interval: null
    access_duration: 30 days
`, "")
	require.NoError(t, err)
	require.Empty(t, receipt.Warnings)
	price, err := priceByKey(t.Context(), w.client[embedded], key, key+"-monthly")
	require.NoError(t, err)
	require.NotNil(t, price.SolanaPlan, "the apply published the recurring price's plan")

	monthly := w.options(price.ID)
	require.Equal(t, []string{"solana_pay"}, monthly["solana"].Accepts, "a price with a Solana plan subscribes from a wallet: %+v", monthly)
	require.Equal(t, []string{"saved_card", "new_card"}, monthly["nmi"].Accepts)
	require.Equal(t, []string{"saved_card", "new_card"}, monthly["stripe"].Accepts, "a recurring line is no Stripe hosted sale")
	require.NotContains(t, monthly, "ccbill", "ours never sell through CCBill")

	once := w.options(pid(priceID(t, w, key, key+"-once")))
	require.Equal(t, []string{"solana_pay"}, once["solana"].Accepts)
	require.Equal(t, []string{"saved_card", "new_card", "redirect"}, once["stripe"].Accepts, "one-time Stripe also sells on hosted Checkout")
	require.Equal(t, []string{"saved_card", "new_card"}, once["nmi"].Accepts)
	require.NotContains(t, once, "ccbill")

	// The offered Solana option is sellable: the order answers a Solana Pay
	// transaction request.
	buyer := w.newCustomer()
	paid, err := buyer.buyOrder(embedded, order{price: pid(priceID(t, w, key, key+"-once")), rail: "solana"})
	require.NoError(t, err)
	require.Equal(t, "requires_action", paid.Status)
	require.NotNil(t, paid.NextAction, "%+v", paid.Order)
	require.Equal(t, "solana_pay", paid.NextAction.Type)
	require.True(t, strings.HasPrefix(*paid.NextAction.URL, "solana:https://e2e.test/billing/v1/orders/"+paid.Order.ID.String()+"/solana-pay"), "%+v", paid.NextAction)

	// sell_via restricts a price to some of the merchant's methods.
	cardOnly, _, err := w.applyCatalogWith(`  "{key}-card":
    amount: 5 USD
    access_duration: 30 days
    sell_via: [card]
  "{key}-solana":
    amount: 5 USD
    access_duration: 30 days
    sell_via: [solana]
`, "")
	require.NoError(t, err)
	card := w.options(pid(priceID(t, w, cardOnly, cardOnly+"-card")))
	require.NotContains(t, card, "solana")
	require.Contains(t, card, "nmi")
	sol := w.options(pid(priceID(t, w, cardOnly, cardOnly+"-solana")))
	require.Equal(t, []string{"solana"}, slices.Collect(maps.Keys(sol)), "%+v", sol)
}

func TestPurchaseOmitsSolanaWhenNotConfigured(t *testing.T) {
	w := newWorld(t)
	key, err := w.applyCatalog(`  "{key}-monthly":
    currency: usd
    unit_amount: 23000000
    billing_interval_hours: 720
    access_duration_hours: 720
  "{key}-once":
    currency: usd
    unit_amount: 5000000
    billing_interval_hours: null
    access_duration_hours: 720
`)
	require.NoError(t, err)
	for _, price := range []string{key + "-monthly", key + "-once"} {
		options := w.options(pid(priceID(t, w, key, price)))
		require.NotContains(t, options, "solana", price)
		require.Contains(t, options, "nmi", price)
		require.Contains(t, options, "stripe", price)
	}
}

// CCBill sells its own provider prices on their FlexForms. An order for a
// recurring price of ours also offers the product's CCBill provider price on
// the same terms (billing-ui's fallback); nothing one-time.
func TestCCBillSellsItsProviderPricesOnTheirFlexForms(t *testing.T) {
	w := newWorld(t)
	key, receipt, err := w.applyCatalogWith(`  "{key}-monthly":
    amount: 23 USD
    billing_interval: 30 days
    access_duration: 30 days
  "{key}-once":
    amount: 5 USD
    billing_interval: null
    access_duration: 30 days
`, fmt.Sprintf(`  "{key}-ccbill":
    psp: ccbill
    flex_id: %s
    recurring_billing_option_id: "%s"
    amount: 23 USD
    billing_interval: 30 days
`, ccbillFlexID, ccbillRBO))
	require.NoError(t, err)
	require.Empty(t, receipt.Warnings)
	provider, err := priceByKey(t.Context(), w.client[embedded], key, key+"-ccbill")
	require.NoError(t, err)
	require.NotNil(t, provider.Provider)
	require.Equal(t, map[string]string{"flex_id": ccbillFlexID, "recurring_billing_option_id": ccbillRBO}, provider.Provider.IDs)
	require.Equal(t, int64(23_000_000), provider.UnitAmount, "CCBill declares its terms")

	options := w.options(pid(priceID(t, w, key, key+"-monthly")))
	require.Equal(t, []string{"redirect"}, options["ccbill"].Accepts, "the CCBill provider price stands in: %+v", options)
	require.Zero(t, options["ccbill"].FailedAttempts)
	require.Contains(t, options, "nmi")
	require.NotContains(t, w.options(pid(priceID(t, w, key, key+"-once"))), "ccbill", "a FlexForm sells subscriptions only")
	direct := w.options(provider.ID)
	require.Equal(t, []string{"ccbill"}, slices.Collect(maps.Keys(direct)), "a provider price sells through its provider alone: %+v", direct)
}

// A price restricted to methods that cannot sell it is refused by name; the
// rest of the document applies and is not recorded, so applying it again
// retries.
func TestCatalogRefusesAPriceItsMethodsCannotSell(t *testing.T) {
	w := prepareWorld(t, 12)
	w.declare = func(psps map[string]openrails.PSPConfig) {
		delete(psps, "stripe")
		delete(psps, "nmi")
	}
	w.start()
	key, receipt, err := w.applyCatalogWith(`  "{key}-card":
    amount: 23 USD
    access_duration: 30 days
    sell_via: [card]
  "{key}-monthly-solana":
    amount: 23 USD
    billing_interval: 30 days
    access_duration: 30 days
    sell_via: [solana]
  "{key}-open":
    amount: 23 USD
    access_duration: 30 days
`, "")
	require.NoError(t, err, "a price that cannot sell is never fatal")
	refused := map[string]bool{}
	for _, warning := range receipt.Warnings {
		refused[warning.Key] = warning.Refused
	}
	require.Equal(t, map[string]bool{key + "-card": true, key + "-monthly-solana": true}, refused, "%+v", receipt.Warnings)
	_, err = priceByKey(t.Context(), w.client[embedded], key, key+"-card")
	require.Error(t, err, "a refused price is left out")
	_, err = priceByKey(t.Context(), w.client[embedded], key, key+"-open")
	require.NoError(t, err)
}

func priceID(t *testing.T, w *world, productKey, key string) string {
	t.Helper()
	price, err := priceByKey(t.Context(), w.client[embedded], productKey, key)
	require.NoError(t, err)
	return price.ID.String()
}
