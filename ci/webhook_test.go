//go:build e2e && integration

package ci_test

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails"
	openrailshttp "github.com/open-rails/openrails/adapters/http"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/billingauth/authtest"
	"github.com/stretchr/testify/require"
)

// stripeCheckoutFake records the purchase metadata sent to Stripe and returns
// a deterministic hosted-session id for the test to deliver back as webhooks.
type stripeCheckoutFake struct {
	t             *testing.T
	purchaseCalls atomic.Int32
	mu            sync.Mutex
	purchase      url.Values
}

func (f *stripeCheckoutFake) RoundTrip(r *http.Request) (*http.Response, error) {
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/v1/account":
		return jsonResponse(`{"object":"account","id":"acct_e2e"}`), nil
	case r.Method == http.MethodGet && r.URL.Path == "/v1/balance":
		return jsonResponse(`{"object":"balance","livemode":false}`), nil
	case r.Method == http.MethodPost && r.URL.Path == "/v1/checkout/sessions":
		if err := r.ParseForm(); err != nil {
			return nil, fmt.Errorf("parse fake purchase form: %w", err)
		}
		f.purchaseCalls.Add(1)
		// Check actual outgoing money, not a fixed success-shaped response.
		for key, want := range map[string]string{
			"mode": "payment", "line_items[0][price_data][currency]": "usd",
			"line_items[0][price_data][unit_amount]": "100", "line_items[0][quantity]": "1",
		} {
			if got := r.PostForm.Get(key); got != want {
				f.t.Errorf("Stripe form %s = %q, want %q", key, got, want)
				return nil, fmt.Errorf("invalid Stripe checkout %s", key)
			}
		}
		f.mu.Lock()
		f.purchase = make(url.Values, len(r.PostForm))
		for key, values := range r.PostForm {
			f.purchase[key] = append([]string(nil), values...)
		}
		f.mu.Unlock()
		return jsonResponse(`{"id":"cs_e2e_webhook","url":"https://checkout.stripe.test/e2e"}`), nil
	default:
		f.t.Errorf("unexpected fake Stripe request: %s %s", r.Method, r.URL.Path)
		return nil, fmt.Errorf("unexpected fake Stripe request: %s %s", r.Method, r.URL.Path)
	}
}

// cid and pid read test ids as their typed form.
func cid(id string) billing.CustomerID { return billing.CustomerID(uuid.MustParse(id)) }

func pid(id string) billing.PriceID {
	parsed, err := billing.ParsePriceID(id)
	if err != nil {
		panic(err)
	}
	return parsed
}

func jsonResponse(body string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": {"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func (f *stripeCheckoutFake) metadata(t *testing.T) (providerSessionID, orderAttemptID, userID, orderID string) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	require.NotNil(t, f.purchase)
	return "cs_e2e_webhook", f.purchase.Get("metadata[order_attempt_id]"), f.purchase.Get("metadata[user_id]"), f.purchase.Get("metadata[order_id]")
}

func stripeWebhookBody(t *testing.T, eventID, eventType, providerSessionID, orderAttemptID, userID, orderID string, created int64) []byte {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"id": eventID, "type": eventType, "created": created,
		"data": map[string]any{"object": map[string]any{
			"id": providerSessionID, "mode": "payment", "status": "complete", "payment_status": "paid",
			"payment_intent": "pi_e2e_webhook", "amount_total": 100, "currency": "usd",
			"metadata": map[string]string{"order_attempt_id": orderAttemptID, "user_id": userID, "order_id": orderID},
		}},
	})
	require.NoError(t, err)
	return body
}

func postSignedStripeWebhook(t *testing.T, handler http.Handler, account, secret string, payload []byte, at time.Time) (int, string) {
	t.Helper()
	timestamp := strconv.FormatInt(at.Unix(), 10)
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(timestamp + "."))
	_, _ = mac.Write(payload)
	req := httptest.NewRequest(http.MethodPost, "/v1/webhooks/stripe/"+account, bytes.NewReader(payload))
	req.Header.Set("Stripe-Signature", "t="+timestamp+",v1="+hex.EncodeToString(mac.Sum(nil)))
	req.Header.Set("Content-Type", "application/json")
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	return res.Code, res.Body.String()
}

