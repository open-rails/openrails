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

func TestPurchaseOffersSolanaWhenConfigured(t *testing.T) {
	w := prepareWorld(t, 12)
	sol := withSolana(t, w)
	w.start()

	plan := sol.plan(t, 4242, 23_000_000)
	key, err := w.applyCatalog(fmt.Sprintf(`  "{key}-monthly":
    currency: usd
    unit_amount: 23000000
    billing_interval_hours: 720
    access_duration_hours: 720
    psps:
      nmi: {}
      solana:
        plan_pda: %s
        plan_id: "4242"
  "{key}-once":
    currency: usd
    unit_amount: 5000000
    billing_interval_hours: null
    access_duration_hours: 720
    psps: {nmi: {}, solana: {}}
`, plan))
	require.NoError(t, err)

	monthly := w.options(pid(priceID(t, w, key, key+"-monthly")))
	require.Equal(t, []string{"solana_pay"}, monthly["solana"].Accepts, "a price with a Solana plan subscribes from a wallet: %+v", monthly)
	require.Equal(t, []string{"saved_card", "new_card"}, monthly["nmi"].Accepts)
	require.Equal(t, []string{"saved_card", "new_card"}, monthly["stripe"].Accepts, "a recurring line is no Stripe hosted sale")
	require.NotContains(t, monthly, "ccbill", "the price links no FlexForm")

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
    psps: {nmi: {}}
  "{key}-once":
    currency: usd
    unit_amount: 5000000
    billing_interval_hours: null
    access_duration_hours: 720
    psps: {nmi: {}}
`)
	require.NoError(t, err)
	for _, price := range []string{key + "-monthly", key + "-once"} {
		options := w.options(pid(priceID(t, w, key, price)))
		require.NotContains(t, options, "solana", price)
		require.Contains(t, options, "nmi", price)
		require.Contains(t, options, "stripe", price)
	}
}

// A CCBill PSP sells a subscription whose price links a FlexForm, on that
// form, and nothing one-time.
func TestCCBillSellsSubscriptionsOnItsFlexForm(t *testing.T) {
	w := newWorld(t)
	key, err := w.applyCatalog(fmt.Sprintf(`  "{key}-monthly":
    currency: usd
    unit_amount: 23000000
    billing_interval_hours: 720
    access_duration_hours: 720
    psps:
      ccbill:
        flex_id: %s
        recurring_billing_option_id: "%s"
  "{key}-once":
    currency: usd
    unit_amount: 5000000
    billing_interval_hours: null
    access_duration_hours: 720
    psps:
      ccbill:
        flex_id: %s
`, ccbillFlexID, ccbillRBO, ccbillFlexID))
	require.NoError(t, err, "armed engine rails sell the one-time price on local terms")
	options := w.options(pid(priceID(t, w, key, key+"-monthly")))
	require.Equal(t, []string{"redirect"}, options["ccbill"].Accepts, "%+v", options)
	require.Zero(t, options["ccbill"].FailedAttempts)
	require.Contains(t, options, "nmi")
	require.NotContains(t, w.options(pid(priceID(t, w, key, key+"-once"))), "ccbill", "a FlexForm sells subscriptions only")
}

func TestCatalogRefusesPriceNoRailCanSell(t *testing.T) {
	w := prepareWorld(t, 12)
	w.declare = func(psps map[string]openrails.PSPConfig) {
		delete(psps, "stripe")
		delete(psps, "nmi")
	}
	w.start()
	_, err := w.applyCatalog(fmt.Sprintf(`  "{key}-once":
    currency: usd
    unit_amount: 23000000
    billing_interval_hours: null
    access_duration_hours: 720
    psps:
      ccbill:
        flex_id: %s
`, ccbillFlexID))
	require.Error(t, err, "a FlexForm sells no one-time price")
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
    psps:
      ccbill:
        flex_id: %s
        recurring_billing_option_id: "%s"
`, ccbillFlexID, ccbillRBO))
	require.NoError(t, err)
}

func priceID(t *testing.T, w *world, productKey, key string) string {
	t.Helper()
	price, err := priceByKey(t.Context(), w.client[embedded], productKey, key)
	require.NoError(t, err)
	return price.ID.String()
}
