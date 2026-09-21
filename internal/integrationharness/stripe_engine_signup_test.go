//go:build integration

package integrationharness

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/config"
	orauthkit "github.com/open-rails/openrails/embed/authkit"
	embcp "github.com/open-rails/openrails/internal/operator"
	"github.com/open-rails/openrails/pkg/billingauth"
	"github.com/stretchr/testify/require"
)

// Real self HTTP/auth/session/runtime/ledger path with a deterministic Stripe
// transport. Card entry and issuer authentication still require sandbox proof.
func TestStripeEngineSignupSelfHTTP(t *testing.T) {
	h := New(t, t.Context())
	var mu sync.Mutex
	var setup, payment map[string]any
	setupPaid, paymentPaid := false, false
	setupCreates, paymentCreates := 0, 0
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
		case r.URL.Path == "/v1/account":
			write(map[string]any{"id": "acct_fixture", "object": "account", "charges_enabled": true})
		case r.Method == "GET" && r.URL.Path == "/v1/customers/search":
			write(map[string]any{"data": []any{}})
		case r.Method == "POST" && r.URL.Path == "/v1/customers":
			write(map[string]any{"id": "cus_signup"})
		case r.Method == "POST" && r.URL.Path == "/v1/setup_intents":
			setupCreates++
			meta := metadata()
			require.Equal(t, "off_session", r.PostForm.Get("usage"))
			setup = map[string]any{"id": "seti_signup", "status": "requires_payment_method", "customer": "cus_signup", "payment_method": "pm_signup", "usage": "off_session", "payment_method_types": []string{"card"}, "livemode": false, "metadata": meta, "client_secret": "seti_signup_secret_private"}
			write(setup)
		case r.Method == "GET" && r.URL.Path == "/v1/setup_intents/seti_signup":
			if setupPaid {
				setup["status"] = "succeeded"
			}
			write(setup)
		case r.Method == "GET" && r.URL.Path == "/v1/payment_methods/pm_signup":
			write(map[string]any{"id": "pm_signup", "type": "card", "customer": "cus_signup", "livemode": false, "card": map[string]any{"last4": "4242", "brand": "visa", "exp_month": 12, "exp_year": 2035}})
		case r.Method == "POST" && r.URL.Path == "/v1/payment_intents":
			paymentCreates++
			meta := metadata()
			require.Equal(t, "999", r.PostForm.Get("amount"))
			require.Equal(t, "off_session", r.PostForm.Get("setup_future_usage"))
			require.Equal(t, "false", r.PostForm.Get("off_session"))
			payment = map[string]any{"id": "pi_signup", "status": "requires_action", "customer": "cus_signup", "payment_method": "pm_signup", "amount": 999, "amount_received": 0, "currency": "usd", "setup_future_usage": "off_session", "capture_method": "automatic", "confirmation_method": "automatic", "livemode": false, "metadata": meta, "client_secret": "pi_signup_secret_private", "latest_charge": "ch_signup"}
			write(payment)
		case r.Method == "GET" && r.URL.Path == "/v1/payment_intents/pi_signup":
			if paymentPaid {
				payment["status"] = "succeeded"
				payment["amount_received"] = 999
			}
			write(payment)
		case r.Method == "GET" && r.URL.Path == "/v1/charges/ch_signup":
			write(map[string]any{"id": "ch_signup", "payment_intent": "pi_signup", "customer": "cus_signup", "payment_method": "pm_signup", "amount": 999, "amount_captured": 999, "currency": "usd", "status": "succeeded", "paid": true, "captured": true})
		default:
			t.Errorf("unexpected Stripe native catalog/schedule or wire route: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(500)
			write(map[string]string{"error": "unexpected wire"})
		}
	}))
	defer gateway.Close()
	var host billingauth.DelegatedAuthenticator
	surface := h.StartStandalone("USD", WithConfig(func(c *config.Config) {
		c.ProviderSandbox = &config.ProviderSandboxConfig{StripeAPIURL: gateway.URL}
		c.ProviderWriteMode = config.ProviderWriteModeFull
		c.NewSubscriptionCollectionPolicy = "engine"
	}), func(c *standaloneConfig) {
		c.delegatedAuthenticator = billingauth.DelegatedAuthenticatorFunc(func(ctx context.Context, r *http.Request) (*billingauth.DelegatedPrincipal, error) {
			return host.AuthenticateDelegated(ctx, r)
		})
	})
	owned := surface.ProvisionOwnedMerchant("stripe-engine-" + uuid.NewString()[:8])
	cp, rt := embcp.Get(surface.App()), surface.App().Runtime
	var err error
	host, err = orauthkit.NewDelegatedAuthenticator(cp.AuthService().Verifier(), owned.MerchantID.String())
	require.NoError(t, err)
	owner := surface.Client(openrails.WithAPIKey(owned.APIKey), openrails.WithMerchantID(owned.MerchantID))
	psp := h.ArmLoopbackStripe(rt, owned.MerchantID)
	product, err := owner.CreateProduct(t.Context(), openrails.CreateProductRequest{Key: uuid.NewString(), DisplayName: "Engine Stripe", EntitlementsSpec: map[string]*int{"engine_access": nil}})
	require.NoError(t, err)
	hours := 720
	price, err := owner.CreatePrice(t.Context(), openrails.CreatePriceRequest{ProductID: product.ID, UnitAmount: 9_990_000, Currency: "USD", AutoRenew: true, AccessDurationHours: &hours})
	require.NoError(t, err)
	user, err := cp.Core().CreateUser(t.Context(), uuid.NewString()+"@example.test", "stripe"+uuid.NewString()[:8])
	require.NoError(t, err)
	token, _, err := cp.Core().MintAccessToken(t.Context(), user.ID, nil)
	require.NoError(t, err)
	_, err = owner.EnsureCustomer(t.Context(), openrails.CustomerID(uuid.MustParse(user.ID)))
	require.NoError(t, err)
	call := func(method, path, key string, body any) map[string]any {
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
		require.True(t, response.StatusCode >= 200 && response.StatusCode < 300, "%s %s: %s", method, path, raw)
		require.Equal(t, "no-store", response.Header.Get("Cache-Control"))
		var envelope map[string]any
		require.NoError(t, json.Unmarshal(raw, &envelope))
		return envelope
	}
	key := "setup-" + uuid.NewString()
	action := call("POST", "/payment-methods/stripe-setup", key, map[string]any{"psp_id": psp, "consent": true})
	require.Equal(t, "seti_signup_secret_private", action["client_secret"])
	replay := call("POST", "/payment-methods/stripe-setup", key, map[string]any{"psp_id": psp, "consent": true})
	require.Equal(t, action["id"], replay["id"])
	mu.Lock()
	setupPaid = true
	mu.Unlock()
	method := call("POST", fmt.Sprintf("/payment-methods/stripe-setup/%s/confirm", action["id"]), "", nil)
	var paid int
	pool := h.MerchantPool(owned.MerchantID.UUID())
	require.NoError(t, pool.QueryRow(t.Context(), `SELECT count(*) FROM billing.payments WHERE customer_id=$1`, user.ID).Scan(&paid))
	require.Zero(t, paid, "setup is not a paid membership")
	quote := call("POST", "/checkout", "enroll-"+uuid.NewString(), map[string]any{"mode": "subscription", "price_id": price.ID, "payment": map[string]any{"psp_id": psp, "rail": "stripe", "payment_method_id": method["payment_method_id"]}})
	require.NotNil(t, quote["membership_quote"])
	require.Nil(t, quote["url"])
	enrollment := call("POST", fmt.Sprintf("/checkout/%s/confirm", quote["id"]), "", map[string]any{"payment": map[string]string{"rail": "stripe"}})
	operation := enrollment["operation"].(map[string]any)
	require.Equal(t, "unknown_needs_verify", operation["status"])
	recovery := call("GET", fmt.Sprintf("/payment-operations/%s/authentication", operation["id"]), "", nil)
	require.Equal(t, "pi_signup_secret_private", recovery["client_secret"])
	mu.Lock()
	paymentPaid = true
	mu.Unlock()
	result := call("POST", fmt.Sprintf("/payment-operations/%s/authentication/confirm", operation["id"]), "", nil)
	require.Equal(t, "succeeded", result["status"])
	completed := call("GET", fmt.Sprintf("/checkout/%s", quote["id"]), "", nil)
	require.Equal(t, "succeeded", completed["status"])
	require.NoError(t, pool.QueryRow(t.Context(), `SELECT count(*) FROM billing.payments WHERE customer_id=$1 AND status='completed'`, user.ID).Scan(&paid))
	require.Equal(t, 1, paid)
	var policy, external string
	require.NoError(t, pool.QueryRow(t.Context(), `SELECT collection_policy,coalesce(rail_subscription_id,'') FROM billing.subscriptions WHERE customer_id=$1`, user.ID).Scan(&policy, &external))
	require.Equal(t, "engine", policy)
	require.Empty(t, external)
	mu.Lock()
	require.Equal(t, 1, setupCreates)
	require.Equal(t, 1, paymentCreates)
	mu.Unlock()
}
