//go:build e2e && integration

package ci_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails"
	openrailshttp "github.com/open-rails/openrails/adapters/http"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	"github.com/stretchr/testify/require"
)

type creditCheckoutFake struct {
	mu    sync.Mutex
	forms []url.Values
}

func (f *creditCheckoutFake) RoundTrip(r *http.Request) (*http.Response, error) {
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/v1/account":
		return jsonResponse(`{"object":"account","id":"acct_e2e"}`), nil
	case r.Method == http.MethodGet && r.URL.Path == "/v1/balance":
		return jsonResponse(`{"object":"balance","livemode":false}`), nil
	case r.Method == http.MethodPost && r.URL.Path == "/v1/checkout/sessions":
		if err := r.ParseForm(); err != nil {
			return nil, err
		}
		if r.PostForm.Get("line_items[0][price_data][unit_amount]") != "100" {
			return nil, fmt.Errorf("wrong purchased credit price: %v", r.PostForm)
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		f.forms = append(f.forms, r.PostForm)
		return jsonResponse(fmt.Sprintf(`{"id":"cs_credit_%d","url":"https://checkout.stripe.test/credit"}`, len(f.forms))), nil
	default:
		return nil, fmt.Errorf("unexpected credit provider request: %s %s", r.Method, r.URL.Path)
	}
}

func (f *creditCheckoutFake) paid(t *testing.T, n int, event string) []byte {
	t.Helper()
	f.mu.Lock()
	form := f.forms[n-1]
	f.mu.Unlock()
	raw, err := json.Marshal(map[string]any{
		"id": event, "type": "checkout.session.completed", "created": time.Now().Unix(),
		"data": map[string]any{"object": map[string]any{
			"id": fmt.Sprintf("cs_credit_%d", n), "mode": "payment", "status": "complete", "payment_status": "paid",
			"payment_intent": fmt.Sprintf("pi_credit_%d", n), "amount_total": 100, "currency": "usd",
			"metadata": map[string]string{"checkout_attempt_id": form.Get("metadata[checkout_attempt_id]"), "user_id": form.Get("metadata[user_id]"), "internal_price_id": form.Get("metadata[internal_price_id]")},
		}},
	})
	require.NoError(t, err)
	return raw
}

