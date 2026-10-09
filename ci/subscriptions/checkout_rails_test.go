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

// Checkout offers a rail iff its PSP is armed and the rail can make that kind
// of new sale (#1078). Each option carries the browser driver and public
// values billing-ui renders; hosts pass options through.

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
	_, err = client.ApplyCatalog(w.t.Context(), params)
	return key, err
}

func (w *world) options(selection billing.GetCheckoutConfigParams) map[string]billing.CheckoutOption {
	w.t.Helper()
	config, err := w.client[remote].GetCheckoutConfig(w.t.Context(), selection)
	require.NoError(w.t, err)
	out := map[string]billing.CheckoutOption{}
	for _, option := range config.Options {
		out[string(option.Rail)] = option
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

func TestCheckoutOffersSolanaWhenConfigured(t *testing.T) {
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

	monthly := w.options(billing.GetCheckoutConfigParams{ProductKey: key, PriceKey: key + "-monthly"})
	solana, ok := monthly["solana"]
	require.True(t, ok, "Solana recurring is offered: %+v", monthly)
	require.Equal(t, "subscription", solana.Mode)
	require.Equal(t, "solana_pay", solana.Driver)
	require.Equal(t, map[string]string{"token_symbol": "DUSD", "token_name": "Dev USD", "network": "devnet"}, solana.PublicConfig)
	require.Equal(t, "collect_js", monthly["nmi"].Driver, "NMI enrolls in the page: %+v", monthly["nmi"])
	require.Equal(t, "e2e-tokenization", monthly["nmi"].PublicConfig["tokenization_key"])
	require.Contains(t, monthly, "stripe", "Stripe stays routable")
	require.Empty(t, monthly["stripe"].Driver, "no publishable key: the browser cannot enroll a Stripe subscription")
	require.NotContains(t, monthly, "ccbill", "CCBill never enrolls a new subscription")

	once := w.options(billing.GetCheckoutConfigParams{ProductKey: key, PriceKey: key + "-once"})
	require.Equal(t, "one_off", once["solana"].Mode)
	require.Equal(t, "solana_pay", once["solana"].Driver)
	require.Equal(t, "DUSD", once["solana"].PublicConfig["token_symbol"], "first accepted stablecoin")
	require.Equal(t, "redirect", once["stripe"].Driver, "one-time Stripe uses hosted Checkout")
	require.Equal(t, "collect_js", once["nmi"].Driver)
	require.NotContains(t, once, "ccbill")

	// The advertised Solana option is sellable: a subscription session opens
	// a Solana Pay request for the published plan.
	buyer := w.newCustomer()
	session, err := createCheckoutAttempt(t.Context(), w.client[embedded], billing.CreateCheckoutAttemptParams{
		Customer: billing.CheckoutCustomerIdentity{ID: cid(buyer.id)}, PriceID: pid(priceID(t, w, key, key+"-monthly")),
		IdempotencyKey: "sol-" + uuid.NewString(),
		PaymentOptions: billing.CheckoutPaymentOptions{PSP: solana.PSP, TokenSymbol: solana.PublicConfig["token_symbol"]},
		SuccessURL:     "https://e2e.test/return", CancelURL: "https://e2e.test/return?canceled=1",
	})
	require.NoError(t, err)
	require.Equal(t, "subscription", session.Mode)
	require.Equal(t, billing.CheckoutAttemptRequiresAction, session.Status)
	require.NotNil(t, session.NextAction, "%+v", session)
	require.Equal(t, "solana_pay", session.NextAction.Type)
	require.True(t, strings.HasPrefix(*session.NextAction.URL, "solana:https://e2e.test/billing/v1/checkout-attempts/"+session.ID.String()+"/solana-pay"), "%+v", session.NextAction)
}

func TestCheckoutOmitsSolanaWhenNotConfigured(t *testing.T) {
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
		options := w.options(billing.GetCheckoutConfigParams{ProductKey: key, PriceKey: price})
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
	options := w.options(billing.GetCheckoutConfigParams{ProductKey: key, PriceKey: key + "-monthly"})
	require.NotContains(t, options, "ccbill")
	require.Contains(t, options, "nmi")

	buyer := w.newCustomer()
	_, err = createCheckoutAttempt(t.Context(), w.client[embedded], billing.CreateCheckoutAttemptParams{
		Customer: billing.CheckoutCustomerIdentity{ID: cid(buyer.id), VerifiedEmail: "buyer@e2e.test"}, PriceID: pid(priceID(t, w, key, key+"-monthly")),
		IdempotencyKey: "ccbill-" + uuid.NewString(),
		PaymentOptions: billing.CheckoutPaymentOptions{PSP: "ccbill", BillingDetails: &billing.BillingDetails{Name: new("E2E Payer"), Address: &billing.BillingAddress{PostalCode: new("10001"), Country: new("US")}}},
		SuccessURL:     "https://e2e.test/return", CancelURL: "https://e2e.test/return?canceled=1",
	})
	require.Error(t, err, "a named CCBill PSP still cannot enroll")
	require.True(t, errors.Is(err, billing.ErrInvalid), "%v", err)
	require.Contains(t, err.Error(), "mode_unsupported")
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
	price, err := w.client[embedded].GetPriceByKey(t.Context(), productKey, key)
	require.NoError(t, err)
	return price.ID.String()
}