// TestStripeWebhookReplayAndReorderingConverges pays a purchase through
// Stripe's hosted page, then delivers completion, a replay and a stale expiry
// to the webhook route: the purchase stays succeeded and pays once.
func TestStripeWebhookReplayAndReorderingConverges(t *testing.T) {
	f := newFixture(t)
	fake := &stripeCheckoutFake{t: t}
	const secret = "whsec_e2e_webhook"
	const account = "acct_e2e_webhook"
	slug := "webhook-" + uuid.NewString()[:8]
	cfg := f.config()
	cfg.ProviderWriteMode = openrails.ProviderWritesFull
	cfg.ReturnOrigins = []string{"https://example.test"}
	// Two merchants on one engine, each with its own Stripe account: a
	// webhook reaches the merchant whose PSP its path names and whose secret
	// signed it.
	const otherSecret, otherAccount = "whsec_e2e_webhook_other", "acct_e2e_webhook_other"
	otherSlug := "webhook-other-" + uuid.NewString()[:8]
	cfg.Merchants = map[string]openrails.MerchantDeclaration{
		slug: {DisplayName: slug, Issuer: authtest.Issuer, PSPs: map[string]openrails.PSPConfig{"stripe": {Rail: "stripe", Environment: "test",
			AccountID: account,
			Secrets:   map[string]string{"secret_key": "sk_test_e2e", "webhook_signing_secret": secret},
		}}},
		otherSlug: {DisplayName: otherSlug, PSPs: map[string]openrails.PSPConfig{"stripe": {Rail: "stripe", Environment: "test",
			AccountID: otherAccount,
			Secrets:   map[string]string{"secret_key": "sk_test_e2e_other", "webhook_signing_secret": otherSecret},
		}}},
	}
	bill, err := openrails.New(t.Context(), cfg, openrails.Deps{FXTransport: testFX.Transport(), Postgres: f.pool, StripeTransport: fake})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, bill.Close(context.Background())) })
	client, err := bill.MerchantBySlug(slug)
	require.NoError(t, err)
	other, err := bill.MerchantBySlug(otherSlug)
	require.NoError(t, err)

	product, err := client.CreateProduct(t.Context(), billing.CreateProductParams{
		Key:          "webhook-product-" + uuid.NewString()[:8],
		DisplayName:  "Webhook product",
		Entitlements: []string{"content:webhook"},
	})
	require.NoError(t, err)
	price, err := client.CreatePrice(t.Context(), billing.CreatePriceParams{
		ProductID:  product.ID,
		Key:        "webhook-price-" + uuid.NewString()[:8],
		UnitAmount: 1_000_000,
		Currency:   "USD",
	})
	require.NoError(t, err)
	userID := uuid.NewString()
	session, err := sell(t, bill, client, sale{customer: cid(userID), price: price.ID})
	require.NoError(t, err)
	_, err = session.pay("pay-1", redirect("https://example.test/success"))
	require.NoError(t, err)
	providerSessionID, orderAttemptID, metadataUserID, metadataOrderID := fake.metadata(t)
	require.Equal(t, userID, metadataUserID)
	require.Equal(t, session.order.ID.String(), metadataOrderID)
	require.Equal(t, strings.TrimPrefix(session.attempt(f).String(), "oat_"), strings.TrimPrefix(orderAttemptID, "oat_"))

	mux := http.NewServeMux()
	require.NoError(t, openrailshttp.Mount(mux, bill, openrails.Routes{Auth: authtest.Deny{}}))
	now := time.Now()
	completed := stripeWebhookBody(t, "evt_e2e_completed", "checkout.session.completed", providerSessionID, orderAttemptID, userID, metadataOrderID, now.Unix())
	status, body := postSignedStripeWebhook(t, mux, account, secret, completed, now)
	require.Equal(t, http.StatusOK, status, body)
	// Exact redelivery exercises the event deduplication key, separately from
	// the distinct event ID below.
	status, body = postSignedStripeWebhook(t, mux, account, secret, completed, now)
	require.Equal(t, http.StatusOK, status, body)

	// A second delivery with a different event id exercises purchase
	// idempotency independently from the webhook-event deduplication key.
	replay := stripeWebhookBody(t, "evt_e2e_replay", "checkout.session.completed", providerSessionID, orderAttemptID, userID, metadataOrderID, now.Add(time.Second).Unix())
	status, body = postSignedStripeWebhook(t, mux, account, secret, replay, now.Add(time.Second))
	require.Equal(t, http.StatusOK, status, body)

	// Stripe may deliver an older expiration after completion. It must not
	// replace the succeeded terminal state.
	expired := stripeWebhookBody(t, "evt_e2e_expired", "checkout.session.expired", providerSessionID, orderAttemptID, userID, metadataOrderID, now.Add(-time.Minute).Unix())
	status, body = postSignedStripeWebhook(t, mux, account, secret, expired, now.Add(2*time.Second))
	require.Equal(t, http.StatusOK, status, body)

	require.Equal(t, billing.OrderComplete, session.read().Status)
	access, err := heldKeys(t.Context(), client, billing.CustomerID(uuid.MustParse(userID)), time.Time{}, "content:webhook")
	require.NoError(t, err)
	require.True(t, access["content:webhook"])
	payments, err := client.ListPayments(t.Context(), billing.PaymentListParams{CustomerID: billing.CustomerID(uuid.MustParse(userID))})
	require.NoError(t, err)
	require.Len(t, payments.Items, 1, "event replay must not duplicate the payment")
	require.EqualValues(t, 1_000_000, payments.Items[0].Amount)
	require.Equal(t, "USD", payments.Items[0].Currency)
	require.Equal(t, "pi_e2e_webhook", payments.Items[0].TransactionID)
	require.EqualValues(t, 1, fake.purchaseCalls.Load(), "webhook handling must not submit another purchase")

	theirs, err := other.ListPayments(t.Context(), billing.PaymentListParams{CustomerID: billing.CustomerID(uuid.MustParse(userID))})
	require.NoError(t, err)
	require.Empty(t, theirs.Items, "the other merchant's books are untouched")
	status, body = postSignedStripeWebhook(t, mux, otherAccount, secret, completed, now.Add(3*time.Second))
	require.Equal(t, http.StatusUnauthorized, status, "the other PSP's path takes only its own secret: %s", body)
}
