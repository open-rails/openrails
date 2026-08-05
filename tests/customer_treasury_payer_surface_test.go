//go:build integration

package tests

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/controlplane"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/http/middleware"
	httprouter "github.com/open-rails/openrails/internal/http/router"
	httproutes "github.com/open-rails/openrails/internal/http/routes"
	"github.com/open-rails/openrails/internal/http/routesurface"
	"github.com/open-rails/openrails/internal/modules/admission"
	"github.com/open-rails/openrails/internal/modules/admission/spendgate"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/pkg/identity"
	billingservice "github.com/open-rails/openrails/pkg/service"
)

// #567 customer-as-PAYER treasury surface, exercised over HTTP against a real
// database. The customer reads its OWN balance/ledger/usage/invoices, configures
// its caps, manages payment methods, and pre-pays — all scoped to the customer's
// payable subject (the customer/merchant uuid), gated by customer:* permissions,
// with NO consumer routes mounted. The shared /v1/me money handlers operate on
// the customer balance because CustomerScopeRequired rebinds the acting payer to
// the customer.

// allCustomerTreasuryPerms is the full payer-surface grant.
var allCustomerTreasuryPerms = []string{
	controlplane.PermCustomerBalanceRead,
	controlplane.PermCustomerBillingUpdate,
	controlplane.PermCustomerPaymentMethodsUpdate,
	controlplane.PermCustomerCheckoutCreate,
	controlplane.PermCustomerSpendDelegationsRead,
	controlplane.PermCustomerSpendDelegationsUpdate,
}

// newCustomerTreasuryServer mounts the customer-treasury surface behind a host-seam
// principal carrying perms. The principal's MerchantID is always the test customer;
// subject is irrelevant on this surface (the payer is rebound to the customer), so a
// random subject proves the rebind is what scopes the balance.
func newCustomerTreasuryServer(t *testing.T, suite *TestContainerSuite, perms []string) *httptest.Server {
	t.Helper()
	router := http.NewServeMux()
	httproutes.RegisterCustomerTreasuryRoutes(httprouter.NewMux(router, "/v1/customers", suite.App.Runtime), suite.App.Runtime,
		middleware.DelegatedPrincipalRequired(hostSeamAuthenticator{subject: uuid.NewString(), perms: perms}), routesurface.AllProviderRoutes())
	srv := httptest.NewServer(router)
	t.Cleanup(srv.Close)
	return srv
}

func customerPath(suffix string) string {
	return "/v1/customers/" + dbtest.TestMerchantSlug + suffix
}

func TestCustomerTreasuryPayerSurface_HTTPFullLoopAndScoping(t *testing.T) {
	suite := getSharedTestSuite(t)
	ctx := suite.MerchantCtx()

	svc, err := billingservice.New(suite.App.Runtime)
	require.NoError(t, err)

	// EUR keeps this test's customer account independent of the USD draw-down E2E; the
	// before/after delta assertions tolerate any balance carried in from fixtures.
	const currency = "EUR"
	customerPayer := identity.CustomerID(dbtest.TestMerchantID.UUID())

	srv := newCustomerTreasuryServer(t, suite, allCustomerTreasuryPerms)

	// Baseline balance for the customer payer in this currency, read over HTTP.
	before := getCustomerBalance(t, srv, currency)

	// Customer loads credits (the post-confirmation effect of a checkout pre-pay).
	depositID := uuid.New()
	const deposit = 4_200_000
	_, err = svc.DepositCredits(ctx, billingservice.DepositCreditsRequest{
		CustomerID: &customerPayer,
		Invoker:    customerPayer.UUID().String(),
		Currency:   currency,
		Amount:     deposit,
		Key:        money.MustIdempotencyKey(money.OpDeposit, "test_customer_prepay", depositID.String()),
	})
	require.NoError(t, err)

	// --- GET balance: reflects the customer payer's loaded credits, NOT an individual. ---
	require.EqualValues(t, before+deposit, getCustomerBalance(t, srv, currency),
		"customer balance must reflect credits loaded onto the customer payable subject")

	// --- GET transactions: the newest entry is this customer deposit, keyed to the customer. ---
	resp := requestCustomerTreasuryJSON(t, srv, http.MethodGet, customerPath("/transactions?currency="+currency+"&limit=5"), nil)
	require.Equal(t, http.StatusOK, resp.status, resp.body)
	txns := decodeJSONObject(t, resp.body)
	list, ok := txns["transactions"].([]any)
	require.True(t, ok, resp.body)
	require.NotEmpty(t, list, resp.body)
	newest, ok := list[0].(map[string]any)
	require.True(t, ok)
	require.Equal(t, customerPayer.UUID().String(), newest["customer_id"], "transaction is scoped to the customer payable subject")
	require.EqualValues(t, deposit, newest["amount"])

	// --- PUT settings: the customer sets self-imposed caps (billing mode stays
	// merchant-granted — the shared handler refuses platform policy fields). ---
	resp = requestCustomerTreasuryJSON(t, srv, http.MethodPut, customerPath("/settings"), map[string]any{
		"currency":              currency,
		"low_balance_threshold": 1_000_000,
	})
	require.Equal(t, http.StatusOK, resp.status, resp.body)
	stored := decodeJSONObject(t, resp.body)
	require.EqualValues(t, 1_000_000, stored["low_balance_threshold"])
	require.NotContains(t, stored, "billing_mode", "billing mode is merchant-granted, not customer self-service")

	// --- GET usage / payments / invoices / payment-methods: all reachable and
	// scoped, returning the customer's (empty) payer state. ---
	for _, p := range []string{
		"/usage?currency=" + currency,
		"/payments",
		"/invoices",
		"/payment-methods",
	} {
		resp = requestCustomerTreasuryJSON(t, srv, http.MethodGet, customerPath(p), nil)
		require.Equal(t, http.StatusOK, resp.status, "GET %s: %s", p, resp.body)
	}

	// --- PUT + GET spend-delegations: the customer's balance-sharing policy. ---
	invoker := uuid.NewString()
	resp = requestCustomerTreasuryJSON(t, srv, http.MethodPut, customerPath("/spend-delegations"), map[string]any{
		"delegations": []map[string]any{{
			"scope":     "invoker",
			"scope_key": invoker,
			"windows":   []map[string]any{{"key": "day", "window_seconds": 86400, "limit": 1000, "currency": currency}},
		}},
	})
	require.Equal(t, http.StatusOK, resp.status, resp.body)
	resp = requestCustomerTreasuryJSON(t, srv, http.MethodGet, customerPath("/spend-delegations"), nil)
	require.Equal(t, http.StatusOK, resp.status, resp.body)
	require.Len(t, decodeDelegations(t, resp.body), 1)
}

