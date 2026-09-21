//go:build integration

package integrationharness

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
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

func TestNativeEngineSignupSelfHTTPAndDueWorker(t *testing.T) {
	h := New(t, t.Context())
	gateway := NewFakeNMIGateway(t)
	vault, billing := "new-vault-"+uuid.NewString(), "new-billing-"+uuid.NewString()
	wire := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/customers") && r.Method == "POST" || strings.HasSuffix(r.URL.Path, "/customers/"+vault) && r.Method == "GET" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"object": "customer", "id": vault, "billing": []any{map[string]any{"id": billing, "priority": 1, "payment_details": map[string]any{"card_number": "411111******1111", "card_exp": "1230"}}}})
			return
		}
		if r.Method == "POST" {
			require.NoError(t, r.ParseForm())
			if r.Form.Get("type") == "sale" {
				require.Empty(t, r.Form.Get("recurring"))
				require.Empty(t, r.Form.Get("subscription_id"))
				require.Equal(t, billing, r.Form.Get("billing_id"))
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
	clock.Advance(720*time.Hour + time.Second)
	checkAccess(false) // Paid access expires even before any worker runs.
	worker := riverjobs.DunningWorker{DB: rt.DB, Config: rt.Config, Clock: clock, NMIResolver: rt.CollectionResolver, EngineCollections: rt.MoneyService}
	require.NoError(t, worker.Work(t.Context(), &river.Job[riverjobs.DunningArgs]{}))
	require.NoError(t, rt.DB.RunInMerchantConn(merchant.WithID(t.Context(), owned.MerchantID), func(ctx context.Context) error { _, err := rt.IntentRunner().RunExecuteOnce(ctx); return err }))
	checkAccess(true)
	var paid int
	require.NoError(t, pool.QueryRow(t.Context(), `SELECT count(*) FROM billing.payments WHERE customer_id=$1 AND status='completed'`, user.ID).Scan(&paid))
	require.Equal(t, 2, paid)
	require.Equal(t, 2, gateway.SaleAttempts())
	require.Empty(t, gateway.Enrollments())
}
