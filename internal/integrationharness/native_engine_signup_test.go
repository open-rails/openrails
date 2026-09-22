//go:build integration

package integrationharness

import (
	"context"
	"encoding/json"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/intents"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/config"
	orauthkit "github.com/open-rails/openrails/embed/authkit"
	embcp "github.com/open-rails/openrails/internal/operator"
	riverjobs "github.com/open-rails/openrails/internal/river"
	"github.com/open-rails/openrails/pkg/billingauth"
	"github.com/open-rails/openrails/pkg/merchant"
	"github.com/riverqueue/river"
	"github.com/stretchr/testify/require"
)

func TestNativeEngineSignupSelfHTTPAndDueWorker(t *testing.T) { nativeEngineSignupSelfHTTP(t, "") }
func TestNativeEngineReplacementCITSelfHTTP(t *testing.T) {
	for _, mode := range []string{"unknown", "declined", "cancel_unknown"} {
		t.Run(mode, func(t *testing.T) { nativeEngineSignupSelfHTTP(t, mode) })
	}
}

func nativeEngineSignupSelfHTTP(t *testing.T, replacementMode string) {
	replacement := replacementMode != ""
	h := New(t, t.Context())
	gateway := NewFakeNMIGateway(t)
	vault, billing := "new-vault-"+uuid.NewString(), "new-billing-"+uuid.NewString()
	newVault, newBilling := "replacement-vault-"+uuid.NewString(), "replacement-billing-"+uuid.NewString()
	var captures atomic.Int32
	var wireMu sync.Mutex
	var sales []url.Values
	wire := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/customers") && r.Method == "POST" || strings.HasSuffix(r.URL.Path, "/customers/"+vault) && r.Method == "GET" || strings.HasSuffix(r.URL.Path, "/customers/"+newVault) && r.Method == "GET" {
			targetVault, targetBilling := vault, billing
			if r.Method == "POST" && captures.Add(1) > 1 || strings.HasSuffix(r.URL.Path, "/customers/"+newVault) {
				targetVault, targetBilling = newVault, newBilling
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"object": "customer", "id": targetVault, "billing": []any{map[string]any{"id": targetBilling, "priority": 1, "payment_details": map[string]any{"card_number": "411111******1111", "card_exp": "1230"}}}})
			return
		}
		if r.Method == "POST" {
			require.NoError(t, r.ParseForm())
			if r.Form.Get("type") == "sale" {
				require.Empty(t, r.Form.Get("recurring"))
				require.Empty(t, r.Form.Get("subscription_id"))
				expectedBilling := billing
				if r.Form.Get("customer_vault_id") == newVault {
					expectedBilling = newBilling
				}
				require.Equal(t, expectedBilling, r.Form.Get("billing_id"))
				wireMu.Lock()
				sales = append(sales, r.PostForm)
				wireMu.Unlock()
				require.Equal(t, "recurring", r.Form.Get("billing_method"))
			}
		}
		gateway.serve(w, r)
	}))
	defer wire.Close()
	clock := clockwork.NewFakeClockAt(time.Now().UTC().Add(-31 * 24 * time.Hour).Truncate(time.Microsecond))
	var host billingauth.DelegatedAuthenticator
	surface := h.StartStandalone("USD", WithClock(clock), WithConfig(func(c *config.Config) {
		c.ProviderSandbox = &config.ProviderSandboxConfig{NMIGatewayURL: wire.URL}
		c.ProviderWriteMode = config.ProviderWriteModeFull
		c.NewSubscriptionCollectionPolicy = "engine"
	}), func(c *standaloneConfig) {
		c.delegatedAuthenticator = billingauth.DelegatedAuthenticatorFunc(func(ctx context.Context, r *http.Request) (*billingauth.DelegatedPrincipal, error) {
			return host.AuthenticateDelegated(ctx, r)
		})
	})
	owned := surface.ProvisionOwnedMerchant("native-engine-" + uuid.NewString()[:8])
	cp, rt := embcp.Get(surface.App()), surface.App().Runtime
	var err error
	host, err = orauthkit.NewDelegatedAuthenticator(cp.AuthService().Verifier(), owned.MerchantID.String())
	require.NoError(t, err)
	owner := surface.Client(openrails.WithAPIKey(owned.APIKey), openrails.WithMerchantID(owned.MerchantID))
	psp := h.ArmLoopbackNMI(rt, owned.MerchantID)
	product, err := owner.CreateProduct(t.Context(), openrails.CreateProductRequest{Key: uuid.NewString(), DisplayName: "Engine native", EntitlementsSpec: map[string]*int{"engine_access": nil}})
	require.NoError(t, err)
	hours := 720
	price, err := owner.CreatePrice(t.Context(), openrails.CreatePriceRequest{ProductID: product.ID, UnitAmount: 9990000, Currency: "USD", AutoRenew: true, AccessDurationHours: &hours})
	require.NoError(t, err)
	user, err := cp.Core().CreateUser(t.Context(), uuid.NewString()+"@example.test", "native"+uuid.NewString()[:8])
	require.NoError(t, err)
	token, _, err := cp.Core().MintAccessToken(t.Context(), user.ID, nil)
	require.NoError(t, err)
	_, err = owner.EnsureCustomer(t.Context(), openrails.CustomerID(uuid.MustParse(user.ID)))
	require.NoError(t, err)
	call := func(path, key string, body any) map[string]any {
		status, raw := cutoverHTTPRequest(t, "POST", surface.BaseURL+"/v1/me"+path, token, key, body)
		require.True(t, status >= 200 && status < 300, "%s: %s", path, raw)
		var v map[string]any
		require.NoError(t, json.Unmarshal(raw, &v))
		return v
	}
	saved := call("/payment-methods", "", map[string]any{"provider": "nmi", "payment_token": "synthetic-collectjs-token", "name_on_card": "Test Payer"})
	method := saved["id"]
	if data, ok := saved["data"].(map[string]any); ok {
		method = data["id"]
	}
	require.NotNil(t, method)
	quote := call("/checkout", "new-agreement-"+uuid.NewString(), map[string]any{"mode": "subscription", "price_id": price.ID, "payment": map[string]any{"psp_id": psp, "rail": "nmi", "payment_method_id": method}})
	require.NotNil(t, quote["membership_quote"])
	complete := call("/checkout/"+quote["id"].(string)+"/confirm", "", map[string]any{"payment": map[string]string{"rail": "nmi"}})
	require.Equal(t, "succeeded", complete["status"])
	_ = call("/checkout/"+quote["id"].(string)+"/confirm", "", map[string]any{"payment": map[string]string{"rail": "nmi"}})
	require.Equal(t, 1, gateway.SaleAttempts())
	require.Empty(t, gateway.Enrollments())
	pool := h.MerchantPool(owned.MerchantID.UUID())
	var policy, external string
	require.NoError(t, pool.QueryRow(t.Context(), `SELECT collection_policy,rail_subscription_id FROM billing.subscriptions WHERE customer_id=$1`, user.ID).Scan(&policy, &external))
	require.Equal(t, "engine", policy)
	require.Empty(t, external)
	checkAccess := func(want bool) {
		require.NoError(t, rt.DB.RunInMerchantConn(merchant.WithID(t.Context(), owned.MerchantID), func(ctx context.Context) error {
			has, err := rt.EntitlementService.IsEntitled(ctx, user.ID, "engine_access", clock.Now())
			if err != nil {
				return err
			}
			require.Equal(t, want, has)
			return nil
		}))
	}
	checkAccess(true)
	if replacement {
		gateway.SetMode(NMISaleDecline)
	}
	clock.Advance(720*time.Hour + time.Second)
	checkAccess(false) // Paid access expires even before any worker runs.
	worker := riverjobs.DunningWorker{DB: rt.DB, Config: rt.Config, Clock: clock, NMIResolver: rt.CollectionResolver, EngineCollections: rt.MoneyService}
	require.NoError(t, worker.Work(t.Context(), &river.Job[riverjobs.DunningArgs]{}))
	subscription, err := openrails.ParseSubscriptionID(complete["subscription_id"].(string))
	require.NoError(t, err)
	var dueID uuid.UUID
	require.NoError(t, pool.QueryRow(t.Context(), `SELECT id FROM billing.rail_intents WHERE subscription_id=$1 AND intent_type='subscription_collection' ORDER BY created_at DESC LIMIT 1`, subscription.UUID()).Scan(&dueID))
	scope := db.WithPSPID(merchant.WithID(t.Context(), owned.MerchantID), psp)
	require.NoError(t, rt.DB.RunInMerchantConn(scope, func(ctx context.Context) error { _, err := rt.IntentRunner().ExecuteByID(ctx, dueID); return err }))
	if replacement {
		savedNew := call("/payment-methods", "", map[string]any{"provider": "nmi", "payment_token": "synthetic-newcard-token", "name_on_card": "Test Payer"})
		newMethodRaw := savedNew["id"]
		if data, ok := savedNew["data"].(map[string]any); ok {
			newMethodRaw = data["id"]
		}
		newMethod, err := openrails.ParsePaymentMethodID(newMethodRaw.(string))
		require.NoError(t, err)
		oldMethod, err := openrails.ParsePaymentMethodID(method.(string))
		require.NoError(t, err)
		var bound uuid.UUID
		var anchor string
		require.NoError(t, pool.QueryRow(t.Context(), `SELECT stored_credential_recurring_ref FROM billing.payment_methods WHERE id=$1`, newMethod.UUID()).Scan(&anchor))
		require.Empty(t, anchor, "saving a token is not recurring payment consent")
		status, _ := cutoverHTTPRequest(t, "PUT", surface.BaseURL+"/v1/me/subscriptions/"+subscription.String()+"/payment-method", token, "", map[string]any{"payment_method_id": newMethod})
		require.GreaterOrEqual(t, status, 400, "unanchored card cannot become an automatic engine instrument")
		gateway.SetMode(NMISaleUncertain)
		gateway.SetVisible(false)
		customer, err := openrails.NewRemote(surface.BaseURL, openrails.WithMerchantID(owned.MerchantID), openrails.WithTokenProvider(func(context.Context) (string, error) { return token, nil }))
		require.NoError(t, err)
		req := openrails.RetrySubscriptionNowRequest{SubscriptionID: subscription, IdempotencyKey: "replacement-cit-" + uuid.NewString(), PaymentMethodID: &newMethod}
		if replacementMode == "declined" {
			gateway.SetMode(NMISaleDecline)
		}
		pending, err := customer.RetrySubscriptionNow(t.Context(), req)
		if replacementMode == "declined" {
			require.Error(t, err)
			require.Equal(t, 3, gateway.SaleAttempts())
			_, err = customer.RetrySubscriptionNow(t.Context(), req)
			require.Error(t, err)
			require.Equal(t, 3, gateway.SaleAttempts())
			var retryAt *time.Time
			require.NoError(t, pool.QueryRow(t.Context(), `SELECT payment_method_id,next_retry_at FROM billing.subscriptions WHERE id=$1`, subscription.UUID()).Scan(&bound, &retryAt))
			require.Equal(t, oldMethod.UUID(), bound)
			require.Nil(t, retryAt, "failed replacement cannot restart automatic charging of the old card")
			require.NoError(t, pool.QueryRow(t.Context(), `SELECT stored_credential_recurring_ref FROM billing.payment_methods WHERE id=$1`, newMethod.UUID()).Scan(&anchor))
			require.Empty(t, anchor)
			var paid int
			require.NoError(t, pool.QueryRow(t.Context(), `SELECT count(*) FROM billing.payments WHERE customer_id=$1 AND status='completed'`, user.ID).Scan(&paid))
			require.Equal(t, 1, paid)
			require.Empty(t, gateway.Enrollments())
			return
		}
		require.NoError(t, err)
		require.Equal(t, intents.StatusUnknownNeedsVerify, pending.Operation.Status)
		require.NoError(t, pool.QueryRow(t.Context(), `SELECT payment_method_id FROM billing.subscriptions WHERE id=$1`, subscription.UUID()).Scan(&bound))
		require.Equal(t, oldMethod.UUID(), bound, "uncertain replacement must not swap card")
		require.NoError(t, pool.QueryRow(t.Context(), `SELECT stored_credential_recurring_ref FROM billing.payment_methods WHERE id=$1`, newMethod.UUID()).Scan(&anchor))
		require.Empty(t, anchor)
		same, err := customer.RetrySubscriptionNow(t.Context(), req)
		require.NoError(t, err)
		require.Equal(t, pending.Operation.ID, same.Operation.ID)
		require.Equal(t, 3, gateway.SaleAttempts())
		if replacementMode == "cancel_unknown" {
			_ = call("/subscriptions/"+subscription.String()+"/cancel", "", map[string]any{"feedback": "cancel during replacement verification"})
			var raw []byte
			require.NoError(t, pool.QueryRow(t.Context(), `SELECT args FROM public.river_job WHERE kind=$1 AND args->>'subscription_id'=$2 ORDER BY id DESC LIMIT 1`, riverjobs.KindSubscriptionCancel, subscription.UUID().String()).Scan(&raw))
			var args riverjobs.CancelSubscriptionArgs
			require.NoError(t, json.Unmarshal(raw, &args))
			cancelWorker := riverjobs.CancelSubscriptionWorker{DB: rt.DB, Config: rt.Config, SubscriptionService: rt.SubscriptionService, SubscriptionLifecycleService: rt.SubscriptionLifecycleService, UserSubscriptionService: rt.UserSubscriptionService}
			require.NoError(t, cancelWorker.Work(t.Context(), &river.Job[riverjobs.CancelSubscriptionArgs]{Args: args}))
		}
		gateway.SetVisible(true)
		rt.Config.EngineAdmissionHold = true
		rt.MoneyService.EngineAdmissionHold = true
		paid, err := customer.RetrySubscriptionNow(t.Context(), req)
		require.NoError(t, err)
		require.Equal(t, intents.StatusSucceeded, paid.Operation.Status)
		require.Equal(t, pending.Operation.ID, paid.Operation.ID)
		require.NoError(t, pool.QueryRow(t.Context(), `SELECT payment_method_id FROM billing.subscriptions WHERE id=$1`, subscription.UUID()).Scan(&bound))
		require.NoError(t, pool.QueryRow(t.Context(), `SELECT stored_credential_recurring_ref FROM billing.payment_methods WHERE id=$1`, newMethod.UUID()).Scan(&anchor))
		if replacementMode == "cancel_unknown" {
			require.Equal(t, oldMethod.UUID(), bound)
			require.Empty(t, anchor, "late receipt cannot authorize a replacement on a canceled obligation")
		} else {
			require.Equal(t, newMethod.UUID(), bound)
			captured := gateway.Sales()
			require.Equal(t, captured[len(captured)-1].TransactionID, anchor)
		}
		rt.Config.EngineAdmissionHold = false
		rt.MoneyService.EngineAdmissionHold = false
		wireMu.Lock()
		require.Len(t, sales, 3)
		require.Equal(t, "customer", sales[2].Get("initiated_by"))
		require.Equal(t, "stored", sales[2].Get("stored_credential_indicator"))
		require.Empty(t, sales[2].Get("initial_transaction_id"))
		require.Equal(t, newVault, sales[2].Get("customer_vault_id"))
		require.Equal(t, newBilling, sales[2].Get("billing_id"))
		wireMu.Unlock()
	}
	checkAccess(replacementMode != "cancel_unknown")
	var paid int
	require.NoError(t, pool.QueryRow(t.Context(), `SELECT count(*) FROM billing.payments WHERE customer_id=$1 AND status='completed'`, user.ID).Scan(&paid))
	require.Equal(t, 2, paid)
	if replacement {
		require.Equal(t, 3, gateway.SaleAttempts())
	} else {
		require.Equal(t, 2, gateway.SaleAttempts())
	}
	require.Empty(t, gateway.Enrollments())
}
