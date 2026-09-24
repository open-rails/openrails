//go:build integration

package integrationharness

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/config"
	orauthkit "github.com/open-rails/openrails/internal/hostauth"
	embcp "github.com/open-rails/openrails/internal/operator"
	"github.com/open-rails/openrails/pkg/billingauth"
	"github.com/stretchr/testify/require"
)

// A one-time purchase on Stripe runs in the page: the card is saved with an
// in-page SetupIntent, then checkout charges that saved card through the
// engine PaymentIntent (customer present). Real self HTTP/session/runtime/
// ledger path with a deterministic Stripe transport.
func TestStripeOneTimeSaleSelfHTTP(t *testing.T) {
	for _, tc := range []struct {
		name     string
		first    string // PaymentIntent outcome of the first attempt
		reason   string
		field    string
		attempts int
	}{
		{name: "saved card paid", first: "succeeded", attempts: 1},
		{name: "3-D Secure in page", first: "requires_action", attempts: 1},
		{name: "incorrect cvc then retry", first: "incorrect_cvc", reason: "incorrect_cvc", field: "cvc", attempts: 2},
		{name: "fraud decline masked then retry", first: "stolen_card", reason: "generic_decline", attempts: 2},
	} {
		t.Run(tc.name, func(t *testing.T) { stripeOneTimeSale(t, tc.first, tc.reason, tc.field, tc.attempts) })
	}
}

