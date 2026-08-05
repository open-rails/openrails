//go:build integration

package integrationharness

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/controlplane"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/pkg/merchant"
)

// or#782 task 3: the data-plane half of the cross-merchant isolation suite.
// cross_merchant_isolation_test.go proves the CREDENTIAL surfaces (api keys,
// remote-application JWTs, delegated admins); these prove the merchant's own
// BUSINESS surfaces — catalog, customers, subscriptions, payments and inbound
// webhooks — over the same real HTTP boundary against a server running as
// openrails_app, where RLS actually enforces.
//
// Until dev connected as a NOBYPASSRLS role these routes were only ever
// exercised through a superuser, so a handler that leans on RLS for scoping
// (rather than carrying its own merchant predicate) leaked everything and every
// dev test still passed. One focused test per surface, each asserting BOTH
// halves: merchant A cannot read or mutate B's row, and B's row is undamaged.

// isolationPair is two live merchants on one standalone server: A is the
// bootstrapped test merchant, B is freshly provisioned. Both tokens carry the
// same permission set, so a denial is never explainable by permissions.
type isolationPair struct {
	h       *Harness
	surface *Surface
	aToken  string
	bToken  string
	bSlug   string
	bID     merchant.ID
}

func newIsolationPair(t *testing.T, ctx context.Context) isolationPair {
	t.Helper()
	h := New(t, ctx)
	surface := h.StartStandalone("usd") // runs as openrails_app: real RLS enforces
	b := surface.ProvisionOwnedMerchant("iso" + strings.ReplaceAll(uuid.NewString(), "-", "")[:16])

	perms := []string{
		controlplane.PermMerchantCatalogRead,
		controlplane.PermMerchantCatalogUpdate,
		controlplane.PermMerchantCustomerSettingsRead,
		controlplane.PermMerchantCustomerSettingsUpdate,
		controlplane.PermMerchantPaymentsRead,
		controlplane.PermMerchantSubscriptionsRead,
		controlplane.PermMerchantSubscriptionsUpdate,
	}
	return isolationPair{
		h:       h,
		surface: surface,
		aToken:  surface.MintAPIKey(dbtest.TestMerchantSlug, "iso-a-"+uuid.NewString(), perms),
		bToken:  surface.MintAPIKey(b.MerchantSlug, "iso-b-"+uuid.NewString(), perms),
		bSlug:   b.MerchantSlug,
		bID:     b.MerchantID,
	}
}

func (p isolationPair) url(path string) string { return p.surface.BaseURL + path }

// bRows are merchant B's seeded billing rows. They are written through an
// RLS-ENFORCING merchant-pinned pool (h.MerchantPool), i.e. the production
// posture — the fixture proves B can write its OWN rows, it never bypasses.
type bRows struct {
	customerID     uuid.UUID
	productID      uuid.UUID
	priceID        uuid.UUID
	subscriptionID uuid.UUID
	paymentID      uuid.UUID
}

func seedMerchantBillingRows(t *testing.T, ctx context.Context, pool *pgxpool.Pool, mid merchant.ID) bRows {
	t.Helper()
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
	out := bRows{
		customerID:     uuid.New(),
		productID:      uuid.New(),
		priceID:        uuid.New(),
		subscriptionID: uuid.New(),
		paymentID:      uuid.New(),
	}
	_, err := pool.Exec(ctx, `
		INSERT INTO openrails.customers (id, merchant_id) VALUES ($1, $2)
	`, out.customerID, mid.UUID())
	require.NoError(t, err, "seed customer")

	_, err = pool.Exec(ctx, `
		INSERT INTO openrails.products (id, merchant_id, key, display_name)
		VALUES ($1, $2, $3, $3)
	`, out.productID, mid.UUID(), "isoprod-"+suffix)
	require.NoError(t, err, "seed product")

	_, err = pool.Exec(ctx, `
		INSERT INTO openrails.prices (id, merchant_id, product_id, key, amount, currency)
		VALUES ($1, $2, $3, $4, 1000000, 'USD')
	`, out.priceID, mid.UUID(), out.productID, "isoprice-"+suffix)
	require.NoError(t, err, "seed price")

	pspID := dbtest.EnsureTestPSP(ctx, t, pool, mid.UUID(), "nmi")

	_, err = pool.Exec(ctx, `
		INSERT INTO openrails.subscriptions
			(id, merchant_id, customer_id, product_id, price_id, status, rail, rail_subscription_id, psp_id)
		VALUES ($1, $2, $3, $4, $5, 'active', 'nmi', $6, $7)
	`, out.subscriptionID, mid.UUID(), out.customerID, out.productID, out.priceID, "isosub-"+suffix, pspID)
	require.NoError(t, err, "seed subscription")

	_, err = pool.Exec(ctx, `
		INSERT INTO openrails.payments
			(id, merchant_id, customer_id, price_id, subscription_id, rail, transaction_id,
			 amount, list_amount, currency, status, psp_id)
		VALUES ($1, $2, $3, $4, $5, 'nmi', $6, 1000000, 1000000, 'USD', 'completed', $7)
	`, out.paymentID, mid.UUID(), out.customerID, out.priceID, out.subscriptionID, "isotxn-"+suffix, pspID)
	require.NoError(t, err, "seed payment")

	return out
}

