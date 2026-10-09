//go:build e2e && integration

package ci_test

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
	"github.com/open-rails/openrails/billing"
)

// SEC-25: a refunded purchase stays refunded. After the provider reports a
// full refund, a replayed or late completion event for the same checkout
// (a new event id) must not grant the purchase again.
func TestSecurityRefundedPurchaseIsNotRegranted(t *testing.T) {
	f := newFixture(t)
	fake := &stripeCheckoutFake{t: t}
	const secret = "whsec_e2e_security"
	const account = "acct_e2e_security"
	slug := "security-" + uuid.NewString()[:8]
	cfg := f.config()
	cfg.ProviderWriteMode = openrails.ProviderWritesFull
	cfg.ReturnOrigins = []string{"https://example.test"}
	cfg.Merchant = openrails.MerchantDeclaration{Slug: slug, DisplayName: slug,
		PSPs: map[string]openrails.PSPConfig{"stripe": {Rail: "stripe",
			AccountID: account,
			Secrets:   map[string]string{"secret_key": "sk_test_e2e", "webhook_signing_secret": secret},
		}},
	}
	client, err := openrails.New(t.Context(), cfg, openrails.Deps{Postgres: f.pool, StripeTransport: fake})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, client.Close(context.Background())) })

	product, err := client.CreateProduct(t.Context(), billing.CreateProductParams{
		Key: "refund-" + uuid.NewString()[:8], DisplayName: "Refunded post", Entitlements: []string{"content:refunded"},
	})
	require.NoError(t, err)
	price, err := client.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: product.ID, Key: product.Key + "-usd", UnitAmount: 1_000_000, Currency: "USD"})
	require.NoError(t, err)
	userID := uuid.NewString()
	_, err = client.EnsureCustomers(t.Context(), []billing.EnsureCustomerParams{{ID: billing.CustomerID(uuid.MustParse(userID))}})
	require.NoError(t, err)
	session, err := sell(t, client, billing.CreateCheckoutSessionParams{
		Customer: billing.CheckoutCustomerIdentity{ID: cid(userID), VerifiedEmail: "refund@example.test"}, PriceID: price.ID, SuccessURL: "https://example.test/success",
	})
	require.NoError(t, err)
	_, err = session.pay("stripe", nil)
	require.NoError(t, err)
	providerSessionID, checkoutAttemptID, metadataUserID, metadataPriceID := fake.metadata(t)

	mux := http.NewServeMux()
	require.NoError(t, openrailshttp.Mount(mux, client, openrails.Routes{}))
	deliver := func(payload []byte) {
		t.Helper()
		status, body := postSignedStripeWebhook(t, mux, account, secret, payload, time.Now())
		require.Equal(t, http.StatusOK, status, body)
	}
	entitled := func() bool {
		t.Helper()
		got, err := client.CheckEntitlements(t.Context(), billing.CustomerID(uuid.MustParse(userID)), billing.CheckEntitlementsParams{Entitlements: []string{"content:refunded"}})
		require.NoError(t, err)
		return got.Entitlements["content:refunded"]
	}

	now := time.Now()
	deliver(stripeWebhookBody(t, "evt_security_paid", "checkout.session.completed", providerSessionID, checkoutAttemptID, metadataUserID, metadataPriceID, now.Unix()))
	require.True(t, entitled())

	refund, err := json.Marshal(map[string]any{
		"id": "evt_security_refunded", "type": "charge.refunded", "created": now.Add(time.Second).Unix(),
		"data": map[string]any{"object": map[string]any{
			"object": "charge", "id": "ch_e2e_webhook", "payment_intent": "pi_e2e_webhook",
			"amount": 100, "amount_refunded": 100, "refunded": true, "currency": "usd",
			"refunds": map[string]any{"object": "list", "data": []map[string]any{{
				"object": "refund", "id": "re_e2e_security", "amount": 100, "currency": "usd", "status": "succeeded",
				"charge": "ch_e2e_webhook", "payment_intent": "pi_e2e_webhook",
			}}},
		}},
	})
	require.NoError(t, err)
	deliver(refund)
	require.False(t, entitled(), "a full provider refund ends the purchase")

	for i, kind := range []string{"checkout.session.completed", "checkout.session.async_payment_succeeded"} {
		deliver(stripeWebhookBody(t, "evt_security_replay_"+strings.Repeat("x", i+1), kind, providerSessionID, checkoutAttemptID, metadataUserID, metadataPriceID, now.Add(time.Duration(i+2)*time.Second).Unix()))
		require.False(t, entitled(), "%s after the refund must not grant again", kind)
	}
	access, err := client.CheckProductAccess(t.Context(), billing.CustomerID(uuid.MustParse(userID)), billing.CheckProductAccessParams{ProductIDs: []billing.ProductID{product.ID}})
	require.NoError(t, err)
	require.False(t, access[product.ID.String()], "the refunded product is not owned")
	payments, err := client.ListPayments(t.Context(), billing.PaymentListParams{CustomerID: billing.CustomerID(uuid.MustParse(userID))})
	require.NoError(t, err)
	var charged int
	for _, p := range payments.Items {
		if p.Amount > 0 {
			charged++
		}
	}
	require.Equal(t, 1, charged, "replays never record another purchase")
	require.EqualValues(t, 1, fake.checkoutCalls.Load())
}