func TestCustomerTreasuryPayerSurface_PermissionSplit(t *testing.T) {
	suite := getSharedTestSuite(t)

	// A read-only finance persona: customer:balance:read ONLY.
	srv := newCustomerTreasuryServer(t, suite, []string{controlplane.PermCustomerBalanceRead})

	// Reads are allowed.
	resp := requestCustomerTreasuryJSON(t, srv, http.MethodGet, customerPath("/balance?currency=USD"), nil)
	require.Equal(t, http.StatusOK, resp.status, resp.body)

	// Every write/spend route is forbidden — distinct grants, not one bundle.
	forbidden := []struct {
		method, path string
		body         any
	}{
		{http.MethodPut, "/settings", map[string]any{"currency": "USD", "low_balance_threshold": 1}},
		{http.MethodGet, "/payment-methods", nil},
		{http.MethodPost, "/checkout", map[string]any{"payment": map[string]any{"rail": "stripe"}}},
		{http.MethodPut, "/spend-delegations", map[string]any{"delegations": []any{}}},
	}
	for _, f := range forbidden {
		resp = requestCustomerTreasuryJSON(t, srv, f.method, customerPath(f.path), f.body)
		require.Equal(t, http.StatusForbidden, resp.status, "%s %s must require its own customer:* grant: %s", f.method, f.path, resp.body)
	}
}

func TestCustomerTreasuryPayerSurface_MerchantTokenAndCrossCustomerRejected(t *testing.T) {
	suite := getSharedTestSuite(t)

	// Namespace separation: a merchant-only (seller hat) token holds NO customer:*
	// grant, so it cannot reach any customer-payer route.
	merchantSrv := newCustomerTreasuryServer(t, suite, []string{
		controlplane.PermMerchantSettingsRead,
		controlplane.PermMerchantCatalogUpdate,
	})
	resp := requestCustomerTreasuryJSON(t, merchantSrv, http.MethodGet, customerPath("/balance?currency=USD"), nil)
	require.Equal(t, http.StatusForbidden, resp.status, "merchant:* token must not reach a customer:* route: %s", resp.body)

	// Cross-customer: a fully-granted customer token may act ONLY on its OWN
	// balance. The host principal resolves to the test customer, so a different
	// :customer_id is a 403.
	customerSrv := newCustomerTreasuryServer(t, suite, allCustomerTreasuryPerms)
	resp = requestCustomerTreasuryJSON(t, customerSrv, http.MethodGet, "/v1/customers/"+uuid.NewString()+"/balance?currency=USD", nil)
	require.Equal(t, http.StatusForbidden, resp.status, "cross-customer access must be customer_scope_mismatch: %s", resp.body)
}

func TestCustomerTreasuryPayerSurface_NoConsumerRoutes(t *testing.T) {
	suite := getSharedTestSuite(t)
	// Full customer grant — proving the routes are ABSENT, not merely forbidden.
	srv := newCustomerTreasuryServer(t, suite, allCustomerTreasuryPerms)

	for _, p := range []string{
		"/products",
		"/products/" + uuid.NewString() + "/access",
		"/entitlements/active",
		"/subscriptions",
		"/notifications",
	} {
		resp := requestCustomerTreasuryJSON(t, srv, http.MethodGet, customerPath(p), nil)
		require.Equal(t, http.StatusNotFound, resp.status, "consumer route %s must not exist on the customer surface: %s", p, resp.body)
	}
}