// TestCrossMerchantCatalogIsolationHTTP: merchant B's product, created through
// the live catalog write route, is invisible and immutable to merchant A.
func TestCrossMerchantCatalogIsolationHTTP(t *testing.T) {
	ctx := context.Background()
	p := newIsolationPair(t, ctx)

	key := "isocat" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
	status, body := requestJSON(t, http.MethodPost, p.url("/v1/merchant/catalog/products"), p.bToken, map[string]any{
		"key":          key,
		"display_name": "Merchant B Product",
	})
	require.Equalf(t, http.StatusCreated, status, "B creates its own product: %s", string(body))
	var created struct {
		ID string `json:"id"`
	}
	require.NoError(t, json.Unmarshal(body, &created))
	require.NotEmpty(t, created.ID)

	// A lists: B's product must not appear at all.
	status, body = requestJSON(t, http.MethodGet, p.url("/v1/merchant/catalog/products?limit=100"), p.aToken, nil)
	require.Equalf(t, http.StatusOK, status, "A lists its own catalog: %s", string(body))
	require.NotContainsf(t, string(body), created.ID, "merchant A listed merchant B's product")
	require.NotContainsf(t, string(body), key, "merchant A listed merchant B's product key")

	// A reads it directly, by id and by the merchant-scoped key.
	status, body = requestJSON(t, http.MethodGet, p.url("/v1/merchant/catalog/products/"+created.ID), p.aToken, nil)
	require.Equalf(t, http.StatusNotFound, status, "A read B's product by id: %s", string(body))
	status, body = requestJSON(t, http.MethodGet, p.url("/v1/merchant/catalog/products/by-key/"+key), p.aToken, nil)
	require.Equalf(t, http.StatusNotFound, status, "A read B's product by key: %s", string(body))

	// A mutates it.
	status, body = requestJSON(t, http.MethodPatch, p.url("/v1/merchant/catalog/products/"+created.ID), p.aToken, map[string]any{
		"display_name": "Stolen By A",
	})
	require.NotContainsf(t, []int{http.StatusOK, http.StatusCreated, http.StatusNoContent}, status,
		"merchant A mutated merchant B's product: %s", string(body))

	// B's row is intact.
	status, body = requestJSON(t, http.MethodGet, p.url("/v1/merchant/catalog/products/"+created.ID), p.bToken, nil)
	require.Equalf(t, http.StatusOK, status, "B still reads its own product: %s", string(body))
	require.Contains(t, string(body), "Merchant B Product")
	require.NotContains(t, string(body), "Stolen By A")
}

// TestCrossMerchantCustomerIsolationHTTP: the #740 customer list/search and the
// per-customer profile route are merchant-scoped.
func TestCrossMerchantCustomerIsolationHTTP(t *testing.T) {
	ctx := context.Background()
	p := newIsolationPair(t, ctx)
	rows := seedMerchantBillingRows(t, ctx, p.h.MerchantPool(p.bID.UUID()), p.bID)

	status, body := requestJSON(t, http.MethodGet, p.url("/v1/merchant/customers?limit=100"), p.bToken, nil)
	require.Equalf(t, http.StatusOK, status, "B lists its own customers: %s", string(body))
	require.Containsf(t, string(body), rows.customerID.String(), "B must see its own customer: %s", string(body))

	status, body = requestJSON(t, http.MethodGet, p.url("/v1/merchant/customers?limit=100"), p.aToken, nil)
	require.Equalf(t, http.StatusOK, status, "A lists its own customers: %s", string(body))
	require.NotContainsf(t, string(body), rows.customerID.String(), "merchant A listed merchant B's customer")

	// Search by the exact id must not become a cross-merchant lookup either.
	status, body = requestJSON(t, http.MethodGet,
		p.url("/v1/merchant/customers?limit=100&q="+rows.customerID.String()), p.aToken, nil)
	require.Equalf(t, http.StatusOK, status, "A searches its own customers: %s", string(body))
	require.NotContainsf(t, string(body), rows.customerID.String(), "merchant A searched up merchant B's customer")

	// The per-customer billing profile answers with A's (empty) view, never B's.
	status, body = requestJSON(t, http.MethodGet,
		p.url("/v1/merchant/customers/"+rows.customerID.String()), p.aToken, nil)
	if status == http.StatusOK {
		var profile adminBillingProfileSnapshot
		require.NoError(t, json.Unmarshal(body, &profile))
		require.Emptyf(t, profile.CreditBalance, "merchant A saw merchant B's customer balances: %s", string(body))
	} else {
		require.Equalf(t, http.StatusNotFound, status, "A's view of B's customer must be empty or absent: %s", string(body))
	}
}

