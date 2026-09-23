//go:build integration

package integrationharness

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/embed"
	"github.com/open-rails/openrails/internal/dbtest"
	httproutes "github.com/open-rails/openrails/internal/http/routes"
	"github.com/open-rails/openrails/internal/httptesthost"
	"github.com/open-rails/openrails/pkg/billingauth"
)

// selfSurface issues one authenticated self-route request as the customer.
type selfSurface func(method, path string, body any) (int, []byte)

// TestSelfSubscriptionWireParity pins the customer's own subscription routes
// on the shared Subscription shape, identically embedded and standalone, and
// proves the engine accepts its own answer: the ids the list returns are the
// ids cancel, resume, GET-by-id and /me/status agree on, with no rewriting.
func TestSelfSubscriptionWireParity(t *testing.T) {
	ctx := context.Background()
	h := New(t, ctx)
	pool := h.sharedPool()
	dbtest.EnsureTestMerchant(ctx, t, pool)
	mid := dbtest.TestMerchantID.UUID()
	standalone := h.StartStandalone("usd")

	rt, err := embed.New(ctx, embed.Options{
		Config: &config.Config{TestMode: config.CredentialPostureSandbox, MerchantConfigSource: config.MerchantConfigSourceAPI, SecretBackend: config.SecretBackendDB, ProviderWriteMode: config.ProviderWriteModeFull, DB: &config.DBConfig{URL: h.DSN}},
		Redis:  h.Redis, River: embed.RiverManagedByOpenRails(),
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = rt.Close(context.Background()) })

	type surface struct {
		customer uuid.UUID
		call     selfSurface
	}
	surfaces := map[string]surface{}

	embeddedCustomer := uuid.New()
	authn := billingauth.DelegatedAuthenticatorFunc(func(ctx context.Context, r *http.Request) (*billingauth.DelegatedPrincipal, error) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			return nil, billingauth.ErrUnauthenticated
		}
		return &billingauth.DelegatedPrincipal{MerchantID: dbtest.TestMerchantID.String(), MerchantSlug: dbtest.TestMerchantSlug, SubjectID: embeddedCustomer.String(), Issuer: "embedded-host"}, nil
	})
	handler, err := httptesthost.Handler(rt, httptesthost.Options{HTTP: embed.HTTPConfig{Customer: true, Gate: httproutes.NewGate(httproutes.GateOptions{DelegatedAuthenticator: authn})}, DelegatedAuthenticator: authn})
	require.NoError(t, err)
	surfaces["embedded"] = surface{customer: embeddedCustomer, call: func(method, path string, body any) (int, []byte) {
		var buf bytes.Buffer
		if body != nil {
			require.NoError(t, json.NewEncoder(&buf).Encode(body))
		}
		req := httptest.NewRequest(method, path, &buf)
		req.Header.Set("Authorization", "Bearer host-session")
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec.Code, rec.Body.Bytes()
	}}

	standaloneCustomer := uuid.New()
	caller := standalone.RegisterDelegatedCaller("selfsub-"+strings.ReplaceAll(uuid.NewString(), "-", "")[:8], dbtest.TestMerchantSlug, standaloneCustomer.String(), nil)
	surfaces["standalone"] = surface{customer: standaloneCustomer, call: func(method, path string, body any) (int, []byte) {
		return requestJSON(t, method, standalone.BaseURL+path, caller.Token, body)
	}}

	exec := func(sql string, args ...any) {
		_, err := pool.Exec(ctx, sql, args...)
		require.NoError(t, err, sql)
	}
	psp := dbtest.EnsureTestPSP(ctx, t, pool, mid, "nmi")
	stripePSP := dbtest.EnsureTestPSP(ctx, t, pool, mid, "stripe")
	now := time.Now().UTC()

	shapes := map[string][]string{}
	for name, s := range surfaces {
		t.Run(name, func(t *testing.T) {
			suffix := uuid.NewString()[:8]
			productID, priceID, scheduledPriceID := uuid.New(), uuid.New(), uuid.New()
			methodID, activeID, cancelledID := uuid.New(), uuid.New(), uuid.New()
			exec(`INSERT INTO billing.customers(merchant_id,id) VALUES($1,$2)`, mid, s.customer)
			exec(`INSERT INTO billing.products(id,merchant_id,key,display_name,entitlements_spec) VALUES($1,$2,$3,'Self Pro','{"premium":null}')`, productID, mid, "selfsub-"+suffix)
			exec(`INSERT INTO billing.prices(id,merchant_id,product_id,key,amount,currency,access_duration_hours,auto_renew) VALUES($1,$2,$3,$4,9007199254740993,'USD',720,true),($5,$2,$3,$6,96000000,'USD',8760,true)`,
				priceID, mid, productID, "selfsub-m-"+suffix, scheduledPriceID, "selfsub-a-"+suffix)
			exec(`INSERT INTO billing.payment_methods(id,merchant_id,customer_id,psp_id,rail,rail_customer_ref,rail_method_ref,initial_transaction_id,last_four,card_type,expiry_date) VALUES($1,$2,$3,$4,'nmi',$5::text,$5::text,$5::text,'4242','visa','1230')`,
				methodID, mid, s.customer, psp, "selfsub-"+methodID.String())
			exec(`INSERT INTO billing.subscriptions(id,merchant_id,customer_id,product_id,price_id,scheduled_price_id,psp_id,rail,status,rail_subscription_id,payment_method_id,started_at,current_period_starts_at,current_period_ends_at) VALUES($1,$2,$3,$4,$5,$6,$7,'nmi','active',$8,$9,$10,$10,$11)`,
				activeID, mid, s.customer, productID, priceID, scheduledPriceID, psp, "rail-"+activeID.String(), methodID, now.Add(-time.Hour), now.Add(720*time.Hour))
			exec(`INSERT INTO billing.subscriptions(id,merchant_id,customer_id,product_id,price_id,psp_id,rail,status,rail_subscription_id,started_at,current_period_starts_at,current_period_ends_at,cancelled_at,cancel_type) VALUES($1,$2,$3,$4,$5,$6,'stripe','cancelled',$7,$8,$8,$9,$10,'user')`,
				cancelledID, mid, s.customer, productID, priceID, stripePSP, "sub_"+cancelledID.String(), now.Add(-48*time.Hour), now.Add(600*time.Hour), now.Add(-time.Hour))
			exec(`INSERT INTO billing.entitlements(id,merchant_id,customer_id,entitlement,start_at,end_at,source_id,source_type) VALUES($1,$2,$3,'premium',$4,$5,$6,'subscription')`,
				uuid.New(), mid, s.customer, now.Add(-time.Hour), now.Add(720*time.Hour), activeID)

			status, body := s.call(http.MethodGet, "/v1/me/subscriptions?status=all", nil)
			require.Equal(t, http.StatusOK, status, string(body))
			var list openrails.Page[openrails.Subscription]
			require.NoError(t, json.Unmarshal(body, &list), string(body))
			require.Len(t, list.Data, 2)
			var active, cancelled openrails.Subscription
			for _, row := range list.Data {
				switch row.Status {
				case "active":
					active = row
				case "cancelled":
					cancelled = row
				}
			}
			// The engine's own ids, in their one wire spelling.
			require.Equal(t, openrails.SubscriptionID(activeID), active.ID)
			require.Equal(t, s.customer.String(), active.CustomerID)
			require.Equal(t, openrails.ProductID(productID).String(), active.ProductID)
			require.Equal(t, openrails.PriceID(priceID).String(), active.PriceID)
			require.NotNil(t, active.ScheduledPriceID)
			require.Equal(t, openrails.PriceID(scheduledPriceID).String(), *active.ScheduledPriceID)
			require.NotNil(t, active.PaymentMethodID)
			require.Equal(t, openrails.PaymentMethodID(methodID), *active.PaymentMethodID)
			require.NotNil(t, active.Price)
			require.Equal(t, active.PriceID, active.Price.ID)
			require.EqualValues(t, 9007199254740993, active.Price.UnitAmount)
			require.NotNil(t, active.Product)
			require.Equal(t, active.ProductID, active.Product.ID)
			require.NotNil(t, active.ScheduledPrice)
			require.Equal(t, *active.ScheduledPriceID, active.ScheduledPrice.ID)
			require.NotNil(t, active.ScheduledProduct)
			require.NotNil(t, active.Card, string(body))
			require.Equal(t, "4242", active.Card.Last4)
			require.NotNil(t, active.Access)
			require.Equal(t, active.ID, active.Access.SubscriptionID)
			require.Equal(t, "subscription", active.Access.Kind)
			require.True(t, cancelled.Resumable)
			require.Equal(t, openrails.SubscriptionID(cancelledID), cancelled.ID)

			// Raw wire: prefixed ids, unit_amount as a decimal string, no bare uuid.
			var raw map[string]any
			require.NoError(t, json.Unmarshal(body, &raw))
			rows := raw["data"].([]any)
			var rawActive map[string]any
			for _, r := range rows {
				if row := r.(map[string]any); row["status"] == "active" {
					rawActive = row
				}
			}
			require.Equal(t, "sub_"+activeID.String(), rawActive["id"])
			require.Equal(t, "9007199254740993", rawActive["price"].(map[string]any)["unit_amount"])
			require.Equal(t, "sub_"+activeID.String(), rawActive["access"].(map[string]any)["subscription_id"])
			for key, value := range rawActive {
				if str, ok := value.(string); ok {
					require.NotEqual(t, activeID.String(), str, "bare uuid at %s", key)
					require.NotEqual(t, priceID.String(), str, "bare uuid at %s", key)
				}
			}
			keys := make([]string, 0, len(rawActive))
			for key := range rawActive {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			shapes[name] = keys

			// The listed id is the id every other self route takes, unchanged.
			status, body = s.call(http.MethodGet, "/v1/me/subscriptions/"+active.ID.String(), nil)
			require.Equal(t, http.StatusOK, status, string(body))
			var one openrails.Subscription
			require.NoError(t, json.Unmarshal(body, &one))
			require.Equal(t, active.ID, one.ID)
			require.Equal(t, active.Access, one.Access)

			status, body = s.call(http.MethodGet, "/v1/me/status", nil)
			require.Equal(t, http.StatusOK, status, string(body))
			var billing openrails.BillingStatus
			require.NoError(t, json.Unmarshal(body, &billing), string(body))
			require.True(t, billing.HasActiveSubscription)
			require.NotNil(t, billing.Subscription)
			require.Equal(t, active.ID, billing.Subscription.ID)
			require.NotNil(t, billing.Access)
			require.Equal(t, active.ID, billing.Access.SubscriptionID)
			require.Len(t, billing.Entitlements, 1)
			require.NotNil(t, billing.Entitlements[0].SourceID)
			require.Equal(t, active.ID.String(), *billing.Entitlements[0].SourceID)

			status, body = s.call(http.MethodPost, "/v1/me/subscriptions/"+active.ID.String()+"/cancel", map[string]string{"feedback": "moving on"})
			require.Equal(t, http.StatusAccepted, status, string(body))
			status, body = s.call(http.MethodPost, "/v1/me/subscriptions/"+cancelled.ID.String()+"/resume", nil)
			require.Equal(t, http.StatusAccepted, status, string(body))

			// The bare uuid is not an id on any of them.
			for _, path := range []string{"/v1/me/subscriptions/" + activeID.String(), "/v1/me/subscriptions/" + activeID.String() + "/resume"} {
				method := http.MethodGet
				if strings.HasSuffix(path, "/resume") {
					method = http.MethodPost
				}
				status, body = s.call(method, path, nil)
				require.Equal(t, http.StatusBadRequest, status, "%s: %s", path, string(body))
			}
		})
	}
	require.Equal(t, shapes["standalone"], shapes["embedded"], "self subscription shape differs between deployments")
}
