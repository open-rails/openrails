//go:build integration && nativepolicy

package integrationharness

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/embed"
	"github.com/open-rails/openrails/internal/app"
	orauthkit "github.com/open-rails/openrails/internal/hostauth"
	"github.com/open-rails/openrails/internal/httptesthost"
	embcp "github.com/open-rails/openrails/internal/operator"
	riverjobs "github.com/open-rails/openrails/internal/river"
	"github.com/riverqueue/river"
	"github.com/stretchr/testify/require"
)

// Run alone: qualification uses a verified fail-closed environment proxy before
// any HTTP transport caches proxy settings. This is characterization, not native
// enrollment acceptance (the independent initial-charge gap is owned by #576).
func TestFreshNativeProviderRecoveryGap(t *testing.T) {
	var blocked atomic.Int64
	guard := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Host, "cdn.jsdelivr.net:") && !strings.HasPrefix(r.Host, "latest.currency-api.pages.dev:") {
			blocked.Add(1)
			t.Logf("blocked unexpected egress %s", r.Host)
		}
		http.Error(w, "non-loopback egress forbidden", 502)
	}))
	defer guard.Close()
	t.Setenv("HTTP_PROXY", guard.URL)
	t.Setenv("HTTPS_PROXY", guard.URL)
	t.Setenv("NO_PROXY", "127.0.0.1,localhost,::1")
	proxy, err := http.ProxyFromEnvironment(&http.Request{URL: &url.URL{Scheme: "https", Host: "secure.nmi.com"}})
	require.NoError(t, err)
	require.NotNil(t, proxy)
	require.Equal(t, guard.URL, proxy.String())
	t.Cleanup(func() { require.Zero(t, blocked.Load()) })
	h := New(t, t.Context())
	gateway := NewFakeNMIGateway(t)
	vault, billing := "fresh-vault-"+uuid.NewString(), "fresh-billing-"+uuid.NewString()
	var vaultCreates atomic.Int64
	wire := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/customers") && r.Method == http.MethodPost {
			vaultCreates.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"id": vault, "billing": []any{map[string]any{"id": billing, "priority": 1, "payment_details": map[string]any{"card_number": "411111******1111", "card_exp": "1230"}}}})
			return
		}
		gateway.serve(w, r)
	}))
	defer wire.Close()
	surface := h.StartStandalone("USD", WithConfig(func(c *config.Config) { c.ProviderSandbox = &config.ProviderSandboxConfig{NMIGatewayURL: wire.URL} }))
	owned := surface.ProvisionOwnedMerchant("native-policy-" + uuid.NewString()[:8])
	rt := surface.App().Runtime
	psp := h.ArmLoopbackNMI(rt, owned.MerchantID)
	owner := surface.Client(openrails.WithAPIKey(owned.APIKey), openrails.WithMerchantID(owned.MerchantID))
	cp := embcp.Get(surface.App())
	user, err := cp.Core().CreateUser(t.Context(), uuid.NewString()+"@example.test", "payer"+uuid.NewString()[:8])
	require.NoError(t, err)
	token, _, err := cp.Core().MintAccessToken(t.Context(), user.ID, nil)
	require.NoError(t, err)
	customer := openrails.CustomerID(uuid.MustParse(user.ID))
	_, err = owner.EnsureCustomer(t.Context(), customer)
	require.NoError(t, err)
	product, err := owner.CreateProduct(t.Context(), openrails.CreateProductRequest{Key: uuid.NewString(), DisplayName: "Native recovery", EntitlementsSpec: map[string]*int{"paid": nil}})
	require.NoError(t, err)
	hours := 720
	price, err := owner.CreatePrice(t.Context(), openrails.CreatePriceRequest{ProductID: product.ID, UnitAmount: 9_990_000, Currency: "USD", AutoRenew: true, AccessDurationHours: &hours})
	require.NoError(t, err)
	pool := h.MerchantPool(owned.MerchantID.UUID())
	_, err = pool.Exec(t.Context(), `INSERT INTO billing.price_psp_bindings(merchant_id,price_id,psp_id,plan_id) VALUES($1,$2,$3,'native-policy-plan')`, owned.MerchantID.UUID(), price.ID.UUID(), psp)
	require.NoError(t, err)
	session, err := owner.CreateCheckoutSession(t.Context(), openrails.CreateCheckoutSessionRequest{Customer: openrails.CheckoutCustomerIdentity{ID: customer.String(), VerifiedEmail: *user.Email, Username: "native-payer"}, PriceID: price.ID.String(), IdempotencyKey: uuid.NewString(), PaymentOptions: openrails.CheckoutPaymentOptions{PSPID: psp.String(), PaymentToken: "synthetic-collect-token"}})
	require.NoError(t, err)
	require.Equal(t, "succeeded", session.Status)
	require.NotNil(t, session.SubscriptionID)
	require.EqualValues(t, 1, vaultCreates.Load())
	var policy string
	var method uuid.UUID
	require.NoError(t, pool.QueryRow(t.Context(), `SELECT collection_policy,payment_method_id FROM billing.subscriptions WHERE id=$1`, session.SubscriptionID.UUID()).Scan(&policy, &method))
	require.Equal(t, "provider", policy)
	// Inject only the observed missed-renewal lifecycle; do not mutate ownership.
	end := time.Now().UTC().Truncate(time.Microsecond).Add(-time.Hour)
	_, err = pool.Exec(t.Context(), `UPDATE billing.subscriptions SET status='past_due',current_period_starts_at=$2,current_period_ends_at=$3,next_retry_at=$4 WHERE id=$1`, session.SubscriptionID.UUID(), end.Add(-30*24*time.Hour), end, end)
	require.NoError(t, err)
	authn, err := orauthkit.NewDelegatedAuthenticator(cp.AuthService().Verifier(), owned.MerchantID.String())
	require.NoError(t, err)
	customerRuntime, err := embed.New(t.Context(), embed.Options{Config: &config.Config{Encryption: &config.EncryptionConfig{MasterKey: "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8="}, TestMode: config.CredentialPostureSandbox, MerchantConfigHTTP: true, SecretBackend: config.SecretBackendDB, ProviderWriteMode: config.ProviderWriteModeFull, DB: &config.DBConfig{URL: h.DSN}, ProviderSandbox: &config.ProviderSandboxConfig{NMIGatewayURL: wire.URL}}, Redis: h.Redis, River: embed.RiverManagedByOpenRails(), DelegatedAuthenticator: authn})
	require.NoError(t, err)
	defer customerRuntime.Close(context.Background())
	app.HostGraph(customerRuntime).Runtime.SetConfiguredMerchant(owned.MerchantID)
	handler, err := httptesthost.Handler(customerRuntime, httptesthost.Options{HTTP: embed.HTTPConfig{CustomerRoutes: []embed.CustomerRoutesConfig{{Treasury: true}}}, DelegatedAuthenticator: authn})
	require.NoError(t, err)
	mounted := httptest.NewServer(handler)
	defer mounted.Close()
	customerClient, err := openrails.NewRemote(mounted.URL, openrails.WithMerchantID(owned.MerchantID), openrails.WithTokenProvider(func(context.Context) (string, error) { return token, nil }))
	require.NoError(t, err)
	observed, err := customerClient.GetMySubscription(t.Context(), *session.SubscriptionID)
	require.NoError(t, err)
	require.Equal(t, "provider", observed.CollectionPolicy)
	require.NotNil(t, observed.Recovery)
	require.False(t, observed.Recovery.Retryable)
	require.Equal(t, "customer_payment_unsupported", observed.Recovery.BlockedReason)
	before := gateway.SaleAttempts()
	_, err = customerClient.RetrySubscriptionNow(t.Context(), openrails.RetrySubscriptionNowRequest{SubscriptionID: *session.SubscriptionID, IdempotencyKey: uuid.NewString()})
	require.Error(t, err)
	worker := riverjobs.DunningWorker{DB: rt.DB, Config: rt.Config, NMIResolver: rt.CollectionResolver}
	require.NoError(t, worker.Work(t.Context(), &river.Job[riverjobs.DunningArgs]{}))
	require.Equal(t, before, gateway.SaleAttempts())
	var attempts int
	require.NoError(t, pool.QueryRow(t.Context(), `SELECT count(*) FROM billing.rail_intents WHERE subscription_id=$1 AND intent_type IN ('manual_rebill','subscription_collection')`, session.SubscriptionID.UUID()).Scan(&attempts))
	require.Zero(t, attempts)
	t.Log("fresh token checkout -> native provider subscription -> past_due -> public recovery unsupported; due worker admitted zero charges")
}