// A pack is consumed into an existing money balance, never owned permanently.
// Accepted benefit terms survive a product edit while Stripe checkout is open;
// duplicate receipts, repeat purchases and refunds share the same source lot.
func TestPurchasedCreditsFreezeBenefitsAndRemainRepeatable(t *testing.T) {
	f := newFixture(t)
	provider := &creditCheckoutFake{}
	cfg := f.config()
	cfg.ProviderWriteMode = openrails.ProviderWritesFull
	slug := "credit-purchase-" + uuid.NewString()[:8]
	cfg.Merchant = openrails.MerchantDeclaration{Slug: slug, DisplayName: slug, PSPs: map[string]openrails.PSPConfig{"stripe": {Rail: "stripe", AccountID: "acct_e2e", Secrets: map[string]string{"secret_key": "sk_test_e2e", "webhook_signing_secret": "whsec_e2e"}}}}
	client, err := openrails.New(t.Context(), cfg, openrails.Deps{Postgres: f.pool, StripeTransport: provider})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, client.Close(context.Background())) })
	ctx := t.Context()
	face := int64(2_000_000)
	product, err := client.CreateProduct(ctx, billing.CreateProductParams{Key: "api-credit", DisplayName: "API credit", CreditGrant: &catalog.CreditGrantSpec{Currency: "USD", Amount: &face}})
	require.NoError(t, err)
	price, err := client.CreatePrice(ctx, billing.CreatePriceParams{ProductID: product.ID, Key: "pack", Currency: "USD", UnitAmount: 1_000_000})
	require.NoError(t, err)
	customer := billing.CustomerID(uuid.New())
	_, err = client.EnsureCustomers(ctx, []billing.EnsureCustomerParams{{ID: customer}})
	require.NoError(t, err)
	buy := func(key string) {
		t.Helper()
		_, err := client.CreateCheckoutAttempt(ctx, billing.CreateCheckoutAttemptParams{Customer: billing.CheckoutCustomerIdentity{ID: customer, VerifiedEmail: "api@example.test"}, PriceID: price.ID, IdempotencyKey: key, PaymentOptions: billing.CheckoutPaymentOptions{PSP: "stripe"}, SuccessURL: "https://e2e.test/success", CancelURL: "https://e2e.test/cancel"})
		require.NoError(t, err)
	}
	buy("pack-1")
	face, days := int64(3_000_000), 2
	_, err = client.UpdateProduct(ctx, product.ID, billing.UpdateProductParams{CreditGrant: catalog.Value(catalog.CreditGrantSpec{Currency: "USD", Amount: &face, ExpiresAfterDays: &days})})
	require.NoError(t, err)
	mux := http.NewServeMux()
	require.NoError(t, openrailshttp.Mount(mux, client, openrails.Routes{}))
	deliver := func(raw []byte) {
		t.Helper()
		status, body := postSignedStripeWebhook(t, mux, "acct_e2e", "whsec_e2e", raw, time.Now())
		require.Equal(t, http.StatusOK, status, body)
	}
	// Different webhook event IDs can report the same charge concurrently.
	// Only the payment's stable identity may create spendable credit.
	type response struct {
		status int
		body   string
	}
	answers := make(chan response, 4)
	for n := range 4 {
		payload := provider.paid(t, 1, fmt.Sprintf("evt_credit_first_%d", n))
		go func() {
			status, body := postSignedStripeWebhook(t, mux, "acct_e2e", "whsec_e2e", payload, time.Now())
			answers <- response{status, body}
		}()
	}
	for range 4 {
		answer := <-answers
		require.Equal(t, http.StatusOK, answer.status, answer.body)
	}
	deliver(provider.paid(t, 1, "evt_credit_duplicate"))
	lots, err := client.ListCreditGrants(ctx, customer, billing.CreditGrantListParams{})
	require.NoError(t, err)
	require.Len(t, lots.Items, 1)
	first := lots.Items[0]
	require.EqualValues(t, 2_000_000, first.Amount, "accepted benefit survives product edit")
	require.NotNil(t, first.ExpiresAt)
	require.True(t, first.ExpiresAt.Equal(first.StartsAt.AddDate(0, 0, 365)), "omitted expiry means one year from fulfillment")
	require.Equal(t, "purchase", first.SourceType)
	balance, err := client.GetBalance(ctx, customer, "USD")
	require.NoError(t, err)
	require.EqualValues(t, 2_000_000, balance.BalanceAmount)
	access, err := client.CheckProductAccess(ctx, customer, billing.CheckProductAccessParams{ProductIDs: []billing.ProductID{product.ID}})
	require.NoError(t, err)
	require.False(t, access[product.ID.String()], "a purchased credit pack is not permanent ownership")

	buy("pack-2")
	deliver(provider.paid(t, 2, "evt_credit_second"))
	lots, err = client.ListCreditGrants(ctx, customer, billing.CreditGrantListParams{})
	require.NoError(t, err)
	require.Len(t, lots.Items, 2)
	for _, lot := range lots.Items {
		if lot.ID == first.ID {
			continue
		}
		require.EqualValues(t, 3_000_000, lot.Amount)
		require.True(t, lot.ExpiresAt.Equal(lot.StartsAt.AddDate(0, 0, 2)))
	}
	balance, err = client.GetBalance(ctx, customer, "USD")
	require.NoError(t, err)
	require.EqualValues(t, 5_000_000, balance.BalanceAmount)

	refund, err := json.Marshal(map[string]any{"id": "evt_credit_refund", "type": "refund.created", "created": time.Now().Unix(), "data": map[string]any{"object": map[string]any{"object": "refund", "id": "re_credit_first", "amount": 100, "currency": "usd", "status": "succeeded", "payment_intent": "pi_credit_1"}}})
	require.NoError(t, err)
	deliver(refund)
	deliver(provider.paid(t, 1, "evt_credit_after_refund"))
	balance, err = client.GetBalance(ctx, customer, "USD")
	require.NoError(t, err)
	require.EqualValues(t, 3_000_000, balance.BalanceAmount, "refund removes only its original lot; late success cannot regrant")

	// A later dispute recovery cannot restore an earlier voluntary refund.
	// Half of pack 2 is refunded, the other half consumed, then that consumed
	// half is charged back and the dispute won. No credit becomes available.
	providerEvent := func(id, kind string, object map[string]any) {
		t.Helper()
		raw, err := json.Marshal(map[string]any{"id": id, "type": kind, "created": time.Now().Unix(), "data": map[string]any{"object": object}})
		require.NoError(t, err)
		deliver(raw)
	}
	providerEvent("evt_partial_credit_refund", "refund.created", map[string]any{"object": "refund", "id": "re_credit_second", "amount": 50, "currency": "usd", "status": "succeeded", "payment_intent": "pi_credit_2"})
	_, err = recordUsage(ctx, client, billing.RecordUsageParams{CustomerID: customer, Invoker: customer.String(), Currency: "USD", EventType: "api", Amount: 1_500_000, Source: "api", SourceID: "consume-remaining-credit"})
	require.NoError(t, err)
	providerEvent("evt_credit_disputed", "charge.dispute.created", map[string]any{"object": "dispute", "id": "dp_credit_second", "amount": 50, "currency": "usd", "status": "needs_response", "payment_intent": "pi_credit_2"})
	providerEvent("evt_credit_dispute_won", "charge.dispute.closed", map[string]any{"object": "dispute", "id": "dp_credit_second", "amount": 50, "currency": "usd", "status": "won", "payment_intent": "pi_credit_2"})
	balance, err = client.GetBalance(ctx, customer, "USD")
	require.NoError(t, err)
	require.Zero(t, balance.BalanceAmount, "winning a spent-credit dispute never restores credit from an unrelated refund")
}