// TestCrossMerchantSubscriptionIsolationHTTP: read AND the destructive cancel.
func TestCrossMerchantSubscriptionIsolationHTTP(t *testing.T) {
	ctx := context.Background()
	p := newIsolationPair(t, ctx)
	rows := seedMerchantBillingRows(t, ctx, p.h.MerchantPool(p.bID.UUID()), p.bID)
	subID := rows.subscriptionID.String()

	status, body := requestJSON(t, http.MethodGet, p.url("/v1/merchant/subscriptions?limit=100"), p.bToken, nil)
	require.Equalf(t, http.StatusOK, status, "B lists its own subscriptions: %s", string(body))
	require.Containsf(t, string(body), subID, "B must see its own subscription: %s", string(body))

	status, body = requestJSON(t, http.MethodGet, p.url("/v1/merchant/subscriptions?limit=100"), p.aToken, nil)
	require.Equalf(t, http.StatusOK, status, "A lists its own subscriptions: %s", string(body))
	require.NotContainsf(t, string(body), subID, "merchant A listed merchant B's subscription")

	status, body = requestJSON(t, http.MethodGet, p.url("/v1/merchant/subscriptions/"+subID), p.aToken, nil)
	require.Equalf(t, http.StatusNotFound, status, "A read B's subscription: %s", string(body))

	// Cancellation is the highest-consequence write on this surface (or#664:
	// entitlements must never be lost to someone else's request).
	status, body = requestJSON(t, http.MethodPost, p.url("/v1/merchant/subscriptions/"+subID+"/cancel"), p.aToken, map[string]any{
		"cancel_type": "immediate",
	})
	require.NotContainsf(t, []int{http.StatusOK, http.StatusAccepted, http.StatusNoContent}, status,
		"merchant A cancelled merchant B's subscription: %s", string(body))

	status, body = requestJSON(t, http.MethodGet, p.url("/v1/merchant/subscriptions/"+subID), p.bToken, nil)
	require.Equalf(t, http.StatusOK, status, "B still reads its own subscription: %s", string(body))
	require.Containsf(t, string(body), "active", "B's subscription must still be active: %s", string(body))
}

// TestCrossMerchantPaymentIsolationHTTP: payment history and the refund route.
func TestCrossMerchantPaymentIsolationHTTP(t *testing.T) {
	ctx := context.Background()
	p := newIsolationPair(t, ctx)
	rows := seedMerchantBillingRows(t, ctx, p.h.MerchantPool(p.bID.UUID()), p.bID)
	payID := rows.paymentID.String()

	status, body := requestJSON(t, http.MethodGet, p.url("/v1/merchant/payments?limit=100"), p.bToken, nil)
	require.Equalf(t, http.StatusOK, status, "B lists its own payments: %s", string(body))
	require.Containsf(t, string(body), payID, "B must see its own payment: %s", string(body))

	status, body = requestJSON(t, http.MethodGet, p.url("/v1/merchant/payments?limit=100"), p.aToken, nil)
	require.Equalf(t, http.StatusOK, status, "A lists its own payments: %s", string(body))
	require.NotContainsf(t, string(body), payID, "merchant A listed merchant B's payment")

	status, body = requestJSON(t, http.MethodGet, p.url("/v1/merchant/payments/"+payID), p.aToken, nil)
	require.Equalf(t, http.StatusNotFound, status, "A read B's payment: %s", string(body))

	// A refund moves money out of B's account — the write that must never cross.
	status, body = requestJSON(t, http.MethodPost, p.url("/v1/merchant/payments/"+payID+"/refunds"), p.aToken, map[string]any{
		"amount": 1000000,
		"reason": "cross-merchant refund attempt",
	})
	require.NotContainsf(t, []int{http.StatusOK, http.StatusCreated, http.StatusAccepted}, status,
		"merchant A refunded merchant B's payment: %s", string(body))

	// B's customer payment history is likewise A-invisible.
	status, body = requestJSON(t, http.MethodGet,
		p.url("/v1/merchant/customers/"+rows.customerID.String()+"/payments"), p.aToken, nil)
	require.NotContainsf(t, string(body), payID,
		"merchant A read merchant B's customer payment history (status %d)", status)
}

