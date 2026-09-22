//go:build integration

package integrationharness

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails/internal/intents"
	riverjobs "github.com/open-rails/openrails/internal/river"
	"github.com/riverqueue/river"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/config"
	orauthkit "github.com/open-rails/openrails/embed/authkit"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/modules/webhooks"
	embcp "github.com/open-rails/openrails/internal/operator"
	"github.com/open-rails/openrails/pkg/billingauth"
	"github.com/open-rails/openrails/pkg/merchant"
	"github.com/stretchr/testify/require"
)

// Real self HTTP/auth/session/runtime/ledger path with a deterministic Stripe
// transport. Card entry and issuer authentication still require sandbox proof.
func TestStripeEngineSignupSelfHTTP(t *testing.T) {
	for _, reversal := range []string{"", "refund", "dispute", "partial_refund"} {
		name := reversal
		if name == "" {
			name = "authentication"
		}
		t.Run(name, func(t *testing.T) { stripeEngineSignupSelfHTTP(t, reversal, "") })
	}
}
func TestStripeEngineCustomerRetrySelfHTTP(t *testing.T) {
	stripeEngineSignupSelfHTTP(t, "", "current")
}
func TestStripeEngineReplacementSelfHTTP(t *testing.T) {
	stripeEngineSignupSelfHTTP(t, "", "replacement")
}
func stripeEngineSignupSelfHTTP(t *testing.T, reversal string, retryMode string) {
	customerRetry := retryMode != ""
	clock := clockwork.NewFakeClockAt(time.Now().UTC().Add(-720*time.Hour - time.Minute).Truncate(time.Second))
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
			setupID, methodID := "seti_signup", "pm_signup"
			if setupCreates > 1 {
				setupID, methodID = "seti_replacement", "pm_replacement"
			}
			setup = map[string]any{"id": setupID, "status": "requires_payment_method", "customer": "cus_signup", "payment_method": methodID, "usage": "off_session", "payment_method_types": []string{"card"}, "livemode": false, "metadata": meta, "client_secret": setupID + "_secret_private"}
			write(setup)
		case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/v1/setup_intents/"):
			require.Equal(t, "/v1/setup_intents/"+setup["id"].(string), r.URL.Path)
			if setupPaid {
				setup["status"] = "succeeded"
			}
			write(setup)
		case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/v1/payment_methods/"):
			write(map[string]any{"id": strings.TrimPrefix(r.URL.Path, "/v1/payment_methods/"), "type": "card", "customer": "cus_signup", "livemode": false, "card": map[string]any{"last4": "4242", "brand": "visa", "exp_month": 12, "exp_year": 2035}})
		case r.Method == "POST" && r.URL.Path == "/v1/payment_intents":
			paymentCreates++
			meta := metadata()
			require.Equal(t, "999", r.PostForm.Get("amount"))
			if meta["openrails_initial"] == "true" {
				require.Equal(t, "off_session", r.PostForm.Get("setup_future_usage"))
				require.Equal(t, "false", r.PostForm.Get("off_session"))
			} else {
				require.Empty(t, r.PostForm.Get("setup_future_usage"))
				if meta["openrails_customer_retry"] == "true" {
					require.Equal(t, "false", r.PostForm.Get("off_session"))
				} else {
					require.Equal(t, "true", r.PostForm.Get("off_session"))
				}
			}
			payment = map[string]any{"id": "pi_signup", "status": "requires_action", "customer": "cus_signup", "payment_method": r.PostForm.Get("payment_method"), "amount": 999, "amount_received": 0, "currency": "usd", "setup_future_usage": "off_session", "capture_method": "automatic", "confirmation_method": "automatic", "livemode": false, "metadata": meta, "client_secret": "pi_signup_secret_private", "latest_charge": "ch_signup"}
			if customerRetry && paymentCreates > 1 {
				id, charge := "pi_renewal", "ch_renewal"
				if paymentCreates > 2 {
					id, charge = "pi_customerretry", "ch_customerretry"
				}
				payment["id"], payment["latest_charge"], payment["client_secret"], payment["setup_future_usage"] = id, charge, id+"_secret_private", ""
				if paymentCreates == 2 {
					payment["status"] = "requires_payment_method"
					payment["last_payment_error"] = map[string]any{"code": "card_declined", "decline_code": "insufficient_funds"}
				}
			}
			if reversal != "" {
				paymentPaid = true
				w.WriteHeader(http.StatusBadGateway)
				write(map[string]any{"error": "simulated accepted payment with lost response"})
				return
			}
			write(payment)
		case r.Method == "GET" && r.URL.Path == "/v1/payment_intents":
			write(map[string]any{"data": []any{payment}, "has_more": false})
		case r.Method == "POST" && strings.HasPrefix(r.URL.Path, "/v1/payment_intents/") && strings.HasSuffix(r.URL.Path, "/cancel"):
			require.Equal(t, "/v1/payment_intents/"+payment["id"].(string)+"/cancel", r.URL.Path)
			payment["status"] = "canceled"
			write(payment)
		case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/v1/payment_intents/"):
			require.Equal(t, "/v1/payment_intents/"+payment["id"].(string), r.URL.Path)
			if paymentPaid {
				payment["status"] = "succeeded"
				payment["amount_received"] = 999
			}
			write(payment)
		case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/v1/charges/"):
			ch := map[string]any{"id": payment["latest_charge"], "payment_intent": payment["id"], "customer": "cus_signup", "payment_method": payment["payment_method"], "amount": 999, "amount_captured": 999, "currency": "usd", "status": "succeeded", "paid": true, "captured": true}
			if reversal == "refund" {
				ch["refunded"] = true
				ch["amount_refunded"] = 999
			}
			if reversal == "partial_refund" {
				ch["amount_refunded"] = 400
			}
			if reversal == "dispute" {
				ch["disputed"] = true
			}
			write(ch)
		default:
			t.Errorf("unexpected Stripe native catalog/schedule or wire route: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(500)
			write(map[string]string{"error": "unexpected wire"})
		}
	}))
	defer gateway.Close()
	var host billingauth.DelegatedAuthenticator
	surface := h.StartStandalone("USD", WithClock(clock), WithConfig(func(c *config.Config) {
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
	_, err = h.MerchantPool(owned.MerchantID.UUID()).Exec(t.Context(), `UPDATE billing.psps SET evidence=jsonb_set(coalesce(evidence,'{}'::jsonb),'{public_config}','{"publishable_key":"pk_test_loopback"}') WHERE id=$1`, psp)
	require.NoError(t, err)
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
		if strings.Contains(path, "checkout") || strings.Contains(path, "authentication") || strings.Contains(path, "stripe-setup") {
			require.Equal(t, "no-store", response.Header.Get("Cache-Control"))
		}
		var envelope map[string]any
		require.NoError(t, json.Unmarshal(raw, &envelope))
		return envelope
	}
	key := "setup-" + uuid.NewString()
	action := call("POST", "/payment-methods/stripe-setup", key, map[string]any{"psp_id": psp, "consent": true})
	require.Equal(t, "seti_signup_secret_private", action["client_secret"])
	require.Equal(t, "pk_test_loopback", action["publishable_key"])
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
	var reversalEvent []byte
	webhookService := &webhooks.StripeWebhookService{DB: rt.DB, PaymentService: rt.PaymentService, SubscriptionService: rt.SubscriptionService, PriceService: rt.PriceService, SubscriptionLifecycleService: rt.SubscriptionLifecycleService, Clock: rt.Clock}
	deliverReversal := func() error {
		return rt.DB.RunInMerchantConn(db.WithPSPID(merchant.WithID(t.Context(), owned.MerchantID), psp), func(ctx context.Context) error { return webhookService.HandleStripeWebhook(ctx, reversalEvent) })
	}
	if reversal == "" {
		recovery := call("GET", fmt.Sprintf("/payment-operations/%s/authentication", operation["id"]), "", nil)
		require.Equal(t, "pi_signup_secret_private", recovery["client_secret"])
		require.Equal(t, "pk_test_loopback", recovery["publishable_key"])
		require.Equal(t, "pm_signup", recovery["provider_payment_method_id"])
		mu.Lock()
		paymentPaid = true
		mu.Unlock()
	} else {
		eventType, status, ref := "refund.created", "succeeded", "re_signup"
		if reversal == "dispute" {
			eventType = "charge.dispute.created"
			status = "needs_response"
			ref = "dp_signup"
		}
		refundAmount := 999
		if reversal == "partial_refund" {
			refundAmount = 400
		}
		reversalEvent, err = json.Marshal(map[string]any{"id": "evt_reversal", "type": eventType, "data": map[string]any{"object": map[string]any{"id": ref, "charge": "ch_signup", "payment_intent": "pi_signup", "amount": refundAmount, "currency": "usd", "status": status, "reason": "fraudulent"}}})
		require.NoError(t, err)
		require.Error(t, deliverReversal(), "out-of-order reversal waits for the original payment")
	}
	result := call("POST", fmt.Sprintf("/payment-operations/%s/authentication/confirm", operation["id"]), "", nil)
	require.Equal(t, "succeeded", result["status"])
	completed := call("GET", fmt.Sprintf("/checkout/%s", quote["id"]), "", nil)
	require.Equal(t, "succeeded", completed["status"])
	sessionID, err := openrails.ParseCheckoutSessionID(quote["id"].(string))
	require.NoError(t, err)
	var persisted string
	require.NoError(t, pool.QueryRow(t.Context(), `SELECT status FROM billing.checkout_sessions WHERE id=$1`, sessionID.UUID()).Scan(&persisted))
	require.Equal(t, "succeeded", persisted, "terminal operation and checkout projection commit together")
	require.NoError(t, pool.QueryRow(t.Context(), `SELECT count(*) FROM billing.payments WHERE customer_id=$1 AND status='completed'`, user.ID).Scan(&paid))
	require.Equal(t, 1, paid)
	var policy, external string
	require.NoError(t, pool.QueryRow(t.Context(), `SELECT collection_policy,coalesce(rail_subscription_id,'') FROM billing.subscriptions WHERE customer_id=$1`, user.ID).Scan(&policy, &external))
	require.Equal(t, "engine", policy)
	require.Empty(t, external)
	if reversal != "" {
		require.NoError(t, deliverReversal())
		require.NoError(t, deliverReversal(), "duplicate reversal converges once")
		var count, grants int
		var status string
		require.NoError(t, pool.QueryRow(t.Context(), `SELECT count(*) FROM billing.payments WHERE customer_id=$1`, user.ID).Scan(&count))
		require.Equal(t, 2, count, "one original and one refund/dispute movement")
		require.NoError(t, pool.QueryRow(t.Context(), `SELECT status FROM billing.subscriptions WHERE customer_id=$1`, user.ID).Scan(&status))
		if reversal == "partial_refund" {
			require.Equal(t, "active", status)
		} else {
			require.Equal(t, "cancelled", status)
		}
		require.NoError(t, pool.QueryRow(t.Context(), `SELECT count(*) FROM billing.grants WHERE customer_id=$1 AND event='grant'`, user.ID).Scan(&grants))
		if reversal == "partial_refund" {
			require.Positive(t, grants, "partial refund preserves existing paid-access policy")
		} else {
			require.Zero(t, grants, "fully reversed signup must never grant access")
		}
		if reversal == "dispute" {
			reversalEvent, err = json.Marshal(map[string]any{"id": "evt_dispute_won", "type": "charge.dispute.closed", "data": map[string]any{"object": map[string]any{"id": "dp_signup", "charge": "ch_signup", "payment_intent": "pi_signup", "amount": 999, "currency": "usd", "status": "won"}}})
			require.NoError(t, err)
			require.NoError(t, deliverReversal())
			require.NoError(t, deliverReversal())
			require.NoError(t, pool.QueryRow(t.Context(), `SELECT count(*) FROM billing.payments WHERE customer_id=$1`, user.ID).Scan(&count))
			require.Equal(t, 3, count)
			require.NoError(t, pool.QueryRow(t.Context(), `SELECT status FROM billing.subscriptions WHERE customer_id=$1`, user.ID).Scan(&status))
			require.Equal(t, "cancelled", status, "won dispute restores money without restarting an engine agreement")
		}
	}
	if customerRetry {
		subscription, err := openrails.ParseSubscriptionID(completed["subscription_id"].(string))
		require.NoError(t, err)
		mu.Lock()
		paymentPaid = false
		mu.Unlock()
		clock.Advance(720*time.Hour + time.Second)
		worker := riverjobs.DunningWorker{DB: rt.DB, Config: rt.Config, Clock: clock, NMIResolver: rt.CollectionResolver, EngineCollections: rt.MoneyService, Intents: rt.IntentRunner()}
		require.NoError(t, worker.Work(t.Context(), &river.Job[riverjobs.DunningArgs]{}))
		var dueID uuid.UUID
		require.NoError(t, pool.QueryRow(t.Context(), `SELECT id FROM billing.rail_intents WHERE subscription_id=$1 AND intent_type='subscription_collection' ORDER BY created_at DESC LIMIT 1`, subscription.UUID()).Scan(&dueID))
		scope := db.WithPSPID(merchant.WithID(t.Context(), owned.MerchantID), psp)
		require.NoError(t, rt.DB.RunInMerchantConn(scope, func(ctx context.Context) error {
			result, err := rt.IntentRunner().ExecuteByID(ctx, dueID)
			require.NoError(t, err)
			require.Equal(t, intents.StatusFailedTerminal, result.Status)
			return err
		}))
		var retryAt *time.Time
		var subStatus string
		require.NoError(t, pool.QueryRow(t.Context(), `SELECT status,next_retry_at FROM billing.subscriptions WHERE id=$1`, subscription.UUID()).Scan(&subStatus, &retryAt))
		require.Equal(t, "past_due", subStatus)
		require.NotNil(t, retryAt)
		require.True(t, retryAt.After(clock.Now()), "customer retry explicitly bypasses future automatic delay")
		customer, err := openrails.NewRemote(surface.BaseURL, openrails.WithMerchantID(owned.MerchantID), openrails.WithTokenProvider(func(context.Context) (string, error) { return token, nil }))
		require.NoError(t, err)
		localMethod, err := openrails.ParsePaymentMethodID(method["payment_method_id"].(string))
		require.NoError(t, err)
		view := call("GET", "/subscriptions/"+subscription.String(), "", nil)
		recoveryView := view["recovery"].(map[string]any)
		require.Equal(t, true, recoveryView["retryable"])
		require.Equal(t, "insufficient_funds", recoveryView["last_failure_reason"])
		replacement := openrails.PaymentMethodID(uuid.New())
		_, err = customer.RetrySubscriptionNow(t.Context(), openrails.RetrySubscriptionNowRequest{SubscriptionID: subscription, IdempotencyKey: "wrong-method-" + uuid.NewString(), PaymentMethodID: &replacement})
		require.Error(t, err)
		if retryMode == "replacement" {
			mu.Lock()
			setupPaid = false
			mu.Unlock()
			replacementAction := call("POST", "/payment-methods/stripe-setup", "new-card-"+uuid.NewString(), map[string]any{"psp_id": psp, "consent": true})
			require.Equal(t, "seti_replacement_secret_private", replacementAction["client_secret"])
			mu.Lock()
			setupPaid = true
			mu.Unlock()
			replacementSaved := call("POST", fmt.Sprintf("/payment-methods/stripe-setup/%s/confirm", replacementAction["id"]), "", nil)
			newMethod, err := openrails.ParsePaymentMethodID(replacementSaved["payment_method_id"].(string))
			require.NoError(t, err)
			var anchor string
			require.NoError(t, pool.QueryRow(t.Context(), `SELECT stored_credential_recurring_ref FROM billing.payment_methods WHERE id=$1`, newMethod.UUID()).Scan(&anchor))
			require.Equal(t, "seti_replacement", anchor)
			_ = call("PUT", "/subscriptions/"+subscription.String()+"/payment-method", "", map[string]any{"payment_method_id": newMethod})
			localMethod = newMethod
		}
		rt.Config.NewSubscriptionCollectionPolicy = "provider" // stored engine ownership survives a new-enrollment default change
		retryRequest := openrails.RetrySubscriptionNowRequest{SubscriptionID: subscription, IdempotencyKey: "retry-" + uuid.NewString(), PaymentMethodID: &localMethod}
		retried, err := customer.RetrySubscriptionNow(t.Context(), retryRequest)
		require.NoError(t, err)
		require.True(t, retried.Operation.Unresolved())
		again, err := customer.RetrySubscriptionNow(t.Context(), retryRequest)
		require.NoError(t, err)
		require.Equal(t, retried.Operation.ID, again.Operation.ID)
		differentKey := retryRequest
		differentKey.IdempotencyKey = "not-accepted-" + uuid.NewString()
		_, err = customer.RetrySubscriptionNow(t.Context(), differentKey)
		require.Error(t, err, "new key cannot alias an unresolved original and charge a later period")
		view = call("GET", "/subscriptions/"+subscription.String(), "", nil)
		recoveryView = view["recovery"].(map[string]any)
		require.Equal(t, false, recoveryView["retryable"])
		require.Equal(t, "authentication_required", recoveryView["blocked_reason"])
		recovery := call("GET", fmt.Sprintf("/payment-operations/%s/authentication", retried.Operation.ID), "", nil)
		require.Equal(t, "pi_customerretry_secret_private", recovery["client_secret"])
		if retryMode == "replacement" {
			require.Equal(t, "pm_replacement", recovery["provider_payment_method_id"])
		}
		mu.Lock()
		paymentPaid = true
		mu.Unlock()
		result := call("POST", fmt.Sprintf("/payment-operations/%s/authentication/confirm", retried.Operation.ID), "", nil)
		require.Equal(t, "succeeded", result["status"])
		rt.Config.EngineAdmissionHold = true
		rt.MoneyService.EngineAdmissionHold = true
		final, err := customer.RetrySubscriptionNow(t.Context(), retryRequest)
		require.NoError(t, err)
		require.True(t, final.Replayed)
		require.Equal(t, retried.Operation.ID, final.Operation.ID)
		require.Equal(t, "active", string(final.Subscription.Status))
		rt.Config.EngineAdmissionHold = false
		rt.MoneyService.EngineAdmissionHold = false
		require.NoError(t, pool.QueryRow(t.Context(), `SELECT count(*) FROM billing.payments WHERE customer_id=$1 AND status='completed'`, user.ID).Scan(&paid))
		require.Equal(t, 2, paid, "initial and recovered period settle once")
	}
	mu.Lock()
	if retryMode == "replacement" {
		require.Equal(t, 2, setupCreates)
	} else {
		require.Equal(t, 1, setupCreates)
	}
	if customerRetry {
		require.Equal(t, 3, paymentCreates)
	} else {
		require.Equal(t, 1, paymentCreates)
	}
	mu.Unlock()
}