// TestCustomerTreasuryPayer_DelegatedDrawDownE2E is the Tensorhub use-case: the customer
// loads credits, sets a per-invoker spend-delegation window over HTTP, and a
// delegated invoker draws the customer balance down THROUGH that window while a
// second invoker is independently metered (per-invoker, not pooled — #563).
func TestCustomerTreasuryPayer_DelegatedDrawDownE2E(t *testing.T) {
	suite := getSharedTestSuite(t)
	ctx := suite.MerchantCtx()
	customerPayer := identity.CustomerID(dbtest.TestMerchantID.UUID())

	svc, err := billingservice.New(suite.App.Runtime)
	require.NoError(t, err)

	// Customer loads a large prepaid balance (the post-checkout effect) so the balance
	// never gates — the spend-delegation WINDOW is what must bind.
	depositID := uuid.New()
	_, err = svc.DepositCredits(ctx, billingservice.DepositCreditsRequest{
		CustomerID: &customerPayer,
		Invoker:    customerPayer.UUID().String(),
		Currency:   money.DefaultCurrency,
		Amount:     1_000_000_000,
		Key:        money.MustIdempotencyKey(money.OpDeposit, "test_customer_prepay", depositID.String()),
	})
	require.NoError(t, err)

	t.Cleanup(func() {
		_, _ = suite.Pool.Exec(context.Background(),
			"DELETE FROM openrails.invoker_spend_limits WHERE customer_id = $1", customerPayer.UUID())
	})

	srv := newCustomerTreasuryServer(t, suite, allCustomerTreasuryPerms)

	// The customer confirms its loaded balance over HTTP.
	require.GreaterOrEqual(t, getCustomerBalance(t, srv, money.DefaultCurrency), int64(1_000_000_000))

	// The customer grants alice a $1000-per-day window — over HTTP, the real surface.
	alice := "user:alice"
	resp := requestCustomerTreasuryJSON(t, srv, http.MethodPut, customerPath("/spend-delegations"), map[string]any{
		"delegations": []map[string]any{{
			"scope":     "invoker",
			"scope_key": alice,
			"windows":   []map[string]any{{"key": "day", "window_seconds": 86400, "limit": 1000, "currency": money.DefaultCurrency}},
		}},
	})
	require.Equal(t, http.StatusOK, resp.status, resp.body)

	// Wire the spendgate-backed admitter over the SAME Postgres + Redis the customer
	// just wrote its policy to (the production admission path, #513).
	adm := admission.NewAdmitter(
		money.NewMoneyService(suite.App.Runtime.DB),
		spendgate.New(suite.RedisClient),
		admission.NewSpendgatePolicyLoader(
			admission.NewBillingPolicyStore(suite.App.Runtime.DB),
			admission.NewInvokerSpendLimitStore(suite.App.Runtime.DB),
			nil,
		),
	)

	drawDown := func(invoker string, amount int64) admission.AdmitDecision {
		d, derr := adm.Admit(ctx, admission.AdmitRequest{
			CustomerID:      customerPayer,
			Invoker:         invoker,
			InvokerType:     string(identity.InvokerTypeDelegated),
			Currency:        money.DefaultCurrency,
			EstimatedAmount: amount,
			Source:          "usage",
			SourceID:        uuid.NewString(),
			ExpiresAt:       suite.GetClock().Now().Add(time.Hour),
		})
		require.NoError(t, derr)
		return d
	}

	// alice draws the customer balance down within her window.
	require.True(t, drawDown(alice, 600).Allowed, "first 600 fits alice's 1000 window")
	require.False(t, drawDown(alice, 600).Allowed, "600+600 breaches alice's 1000 window")

	// bob has no grant of his own; alice's window never widened his access
	// (per-invoker isolation, not a pooled role counter).
	bob := drawDown("user:bob", 100)
	require.False(t, bob.Allowed, "an ungranted invoker cannot spend the customer balance")
	require.Equal(t, admission.DenyDelegatedSpendNotAllowed, bob.DenyCode)
}

func getCustomerBalance(t *testing.T, srv *httptest.Server, currency string) int64 {
	t.Helper()
	resp := requestCustomerTreasuryJSON(t, srv, http.MethodGet, customerPath("/balance?currency="+currency), nil)
	require.Equal(t, http.StatusOK, resp.status, resp.body)
	body := decodeJSONObject(t, resp.body)
	amt, ok := body["balance_amount"].(float64)
	require.True(t, ok, resp.body)
	return int64(amt)
}

func decodeJSONObject(t *testing.T, body string) map[string]any {
	t.Helper()
	var out map[string]any
	require.NoError(t, json.Unmarshal([]byte(body), &out), body)
	return out
}
