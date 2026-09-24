//go:build greenfield && integration

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
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/embed"
	"github.com/open-rails/openrails/internal/solanafake"
)

// Checkout offers a rail iff its PSP is armed and the rail can make that kind
// of new sale (#1078). Each option carries the browser driver and public
// values billing-ui renders; hosts pass options through.

// applyCatalog applies one product with the given price YAML through the
// merchant catalog application, as a host does at boot.
func (w *world) applyCatalog(prices string) (string, error) {
	w.t.Helper()
	client := w.client[embedded]
	revision, err := client.Catalog.Revision(w.t.Context())
	require.NoError(w.t, err)
	key := "rails-" + uuid.NewString()[:8]
	doc := fmt.Sprintf(`schema_version: 1
application_id: gf-%s
expected_revision: %d
products:
- key: %s
  display_name: Rails
  prices:
%s
  entitlements_spec:
    %s: null
`, key, revision.Revision, key, strings.ReplaceAll(prices, "{key}", key), key)
	params, err := openrails.ParseCatalogApplicationYAML([]byte(doc))
	require.NoError(w.t, err)
	_, err = client.Catalog.Apply(w.t.Context(), params)
	return key, err
}

func (w *world) options(priceKey string) map[string]openrails.CheckoutRailOption {
	w.t.Helper()
	list, err := w.client[remote].ListCheckoutRailOptionsByKey(w.t.Context(), priceKey)
	require.NoError(w.t, err)
	out := map[string]openrails.CheckoutRailOption{}
	for _, option := range list {
		out[option.Rail] = option
	}
	return out
}

// withSolana declares a Solana PSP whose signer owns plans on a loopback node.
func withSolana(t *testing.T, w *world) (*solanafake.Node, solanago.PublicKey) {
	fake := solanafake.New()
	t.Cleanup(fake.Close)
	signer := solanago.NewWallet().PrivateKey
	previous := w.cfg
	w.cfg = func(cfg *config.Config) {
		if previous != nil {
			previous(cfg)
		}
		cfg.ProviderSandbox = &config.ProviderSandboxConfig{SolanaRPCURL: fake.URL()}
		cfg.PublicBillingBaseURL = "https://greenfield.test" + mountPrefix
	}
	w.declare = func(psps map[string]embed.PSPConfig) {
		psps["solana"] = embed.PSPConfig{"solana": {
			Signer:   &embed.PSPSignerConfig{Mode: "local_keypair"},
			Secrets:  map[string]string{"private_key": signer.String()},
			Settings: map[string]any{"rpc_provider": "public", "tokens": map[string]any{"SOL": map[string]any{}, "DUSD": map[string]any{}}},
		}}
	}
	return fake, signer.PublicKey()
}

func TestCheckoutOffersSolanaWhenConfigured(t *testing.T) {
	w := prepareWorld(t, 12)
	fake, merchant := withSolana(t, w)
	w.start()

	plan, err := fake.Plan(merchant, 4242, solanafake.DevnetDUSDMint, 23_000_000, monthHours)
	require.NoError(t, err)
	key, err := w.applyCatalog(fmt.Sprintf(`  - key: "{key}-monthly"
    currency: usd
    unit_amount: 23000000
    auto_renew: true
    access_duration_hours: 720
    psps: [nmi, solana]
    psp_links:
      solana:
        plan_pda: %s
        plan_id: "4242"
  - key: "{key}-once"
    currency: usd
    unit_amount: 5000000
    auto_renew: false
    access_duration_hours: 720
    psps: [nmi, solana]
`, plan))
	require.NoError(t, err)

	monthly := w.options(key + "-monthly")
	solana, ok := monthly["solana"]
	require.True(t, ok, "Solana recurring is offered: %+v", monthly)
	require.Equal(t, "subscription", solana.Mode)
	require.Equal(t, "solana_pay", solana.Driver)
	require.Equal(t, map[string]string{"token_symbol": "DUSD", "token_name": "Dev USD", "network": "devnet"}, solana.PublicConfig)
	require.Equal(t, "collect_js", monthly["nmi"].Driver, "NMI enrolls in the page: %+v", monthly["nmi"])
	require.Equal(t, "greenfield-tokenization", monthly["nmi"].PublicConfig["tokenization_key"])
	require.Contains(t, monthly, "stripe", "Stripe stays routable")
	require.Empty(t, monthly["stripe"].Driver, "no publishable key: the browser cannot enroll a Stripe subscription")
	require.NotContains(t, monthly, "ccbill", "CCBill never enrolls a new subscription")

	once := w.options(key + "-once")
	require.Equal(t, "one_off", once["solana"].Mode)
	require.Equal(t, "solana_pay", once["solana"].Driver)
	require.Equal(t, "DUSD", once["solana"].PublicConfig["token_symbol"], "first accepted stablecoin")
	require.Equal(t, "redirect", once["stripe"].Driver, "one-time Stripe uses hosted Checkout")
	require.Equal(t, "collect_js", once["nmi"].Driver)
	require.NotContains(t, once, "ccbill")

	// The advertised Solana option is sellable: a subscription session opens
	// a Solana Pay request for the published plan.
	buyer := w.newCustomer()
	session, err := w.client[embedded].CreateCheckoutSession(t.Context(), openrails.CreateCheckoutSessionRequest{
		Customer: openrails.CheckoutCustomerIdentity{ID: buyer.id}, PriceID: priceID(t, w, key+"-monthly"),
		IdempotencyKey: "sol-" + uuid.NewString(),
		PaymentOptions: openrails.CheckoutPaymentOptions{Rail: solana.Selector, PSPID: solana.PSPID, TokenSymbol: solana.PublicConfig["token_symbol"]},
		SuccessURL:     "https://greenfield.test/return", CancelURL: "https://greenfield.test/return?canceled=1",
	})
	require.NoError(t, err)
	require.Equal(t, "subscription", session.Mode)
	require.Equal(t, "requires_action", session.Status)
	require.True(t, strings.HasPrefix(fmt.Sprint(session.RailData["solana_pay_url"]), "solana:https://greenfield.test/billing/v1/"), "%+v", session.RailData)
}

