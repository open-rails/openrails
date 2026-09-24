//go:build greenfield && integration

package greenfield_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	openrailshttp "github.com/open-rails/openrails/adapters/http"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/embed"
)

// SEC-25: a refunded purchase stays refunded. After the provider reports a
// full refund, a replayed or late completion event for the same checkout
// (a new event id) must not grant the purchase again.
func TestSecurityRefundedPurchaseIsNotRegranted(t *testing.T) {
	f := newFixture(t)
	fake := &stripeCheckoutFake{t: t}
	const secret = "whsec_greenfield_security"
	const account = "acct_greenfield_security"
	slug := "security-" + uuid.NewString()[:8]
	runtime, err := embed.New(t.Context(), embed.Options{
		Config: &config.Config{
			TestMode:            config.CredentialPostureSandbox,
			AllowCatalogUpdates: true,
			ProviderWriteMode:   config.ProviderWriteModeFull,
			DB:                  &config.DBConfig{URL: f.dsn(t), Schema: f.schema},
		},
		Merchant: &embed.MerchantDeclaration{Slug: slug, Config: embed.MerchantConfig{
			DisplayName: slug,
			PSPs: map[string]embed.PSPConfig{"stripe": {"stripe": {
				AccountID: account,
				Secrets:   map[string]string{"secret_key": "sk_test_greenfield", "webhook_signing_secret": secret},
			}}},
		}},
		HTTP:            &embed.HTTPConfig{},
		PGXPool:         f.pool,
		River:           embed.RiverManagedByOpenRails(f.schema),
		StripeTransport: fake,
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, runtime.Close(context.Background())) })
	client, err := runtime.Client()
	require.NoError(t, err)

	product, err := client.Products.Create(t.Context(), &openrails.ProductCreateParams{
		Key: "refund-" + uuid.NewString()[:8], DisplayName: "Refunded post", EntitlementsSpec: map[string]*int{"content:refunded": nil},
	})
	require.NoError(t, err)
	price, err := client.Prices.Create(t.Context(), &openrails.PriceCreateParams{ProductID: product.ID, Key: product.Key + "-usd", UnitAmount: 1_000_000, Currency: "USD"})
	require.NoError(t, err)
	userID := uuid.NewString()
	_, err = client.EnsureCustomer(t.Context(), userID)
	require.NoError(t, err)
	_, err = client.CreateCheckoutSession(t.Context(), openrails.CreateCheckoutSessionRequest{
		Customer: openrails.CheckoutCustomerIdentity{ID: userID, VerifiedEmail: "refund@example.test"}, PriceID: price.ID,
		Entitlement: "content:refunded", OfferKind: openrails.OfferPermanent, PaymentOptions: openrails.CheckoutPaymentOptions{Rail: "stripe"},
		IdempotencyKey: "security-" + uuid.NewString(), SuccessURL: "https://example.test/success", CancelURL: "https://example.test/cancel",
	})
	require.NoError(t, err)
	providerSessionID, checkoutSessionID, metadataUserID, metadataPriceID := fake.metadata(t)

	bundle, err := openrailshttp.Routes(runtime)
	require.NoError(t, err)
	mux := http.NewServeMux()
	require.NoError(t, bundle.Mount(mux))
	deliver := func(payload []byte) {
		t.Helper()
		status, body := postSignedStripeWebhook(t, mux, account, secret, payload, time.Now())
		require.Equal(t, http.StatusOK, status, body)
	}
	entitled := func() bool {
		t.Helper()
		got, err := client.CheckEntitlements(t.Context(), userID, []string{"content:refunded"}, time.Time{})
		require.NoError(t, err)
		return got["content:refunded"]
	}

	now := time.Now()
	deliver(stripeWebhookBody(t, "evt_security_paid", "checkout.session.completed", providerSessionID, checkoutSessionID, metadataUserID, metadataPriceID, now.Unix()))
	require.True(t, entitled())

	refund, err := json.Marshal(map[string]any{
		"id": "evt_security_refunded", "type": "charge.refunded", "created": now.Add(time.Second).Unix(),
		"data": map[string]any{"object": map[string]any{
			"object": "charge", "id": "ch_greenfield_webhook", "payment_intent": "pi_greenfield_webhook",
			"amount": 100, "amount_refunded": 100, "refunded": true, "currency": "usd",
			"refunds": map[string]any{"object": "list", "data": []map[string]any{{
				"object": "refund", "id": "re_greenfield_security", "amount": 100, "currency": "usd", "status": "succeeded",
				"charge": "ch_greenfield_webhook", "payment_intent": "pi_greenfield_webhook",
			}}},
		}},
	})
	require.NoError(t, err)
	deliver(refund)
	require.False(t, entitled(), "a full provider refund ends the purchase")

	for i, kind := range []string{"checkout.session.completed", "checkout.session.async_payment_succeeded"} {
		deliver(stripeWebhookBody(t, "evt_security_replay_"+strings.Repeat("x", i+1), kind, providerSessionID, checkoutSessionID, metadataUserID, metadataPriceID, now.Add(time.Duration(i+2)*time.Second).Unix()))
		require.False(t, entitled(), "%s after the refund must not grant again", kind)
	}
	access, err := client.ProductAccess.Check(t.Context(), &openrails.ProductAccessCheckParams{CustomerID: userID, ProductID: product.ID})
	require.NoError(t, err)
	require.False(t, access.HasAccess, "the refunded product is not owned")
	payments, err := client.ListPayments(t.Context(), openrails.PaymentFilter{CustomerID: userID})
	require.NoError(t, err)
	var charged int
	for _, p := range payments.Data {
		if p.Amount > 0 {
			charged++
		}
	}
	require.Equal(t, 1, charged, "replays never record another purchase")
	require.EqualValues(t, 1, fake.checkoutCalls.Load())
}
