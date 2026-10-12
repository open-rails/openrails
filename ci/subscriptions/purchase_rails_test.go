//go:build e2e && integration

package subscriptions_test

import (
	"errors"
	"fmt"
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
	client := w.client[embedded]
	key := "rails-" + uuid.NewString()[:8]
	lines := strings.Split(strings.TrimRight(strings.ReplaceAll(prices, "{key}", key), "\n"), "\n")
	doc := fmt.Sprintf(`schema_version: 1
products:
  %s:
    display_name: Rails
    entitlements: ["%s"]
    prices:
    %s
`, key, key, strings.Join(lines, "\n    "))
	params, err := catalog.ParseApplicationYAML([]byte(doc))
	require.NoError(w.t, err)
	_, err = client.ApplyCatalog(w.t.Context(), params, billing.ApplyCatalogParams{})
	return key, err
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

// withSolana declares a Solana PSP whose signer owns plans on a loopback node.
func withSolana(t *testing.T, w *world) (*solanafake.Node, solanago.PrivateKey) {
	fake := solanafake.New()
	t.Cleanup(fake.Close)
	signer := solanago.NewWallet().PrivateKey
	previous := w.cfg
	w.cfg = func(cfg *config.Config) {
		if previous != nil {
			previous(cfg)
		}
		cfg.ProviderSandbox = &config.ProviderSandboxConfig{SolanaRPCURL: fake.URL()}
		cfg.PublicBillingBaseURL = "https://e2e.test" + mountPrefix
	}
	w.declare = func(psps map[string]openrails.PSPConfig) {
		psps["solana"] = openrails.SolanaPSP{
			PrivateKey:  signer.String(),
			RPCProvider: "public",
			Tokens:      map[string]openrails.SolanaToken{"SOL": {}, "DUSD": {}},
		}.PSPConfig()
	}
	return fake, signer
}

func TestPurchaseOffersSolanaWhenConfigured(t *testing.T) {
	w := prepareWorld(t, 12)
	fake, merchant := withSolana(t, w)
	w.start()

	plan, err := fake.Plan(merchant.PublicKey(), 4242, solanafake.DevnetDUSDMint, 23_000_000, monthHours)
	require.NoError(t, err)
	key, err := w.applyCatalog(fmt.Sprintf(`  "{key}-monthly":
    currency: usd
    unit_amount: 23000000
    billing_interval_hours: 720
    access_duration_hours: 720
    psps: [nmi, solana]
    psp_links:
      solana:
        plan_pda: %s
        plan_id: "4242"
  "{key}-once":
    currency: usd
    unit_amount: 5000000
    billing_interval_hours: null
    access_duration_hours: 720
    psps: [nmi, solana]
`, plan))
	require.NoError(t, err)

	monthly := w.options(pid(priceID(t, w, key, key+"-monthly")))
	require.Equal(t, []string{"solana_pay"}, monthly["solana"].Accepts, "a price with a Solana plan subscribes from a wallet: %+v", monthly)
	require.Equal(t, []string{"saved_card", "new_card"}, monthly["nmi"].Accepts)
	require.Equal(t, []string{"saved_card", "new_card"}, monthly["stripe"].Accepts, "a recurring line is no Stripe hosted sale")
	require.NotContains(t, monthly, "ccbill", "CCBill never enrolls a new subscription")

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
}

func TestPurchaseOmitsSolanaWhenNotConfigured(t *testing.T) {
	w := newWorld(t)
	key, err := w.applyCatalog(`  "{key}-monthly":
    currency: usd
    unit_amount: 23000000
    billing_interval_hours: 720
    access_duration_hours: 720
    psps: [nmi]
  "{key}-once":
    currency: usd
    unit_amount: 5000000
    billing_interval_hours: null
    access_duration_hours: 720
    psps: [nmi]
`)
	require.NoError(t, err)
	for _, price := range []string{key + "-monthly", key + "-once"} {
		options := w.options(pid(priceID(t, w, key, price)))
		require.NotContains(t, options, "solana", price)
		require.Contains(t, options, "nmi", price)
		require.Contains(t, options, "stripe", price)
	}
}

func TestCCBillNeverSellsNewSubscriptions(t *testing.T) {
	w := newWorld(t)
	key, err := w.applyCatalog(fmt.Sprintf(`  "{key}-monthly":
    currency: usd
    unit_amount: 23000000
    billing_interval_hours: 720
    access_duration_hours: 720
    psps: [ccbill]
    psp_links:
      ccbill:
        form_name: %s
        flex_id: %s
        recurring_billing_option_id: "%s"
`, ccbillFormName, ccbillFlexID, ccbillRBO))
	require.NoError(t, err, "armed engine rails sell the price on local terms")
	options := w.options(pid(priceID(t, w, key, key+"-monthly")))
	require.NotContains(t, options, "ccbill")
	require.Contains(t, options, "nmi")

	// The order takes no payment on CCBill: no option names it.
	buyer := w.newCustomer()
	session, err := buyer.sell(embedded, order{price: pid(priceID(t, w, key, key+"-monthly"))})
	require.NoError(t, err)
	for _, option := range session.order.PaymentOptions {
		require.NotEqual(t, "ccbill", option.Rail, "a CCBill PSP is never offered a new subscription")
	}
	_, err = session.buy(buyer, order{rail: "ccbill"})
	require.Error(t, err, "CCBill sells nothing new")
	var attempts int
	require.NoError(t, w.pool.QueryRow(t.Context(), w.q(`SELECT count(*) FROM billing.order_attempts WHERE customer_id = $1`), buyer.id).Scan(&attempts))
	require.Zero(t, attempts, "no attempt is made on CCBill")
}

func TestCatalogRefusesPriceNoRailCanSell(t *testing.T) {
	w := prepareWorld(t, 12)
	w.declare = func(psps map[string]openrails.PSPConfig) {
		delete(psps, "stripe")
		delete(psps, "nmi")
	}
	w.start()
	_, err := w.applyCatalog(fmt.Sprintf(`  "{key}-monthly":
    currency: usd
    unit_amount: 23000000
    billing_interval_hours: 720
    access_duration_hours: 720
    psps: [ccbill]
    psp_links:
      ccbill:
        form_name: %s
        flex_id: %s
        recurring_billing_option_id: "%s"
`, ccbillFormName, ccbillFlexID, ccbillRBO))
	require.Error(t, err)
	var status *billing.StatusError
	require.True(t, errors.As(err, &status), "%v", err)
	require.Equal(t, "price_not_sellable", status.Code, "%v", err)

	// An archived CCBill price keeps serving its retained cohort.
	_, err = w.applyCatalog(fmt.Sprintf(`  "{key}-retired":
    currency: usd
    unit_amount: 19000000
    billing_interval_hours: 720
    archived: true
    access_duration_hours: 720
    psps: [ccbill]
    psp_links:
      ccbill:
        form_name: %s
        flex_id: %s
        recurring_billing_option_id: "%s"
`, ccbillFormName, ccbillFlexID, ccbillRBO))
	require.NoError(t, err)
}

func priceID(t *testing.T, w *world, productKey, key string) string {
	t.Helper()
	price, err := priceByKey(t.Context(), w.client[embedded], productKey, key)
	require.NoError(t, err)
	return price.ID.String()
}