func stripeOneTimeSale(t *testing.T, first, reason, field string, attempts int) {
	h := New(t, t.Context())
	var mu sync.Mutex
	var setup map[string]any
	payments := map[string]map[string]any{}
	creates, cancels, paid := 0, 0, map[string]bool{}
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		write := func(v any) { require.NoError(t, json.NewEncoder(w).Encode(v)) }
		metadata := func() map[string]string {
			require.NoError(t, r.ParseForm())
			out := map[string]string{}
			for k, vs := range r.PostForm {
				if strings.HasPrefix(k, "metadata[") {
					out[strings.TrimSuffix(strings.TrimPrefix(k, "metadata["), "]")] = vs[0]
				}
			}
			return out
		}
		switch {
		case r.URL.Path == "/v1/balance":
			write(map[string]any{"object": "balance", "available": []any{}, "pending": []any{}})
		case r.Method == "GET" && r.URL.Path == "/v1/customers/search":
			write(map[string]any{"data": []any{}})
		case r.Method == "POST" && r.URL.Path == "/v1/customers":
			write(map[string]any{"id": "cus_buyer"})
		case r.Method == "POST" && r.URL.Path == "/v1/setup_intents":
			setup = map[string]any{"id": "seti_buyer", "status": "succeeded", "customer": "cus_buyer", "payment_method": "pm_buyer", "usage": "off_session", "payment_method_types": []string{"card"}, "livemode": false, "metadata": metadata(), "client_secret": "seti_buyer_secret_private"}
			write(setup)
		case r.Method == "GET" && r.URL.Path == "/v1/setup_intents/seti_buyer":
			write(setup)
		case r.Method == "GET" && r.URL.Path == "/v1/payment_methods/pm_buyer":
			write(map[string]any{"id": "pm_buyer", "type": "card", "customer": "cus_buyer", "livemode": false, "card": map[string]any{"last4": "4242", "brand": "visa", "exp_month": 12, "exp_year": 2035}})
		case r.Method == "POST" && r.URL.Path == "/v1/payment_intents":
			creates++
			meta := metadata()
			require.Equal(t, "true", meta["openrails_one_time"])
			require.Equal(t, "false", r.PostForm.Get("off_session"), "the buyer is present")
			require.Empty(t, r.PostForm.Get("setup_future_usage"), "the card is already saved")
			require.Equal(t, "pm_buyer", r.PostForm.Get("payment_method"))
			id := "pi_sale" + string(rune('0'+creates))
			pi := map[string]any{"object": "payment_intent", "id": id, "status": "succeeded", "customer": "cus_buyer", "payment_method": "pm_buyer", "amount": 499, "amount_received": 499, "currency": "usd", "capture_method": "automatic", "confirmation_method": "automatic", "livemode": false, "metadata": meta, "client_secret": id + "_secret_private", "latest_charge": "ch_" + strings.TrimPrefix(id, "pi_")}
			payments[id] = pi
			if creates == 1 && first != "succeeded" {
				pi["amount_received"] = 0
				pi["status"] = first
				if first != "requires_action" {
					pi["status"] = "requires_payment_method"
					pi["payment_method"] = nil
					pi["last_payment_error"] = map[string]any{"code": "card_declined", "decline_code": first, "payment_method": "pm_buyer"}
					w.WriteHeader(http.StatusPaymentRequired)
					write(map[string]any{"error": map[string]any{"type": "card_error", "code": "card_declined", "decline_code": first, "payment_intent": pi}})
					return
				}
			}
			write(pi)
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/cancel"):
			cancels++
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/payment_intents/"), "/cancel")
			// Stripe clears the method and the last error when it cancels.
			payments[id]["status"], payments[id]["payment_method"], payments[id]["last_payment_error"] = "canceled", nil, nil
			write(payments[id])
		case r.Method == "GET" && r.URL.Path == "/v1/payment_intents":
			list := []any{}
			for _, pi := range payments {
				list = append(list, pi)
			}
			write(map[string]any{"data": list, "has_more": false})
		case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/v1/payment_intents/"):
			id := strings.TrimPrefix(r.URL.Path, "/v1/payment_intents/")
			pi := payments[id]
			if paid[id] {
				pi["status"], pi["amount_received"] = "succeeded", 499
			}
			write(pi)
		case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/v1/charges/"):
			id := strings.TrimPrefix(r.URL.Path, "/v1/charges/ch_")
			write(map[string]any{"id": "ch_" + id, "payment_intent": "pi_" + id, "customer": "cus_buyer", "payment_method": "pm_buyer", "amount": 499, "amount_captured": 499, "currency": "usd", "status": "succeeded", "paid": true, "captured": true})
		default:
			t.Errorf("unexpected Stripe route: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(500)
			write(map[string]string{"error": "unexpected"})
		}
	}))
	defer gateway.Close()

	var host billingauth.DelegatedAuthenticator
	surface := h.StartStandalone("USD", WithConfig(func(c *config.Config) {
		c.ProviderSandbox = &config.ProviderSandboxConfig{StripeAPIURL: gateway.URL}
		c.ProviderWriteMode = config.ProviderWriteModeFull
	}), func(c *standaloneConfig) {
		c.delegatedAuthenticator = billingauth.DelegatedAuthenticatorFunc(func(ctx context.Context, r *http.Request) (*billingauth.DelegatedPrincipal, error) {
			return host.AuthenticateDelegated(ctx, r)
		})
	})
	owned := surface.ProvisionOwnedMerchant("stripe-sale-" + uuid.NewString()[:8])
	cp, rt := embcp.Get(surface.App()), surface.App().Runtime
	var err error
	host, err = orauthkit.NewDelegatedAuthenticator(cp.AuthService().Verifier(), owned.MerchantID.String())
	require.NoError(t, err)
	owner := surface.Client(openrails.WithAPIKey(owned.APIKey), openrails.WithMerchantID(owned.MerchantID))
	psp := h.ArmLoopbackStripe(rt, owned.MerchantID)
	pool := h.MerchantPool(owned.MerchantID.UUID())
	_, err = pool.Exec(t.Context(), `UPDATE billing.psps SET evidence=coalesce(evidence,'{}'::jsonb)||jsonb_build_object('settings',coalesce(evidence->'settings','{}'::jsonb)||'{"publishable_key":"pk_test_buyer"}'::jsonb) WHERE id=$1`, psp)
	require.NoError(t, err)

	// Browser discovery: Stripe with a publishable key is driven in the page,
	// and it takes new checkouts.
	checkoutConfig, err := owner.GetCheckoutConfig(t.Context())
	require.NoError(t, err)
	require.Len(t, checkoutConfig.PSPs, 1)
	require.Equal(t, "elements", checkoutConfig.PSPs[0].Flow)
	require.True(t, checkoutConfig.PSPs[0].Checkout)

	product, err := owner.Products.Create(t.Context(), &openrails.ProductCreateParams{Key: uuid.NewString(), DisplayName: "Post", EntitlementsSpec: map[string]*int{"post_access": nil}})
	require.NoError(t, err)
	price, err := owner.Prices.Create(t.Context(), &openrails.PriceCreateParams{ProductID: product.ID, UnitAmount: 4_990_000, Currency: "USD"})
	require.NoError(t, err)
	user, err := cp.Core().CreateUser(t.Context(), uuid.NewString()+"@example.test", "buyer"+uuid.NewString()[:8])
	require.NoError(t, err)
	token, _, err := cp.Core().MintAccessToken(t.Context(), user.ID, nil)
	require.NoError(t, err)
	_, err = owner.EnsureCustomer(t.Context(), openrails.CustomerID(uuid.MustParse(user.ID)).String())
	require.NoError(t, err)
	send := func(method, path, key string, body any) (int, map[string]any) {
		var data bytes.Buffer
		if body != nil {
			require.NoError(t, json.NewEncoder(&data).Encode(body))
		}
		req, err := http.NewRequestWithContext(t.Context(), method, surface.BaseURL+"/v1/me"+path, &data)
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		if key != "" {
			req.Header.Set("Idempotency-Key", key)
		}
		response, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer response.Body.Close()
		raw, err := io.ReadAll(response.Body)
		require.NoError(t, err)
		var envelope map[string]any
		require.NoError(t, json.Unmarshal(raw, &envelope), "%s %s: %s", method, path, raw)
		return response.StatusCode, envelope
	}
	call := func(method, path, key string, body any) map[string]any {
		status, envelope := send(method, path, key, body)
		require.True(t, status >= 200 && status < 300, "%s %s: %v", method, path, envelope)
		return envelope
	}

	setupAction := call("POST", "/payment-methods/stripe-setup", "setup-"+uuid.NewString(), map[string]any{"psp_id": psp, "consent": true})
	method := call("POST", "/payment-methods/stripe-setup/"+setupAction["id"].(string)+"/confirm", "", nil)
	saved := call("GET", "/payment-methods", "", nil)["data"].([]any)
	require.Len(t, saved, 1)
	card := saved[0].(map[string]any)["card"].(map[string]any)
	require.Equal(t, "4242", card["last4"])
	require.NotEmpty(t, card["brand"])

	buyWith := func(key string) (int, map[string]any) {
		return send("POST", "/checkout", key, map[string]any{"price_id": price.ID, "payment": map[string]any{"psp_id": psp, "rail": "stripe", "payment_method_id": method["payment_method_id"]}})
	}
	buy := func() map[string]any {
		status, session := buyWith("buy-" + uuid.NewString())
		require.True(t, status >= 200 && status < 300, "%v", session)
		return session
	}
	switch first {
	case "succeeded", "requires_action":
	default:
		key := "buy-" + uuid.NewString()
		status, refusal := buyWith(key)
		require.Equal(t, http.StatusPaymentRequired, status, "a definite decline is a card refusal: %v", refusal)
		errorBody := refusal["error"].(map[string]any)
		require.Equal(t, "card_declined", errorBody["code"])
		failure := errorBody["metadata"].(map[string]any)["failure"].(map[string]any)
		require.Equal(t, reason, failure["reason"])
		require.Equal(t, failure["message"], errorBody["message"])
		if field != "" {
			require.Equal(t, field, failure["field"])
		} else {
			require.Nil(t, failure["field"])
		}
		mu.Lock()
		require.Equal(t, 1, cancels, "the declined PaymentIntent is closed")
		mu.Unlock()
		require.Equal(t, "succeeded", buy()["status"], "a definite decline never blocks a new attempt")
		history := call("GET", "/payments", "", nil)["data"].([]any)
		var failed map[string]any
		for _, row := range history {
			if row.(map[string]any)["status"] == "failed" {
				failed = row.(map[string]any)
			}
		}
		require.NotNil(t, failed)
		require.Equal(t, reason, failed["failure"].(map[string]any)["reason"])
	}
	if first != "succeeded" && first != "requires_action" {
		var completed int
		require.NoError(t, pool.QueryRow(t.Context(), `SELECT count(*) FROM billing.payments WHERE customer_id=$1 AND status='completed'`, user.ID).Scan(&completed))
		require.Equal(t, 1, completed, "exactly one captured purchase")
		mu.Lock()
		require.Equal(t, attempts, creates)
		mu.Unlock()
		return
	}
	session := buy()
	require.Empty(t, session["url"], "no hosted Checkout redirect")
	switch first {
	case "succeeded":
		require.Equal(t, "succeeded", session["status"])
	case "requires_action":
		require.Equal(t, "requires_action", session["status"])
		operation := session["operation"].(map[string]any)
		challenge := call("GET", "/payment-operations/"+operation["id"].(string)+"/authentication", "", nil)
		require.Equal(t, "pi_sale1_secret_private", challenge["client_secret"])
		mu.Lock()
		paid["pi_sale1"] = true
		mu.Unlock()
		require.Equal(t, "succeeded", call("POST", "/payment-operations/"+operation["id"].(string)+"/authentication/confirm", "", nil)["status"])
		require.Equal(t, "succeeded", call("GET", "/checkout/"+session["id"].(string), "", nil)["status"])
	}
	var completed int
	require.NoError(t, pool.QueryRow(t.Context(), `SELECT count(*) FROM billing.payments WHERE customer_id=$1 AND status='completed'`, user.ID).Scan(&completed))
	require.Equal(t, 1, completed, "exactly one captured purchase")
	mu.Lock()
	require.Equal(t, attempts, creates)
	mu.Unlock()
}