// TestCrossMerchantWebhookIsolationHTTP: inbound provider webhooks are the one
// unauthenticated write surface — the caller presents a signature, not a token.
// A merchant's webhook secret must therefore authorize delivery into THAT
// merchant only, and the state a delivery creates must land under the addressed
// merchant alone.
//
// Both merchants are freshly provisioned here (not the shared test merchant):
// psps identities are globally unique and other tests in this package arm the
// test merchant's stripe account, so a private pair keeps rail resolution
// unambiguous.
func TestCrossMerchantWebhookIsolationHTTP(t *testing.T) {
	ctx := context.Background()
	h := New(t, ctx)
	surface := h.StartStandalone("usd")
	rt := surface.App().Runtime
	require.NotNil(t, rt)

	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
	a := surface.ProvisionOwnedMerchant("isowha" + suffix)
	b := surface.ProvisionOwnedMerchant("isowhb" + suffix)
	aSecret, bSecret := "whsec_a_"+suffix, "whsec_b_"+suffix

	arm := func(m OwnedMerchant, tag, secret string) {
		t.Helper()
		SeedRailMerchantAccounts(ctx, t, rt, m.MerchantID, config.PSPSet{
			"stripe-iso-" + tag: {
				Rail:      models.RailStripe,
				AccountID: "acct_iso_" + tag + "_" + suffix,
				Stripe:    &config.StripeRailConfig{SecretKey: "sk_test_" + tag + "_" + suffix, WebhookSigningSecret: secret},
			},
		})
	}
	arm(a, "a", aSecret)
	arm(b, "b", bSecret)

	stripeCustomer := "cus_iso_" + suffix
	event := []byte(`{"id":"evt_iso_` + suffix + `","type":"customer.created","data":{"object":{"id":"` + stripeCustomer + `"}}}`)
	// or#893: standalone's ONE webhook surface addresses the receiving PSP
	// account, not a merchant slug — the merchant is derived from the globally
	// unique account row. The isolation property is unchanged: the signature is
	// verified with THAT account's own secret.
	post := func(tag, secret string) (int, string) {
		t.Helper()
		return postSignedStripeWebhook(t, surface.BaseURL+"/v1/webhooks/stripe/acct_iso_"+tag+"_"+suffix, event, secret)
	}

	// The signature is verified against the ADDRESSED account's own secret, so
	// neither merchant's secret can deliver into the other.
	status, resp := post("a", bSecret)
	require.Equalf(t, http.StatusUnauthorized, status, "B's webhook secret delivered into A: %s", resp)
	status, resp = post("b", aSecret)
	require.Equalf(t, http.StatusUnauthorized, status, "A's webhook secret delivered into B: %s", resp)

	// B's own secret delivers into B.
	status, resp = post("b", bSecret)
	require.Equalf(t, http.StatusOK, status, "B's own webhook secret must deliver on B's path: %s", resp)

	// or#893: the merchant-slug alias standalone used to mount alongside this is
	// gone — one surface, one resolution rule.
	status, resp = postSignedStripeWebhook(t, surface.BaseURL+"/v1/merchants/"+b.MerchantSlug+"/webhooks/stripe", event, bSecret)
	require.Equalf(t, http.StatusNotFound, status, "the retired merchant-slug webhook alias must not be served: %s", resp)

	// The delivered state belongs to B alone: A's authenticated reads of the
	// surfaces a provider event writes never see it.
	aToken := surface.MintAPIKey(a.MerchantSlug, "iso-wh-a-"+uuid.NewString(), []string{
		controlplane.PermMerchantCustomerSettingsRead,
		controlplane.PermMerchantPaymentsRead,
		controlplane.PermMerchantSubscriptionsRead,
	})
	for _, path := range []string{
		"/v1/merchant/customers?limit=100",
		"/v1/merchant/payments?limit=100",
		"/v1/merchant/subscriptions?limit=100",
	} {
		status, out := requestJSON(t, http.MethodGet, surface.BaseURL+path, aToken, nil)
		require.Equalf(t, http.StatusOK, status, "A reads %s: %s", path, string(out))
		require.NotContainsf(t, string(out), stripeCustomer, "webhook state delivered to B surfaced under A via %s", path)
		require.NotContainsf(t, string(out), "acct_iso_b_"+suffix, "B's PSP surfaced under A via %s", path)
	}
}

func postSignedStripeWebhook(t *testing.T, url string, body []byte, secret string) (int, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, url, bytes.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Stripe-Signature", stripeSignature(secret, body))
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, string(raw)
}