func TestCheckoutOmitsSolanaWhenNotConfigured(t *testing.T) {
	w := newWorld(t)
	key, err := w.applyCatalog(`  - key: "{key}-monthly"
    currency: usd
    unit_amount: 23000000
    auto_renew: true
    access_duration_hours: 720
    psps: [nmi]
  - key: "{key}-once"
    currency: usd
    unit_amount: 5000000
    auto_renew: false
    access_duration_hours: 720
    psps: [nmi]
`)
	require.NoError(t, err)
	for _, price := range []string{key + "-monthly", key + "-once"} {
		options := w.options(price)
		require.NotContains(t, options, "solana", price)
		require.Contains(t, options, "nmi", price)
		require.Contains(t, options, "stripe", price)
	}
}

func TestCCBillNeverSellsNewSubscriptions(t *testing.T) {
	w := newWorld(t)
	key, err := w.applyCatalog(fmt.Sprintf(`  - key: "{key}-monthly"
    currency: usd
    unit_amount: 23000000
    auto_renew: true
    access_duration_hours: 720
    psps: [ccbill]
    psp_links:
      ccbill:
        form_name: %s
        flex_id: %s
        recurring_billing_option_id: "%s"
`, ccbillFormName, ccbillFlexID, ccbillRBO))
	require.NoError(t, err, "armed engine rails sell the price on local terms")
	options := w.options(key + "-monthly")
	require.NotContains(t, options, "ccbill")
	require.Contains(t, options, "nmi")

	buyer := w.newCustomer()
	_, err = w.client[embedded].CreateCheckoutSession(t.Context(), openrails.CreateCheckoutSessionRequest{
		Customer: openrails.CheckoutCustomerIdentity{ID: buyer.id, VerifiedEmail: "buyer@greenfield.test"}, PriceID: priceID(t, w, key+"-monthly"),
		IdempotencyKey: "ccbill-" + uuid.NewString(),
		PaymentOptions: openrails.CheckoutPaymentOptions{Rail: "ccbill", NameOnCard: "Greenfield Payer", Zip: "10001", Country: "US"},
		SuccessURL:     "https://greenfield.test/return", CancelURL: "https://greenfield.test/return?canceled=1",
	})
	require.Error(t, err, "a named CCBill PSP still cannot enroll")
	require.True(t, errors.Is(err, openrails.ErrInvalid), "%v", err)
	require.Contains(t, err.Error(), "mode_unsupported")
}

func TestCatalogRefusesPriceNoRailCanSell(t *testing.T) {
	w := prepareWorld(t, 12)
	w.declare = func(psps map[string]embed.PSPConfig) {
		delete(psps, "stripe")
		delete(psps, "nmi")
	}
	w.start()
	_, err := w.applyCatalog(fmt.Sprintf(`  - key: "{key}-monthly"
    currency: usd
    unit_amount: 23000000
    auto_renew: true
    access_duration_hours: 720
    psps: [ccbill]
    psp_links:
      ccbill:
        form_name: %s
        flex_id: %s
        recurring_billing_option_id: "%s"
`, ccbillFormName, ccbillFlexID, ccbillRBO))
	require.Error(t, err)
	var status *openrails.StatusError
	require.True(t, errors.As(err, &status), "%v", err)
	require.Equal(t, "price_not_sellable", status.Code, "%v", err)

	// An archived CCBill price keeps serving its retained cohort.
	_, err = w.applyCatalog(fmt.Sprintf(`  - key: "{key}-retired"
    currency: usd
    unit_amount: 19000000
    auto_renew: true
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

func priceID(t *testing.T, w *world, key string) string {
	t.Helper()
	price, err := w.client[embedded].Prices.RetrieveByKey(t.Context(), key)
	require.NoError(t, err)
	return price.ID
}
