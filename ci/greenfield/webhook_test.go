//go:build greenfield && integration

package greenfield_test

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
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails"
	openrailshttp "github.com/open-rails/openrails/adapters/http"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/embed"
	"github.com/stretchr/testify/require"
)

// stripeCheckoutFake is the public provider seam used by the webhook slice.
// It records the checkout metadata that production sends to Stripe and returns
// a deterministic hosted-session id; the test then delivers those facts back
// through the public webhook route.
type stripeCheckoutFake struct {
	mu       sync.Mutex
	checkout url.Values
}

func (f *stripeCheckoutFake) RoundTrip(r *http.Request) (*http.Response, error) {
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/v1/account":
		return jsonResponse(`{"object":"account","id":"acct_greenfield"}`), nil
	case r.Method == http.MethodGet && r.URL.Path == "/v1/balance":
		return jsonResponse(`{"object":"balance","livemode":false}`), nil
	case r.Method == http.MethodPost && r.URL.Path == "/v1/checkout/sessions":
		if err := r.ParseForm(); err != nil {
			return nil, fmt.Errorf("parse fake checkout form: %w", err)
		}
		f.mu.Lock()
		f.checkout = make(url.Values, len(r.PostForm))
		for key, values := range r.PostForm {
			f.checkout[key] = append([]string(nil), values...)
		}
		f.mu.Unlock()
		return jsonResponse(`{"id":"cs_greenfield_webhook","url":"https://checkout.stripe.test/greenfield"}`), nil
	default:
		return nil, fmt.Errorf("unexpected fake Stripe request: %s %s", r.Method, r.URL.Path)
	}
}

func jsonResponse(body string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": {"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func (f *stripeCheckoutFake) metadata(t *testing.T) (providerSessionID, checkoutSessionID, userID, priceID string) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	require.NotNil(t, f.checkout)
	return "cs_greenfield_webhook", f.checkout.Get("metadata[checkout_session_id]"), f.checkout.Get("metadata[user_id]"), f.checkout.Get("metadata[internal_price_id]")
}

func stripeWebhookBody(t *testing.T, eventID, eventType, providerSessionID, checkoutSessionID, userID, priceID string, created int64) []byte {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"id": eventID, "type": eventType, "created": created,
		"data": map[string]any{"object": map[string]any{
			"id": providerSessionID, "mode": "payment", "status": "complete", "payment_status": "paid",
			"payment_intent": "pi_greenfield_webhook", "amount_total": 100, "currency": "usd",
			"metadata": map[string]string{"checkout_session_id": checkoutSessionID, "user_id": userID, "internal_price_id": priceID},
		}},
	})
	require.NoError(t, err)
	return body
}

func postSignedStripeWebhook(t *testing.T, handler http.Handler, account, secret string, eventID string, payload []byte, at time.Time) (int, string) {
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

// TestStripeWebhookReplayAndReorderingConverges exercises only public seams:
// the embedded Client creates a hosted Stripe checkout through a host supplied
// transport, then the mounted net/http webhook route receives completion,
// replay, and a stale provider-closure event. The final public checkout read
// must remain succeeded and the purchase must remain idempotent.
func TestStripeWebhookReplayAndReorderingConverges(t *testing.T) {
	f := newFixture(t)
	fake := &stripeCheckoutFake{}
	const secret = "whsec_greenfield_webhook"
	const account = "acct_greenfield_webhook"
	slug := "webhook-" + uuid.NewString()[:8]
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
		Key:              "webhook-product-" + uuid.NewString()[:8],
		DisplayName:      "Webhook product",
		EntitlementsSpec: map[string]*int{"content:webhook": nil},
	})
	require.NoError(t, err)
	price, err := client.Prices.Create(t.Context(), &openrails.PriceCreateParams{
		ProductID:  product.ID,
		Key:        "webhook-price-" + uuid.NewString()[:8],
		UnitAmount: 1_000_000,
		Currency:   "USD",
	})
	require.NoError(t, err)
	userID := uuid.NewString()
	_, err = client.EnsureCustomer(t.Context(), userID)
	require.NoError(t, err)
	session, err := client.CreateCheckoutSession(t.Context(), openrails.CreateCheckoutSessionRequest{
		Customer:       openrails.CheckoutCustomerIdentity{ID: userID, VerifiedEmail: "webhook@example.test"},
		PriceID:        price.ID,
		Entitlement:    "content:webhook",
		OfferKind:      openrails.OfferPermanent,
		PaymentOptions: openrails.CheckoutPaymentOptions{Rail: "stripe"},
		IdempotencyKey: "greenfield-webhook-" + uuid.NewString(),
		SuccessURL:     "https://example.test/success",
		CancelURL:      "https://example.test/cancel",
	})
	require.NoError(t, err)
	require.NotNil(t, session)
	providerSessionID, checkoutSessionID, metadataUserID, metadataPriceID := fake.metadata(t)
	require.Equal(t, userID, metadataUserID)
	require.Equal(t, strings.TrimPrefix(price.ID, "price_"), metadataPriceID)
	require.Equal(t, strings.TrimPrefix(session.ID, "cs_"), strings.TrimPrefix(checkoutSessionID, "cs_"))

	bundle, err := openrailshttp.Routes(runtime)
	require.NoError(t, err)
	mux := http.NewServeMux()
	require.NoError(t, bundle.Mount(mux))
	now := time.Now()
	completed := stripeWebhookBody(t, "evt_greenfield_completed", "checkout.session.completed", providerSessionID, checkoutSessionID, userID, metadataPriceID, now.Unix())
	status, body := postSignedStripeWebhook(t, mux, account, secret, "evt_greenfield_completed", completed, now)
	require.Equal(t, http.StatusOK, status, body)

	// A second delivery with a different event id exercises purchase
	// idempotency independently from the webhook-event deduplication key.
	replay := stripeWebhookBody(t, "evt_greenfield_replay", "checkout.session.completed", providerSessionID, checkoutSessionID, userID, metadataPriceID, now.Add(time.Second).Unix())
	status, body = postSignedStripeWebhook(t, mux, account, secret, "evt_greenfield_replay", replay, now.Add(time.Second))
	require.Equal(t, http.StatusOK, status, body)

	// Stripe may deliver an older expiration after completion. It must not
	// replace the succeeded terminal state.
	expired := stripeWebhookBody(t, "evt_greenfield_expired", "checkout.session.expired", providerSessionID, checkoutSessionID, userID, metadataPriceID, now.Add(-time.Minute).Unix())
	status, body = postSignedStripeWebhook(t, mux, account, secret, "evt_greenfield_expired", expired, now.Add(2*time.Second))
	require.Equal(t, http.StatusOK, status, body)

	got, err := client.GetCheckoutSession(t.Context(), userID, session.ID)
	require.NoError(t, err)
	require.Equal(t, "succeeded", got.Status)
	access, err := client.CheckEntitlements(t.Context(), userID, []string{"content:webhook"}, time.Time{})
	require.NoError(t, err)
	require.True(t, access["content:webhook"])
}
