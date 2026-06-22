//go:build integration

package tests

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/controlplane"
	"github.com/open-rails/openrails/internal/dbtest"
	ginmw "github.com/open-rails/openrails/internal/http/middleware/ginmw"
	ginroutes "github.com/open-rails/openrails/internal/http/routes/ginroutes"
	"github.com/open-rails/openrails/internal/modules/admission"
	"github.com/open-rails/openrails/internal/modules/admission/spendgate"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/pkg/identity"
	billingservice "github.com/open-rails/openrails/pkg/service"
)

// #566 org-as-PAYER treasury surface, exercised over HTTP against a real
// database. The org reads its OWN balance/ledger/usage/invoices, configures its
// caps, manages payment methods, and pre-pays — all scoped to the org's payable
// subject (the merchant/org uuid), gated by customer:* permissions, with NO
// consumer routes mounted. The shared /v1/me money handlers operate on the org
// balance because OrgTreasuryScopeRequired rebinds the acting payer to the org.

// allCustomerTreasuryPerms is the full payer-surface grant.
var allCustomerTreasuryPerms = []string{
	controlplane.PermCustomerBalanceRead,
	controlplane.PermCustomerBillingUpdate,
	controlplane.PermCustomerPaymentMethodsUpdate,
	controlplane.PermCustomerCheckoutCreate,
	controlplane.PermCustomerSpendDelegationsRead,
	controlplane.PermCustomerSpendDelegationsUpdate,
}

// newOrgTreasuryServer mounts the org-treasury surface behind a host-seam
// principal carrying perms. The principal's MerchantID is always the test org;
// subject is irrelevant on this surface (the payer is rebound to the org), so a
// random subject proves the rebind is what scopes the balance.
func newOrgTreasuryServer(t *testing.T, suite *TestContainerSuite, perms []string) *httptest.Server {
	t.Helper()
	gin.SetMode(gin.TestMode)
	router := gin.New()
	ginroutes.RegisterOrgTreasuryRoutes(router.Group("/v1/orgs"), suite.App.Runtime,
		ginmw.DelegatedPrincipalRequired(hostSeamAuthenticator{subject: uuid.NewString(), perms: perms}))
	srv := httptest.NewServer(router)
	t.Cleanup(srv.Close)
	return srv
}

func orgPath(suffix string) string {
	return "/v1/orgs/" + dbtest.TestMerchantSlug + suffix
}

func TestOrgTreasuryPayerSurface_HTTPFullLoopAndScoping(t *testing.T) {
	suite := getSharedTestSuite(t)
	ctx := dbtest.WithTestMerchant(context.Background())

	svc, err := billingservice.New(suite.App.Runtime)
	require.NoError(t, err)

	// EUR keeps this test's org account independent of the USD draw-down E2E; the
	// before/after delta assertions tolerate any balance carried in from fixtures.
	const currency = "EUR"
	orgPayer := identity.CustomerID(dbtest.TestMerchantID.UUID())

	srv := newOrgTreasuryServer(t, suite, allCustomerTreasuryPerms)

	// Baseline balance for the org payer in this currency, read over HTTP.
	before := getOrgBalance(t, srv, currency)

	// Org loads credits (the post-confirmation effect of a checkout pre-pay).
	depositID := uuid.New()
	const deposit = 4_200_000
	_, err = svc.DepositCredits(ctx, billingservice.DepositCreditsRequest{
		CustomerID: &orgPayer,
		Invoker:    orgPayer.UUID().String(),
		Currency:   currency,
		Amount:     deposit,
		Source:     "test_org_prepay",
		SourceID:   &depositID,
	})
	require.NoError(t, err)

	// --- GET balance: reflects the org payer's loaded credits, NOT an individual. ---
	require.EqualValues(t, before+deposit, getOrgBalance(t, srv, currency),
		"org balance must reflect credits loaded onto the org payable subject")

	// --- GET transactions: the newest entry is this org deposit, keyed to the org. ---
	resp := requestOrgTreasuryJSON(t, srv, http.MethodGet, orgPath("/transactions?currency="+currency+"&limit=5"), nil)
	require.Equal(t, http.StatusOK, resp.status, resp.body)
	txns := decodeJSONObject(t, resp.body)
	list, ok := txns["transactions"].([]any)
	require.True(t, ok, resp.body)
	require.NotEmpty(t, list, resp.body)
	newest, ok := list[0].(map[string]any)
	require.True(t, ok)
	require.Equal(t, orgPayer.UUID().String(), newest["customer_id"], "transaction is scoped to the org payable subject")
	require.EqualValues(t, deposit, newest["amount"])

	// --- PUT settings: the org sets self-imposed caps (billing mode stays
	// merchant-granted — the shared handler refuses platform policy fields). ---
	resp = requestOrgTreasuryJSON(t, srv, http.MethodPut, orgPath("/settings"), map[string]any{
		"currency":            currency,
		"max_spend_per_day":   1_000_000,
		"max_spend_per_month": 9_000_000,
	})
	require.Equal(t, http.StatusOK, resp.status, resp.body)
	stored := decodeJSONObject(t, resp.body)
	require.EqualValues(t, 1_000_000, stored["max_spend_per_day"])
	require.EqualValues(t, 9_000_000, stored["max_spend_per_month"])
	require.NotContains(t, stored, "billing_mode", "billing mode is merchant-granted, not customer self-service")

	// --- GET usage / payments / invoices / payment-methods: all reachable and
	// scoped, returning the org's (empty) payer state. ---
	for _, p := range []string{
		"/usage?currency=" + currency,
		"/payments",
		"/invoices",
		"/payment-methods",
	} {
		resp = requestOrgTreasuryJSON(t, srv, http.MethodGet, orgPath(p), nil)
		require.Equal(t, http.StatusOK, resp.status, "GET %s: %s", p, resp.body)
	}

	// --- PUT + GET spend-delegations: the org's balance-sharing policy. ---
	invoker := uuid.NewString()
	resp = requestOrgTreasuryJSON(t, srv, http.MethodPut, orgPath("/spend-delegations"), map[string]any{
		"delegations": []map[string]any{{
			"scope":     "invoker",
			"scope_key": invoker,
			"windows":   []map[string]any{{"key": "day", "window_seconds": 86400, "limit": 1000, "currency": currency}},
		}},
	})
	require.Equal(t, http.StatusOK, resp.status, resp.body)
	resp = requestOrgTreasuryJSON(t, srv, http.MethodGet, orgPath("/spend-delegations"), nil)
	require.Equal(t, http.StatusOK, resp.status, resp.body)
	require.Len(t, decodeDelegations(t, resp.body), 1)
}

func TestOrgTreasuryPayerSurface_PermissionSplit(t *testing.T) {
	suite := getSharedTestSuite(t)

	// A read-only finance persona: customer:balance:read ONLY.
	srv := newOrgTreasuryServer(t, suite, []string{controlplane.PermCustomerBalanceRead})

	// Reads are allowed.
	resp := requestOrgTreasuryJSON(t, srv, http.MethodGet, orgPath("/balance?currency=USD"), nil)
	require.Equal(t, http.StatusOK, resp.status, resp.body)

	// Every write/spend route is forbidden — distinct grants, not one bundle.
	forbidden := []struct {
		method, path string
		body         any
	}{
		{http.MethodPut, "/settings", map[string]any{"currency": "USD", "max_spend_per_day": 1}},
		{http.MethodGet, "/payment-methods", nil},
		{http.MethodPost, "/checkout", map[string]any{"payment": map[string]any{"processor": "stripe"}}},
		{http.MethodPut, "/spend-delegations", map[string]any{"delegations": []any{}}},
	}
	for _, f := range forbidden {
		resp = requestOrgTreasuryJSON(t, srv, f.method, orgPath(f.path), f.body)
		require.Equal(t, http.StatusForbidden, resp.status, "%s %s must require its own customer:* grant: %s", f.method, f.path, resp.body)
	}
}

func TestOrgTreasuryPayerSurface_MerchantTokenAndCrossOrgRejected(t *testing.T) {
	suite := getSharedTestSuite(t)

	// Namespace separation: a merchant-only (seller hat) token holds NO customer:*
	// grant, so it cannot reach any org-payer route.
	merchantSrv := newOrgTreasuryServer(t, suite, []string{
		controlplane.PermMerchantSettingsRead,
		controlplane.PermMerchantCatalogUpdate,
	})
	resp := requestOrgTreasuryJSON(t, merchantSrv, http.MethodGet, orgPath("/balance?currency=USD"), nil)
	require.Equal(t, http.StatusForbidden, resp.status, "merchant:* token must not reach a customer:* route: %s", resp.body)

	// Cross-org: a fully-granted customer token may act ONLY on its OWN org. The
	// host principal resolves to the test org, so a different :org_id is a 403.
	orgSrv := newOrgTreasuryServer(t, suite, allCustomerTreasuryPerms)
	resp = requestOrgTreasuryJSON(t, orgSrv, http.MethodGet, "/v1/orgs/"+uuid.NewString()+"/balance?currency=USD", nil)
	require.Equal(t, http.StatusForbidden, resp.status, "cross-org access must be org_scope_mismatch: %s", resp.body)
}

func TestOrgTreasuryPayerSurface_NoConsumerRoutes(t *testing.T) {
	suite := getSharedTestSuite(t)
	// Full customer grant — proving the routes are ABSENT, not merely forbidden.
	srv := newOrgTreasuryServer(t, suite, allCustomerTreasuryPerms)

	for _, p := range []string{
		"/products",
		"/products/" + uuid.NewString() + "/access",
		"/entitlements/active",
		"/subscriptions",
		"/notifications",
	} {
		resp := requestOrgTreasuryJSON(t, srv, http.MethodGet, orgPath(p), nil)
		require.Equal(t, http.StatusNotFound, resp.status, "consumer route %s must not exist on the org surface: %s", p, resp.body)
	}
}

// TestOrgTreasuryPayer_DelegatedDrawDownE2E is the Tensorhub use-case: the org
// loads credits, sets a per-invoker spend-delegation window over HTTP, and a
// delegated invoker draws the ORG balance down THROUGH that window while a
// second invoker is independently metered (per-invoker, not pooled — #563).
func TestOrgTreasuryPayer_DelegatedDrawDownE2E(t *testing.T) {
	suite := getSharedTestSuite(t)
	ctx := dbtest.WithTestMerchant(context.Background())
	orgPayer := identity.CustomerID(dbtest.TestMerchantID.UUID())

	svc, err := billingservice.New(suite.App.Runtime)
	require.NoError(t, err)

	// Org loads a large prepaid balance (the post-checkout effect) so the balance
	// never gates — the spend-delegation WINDOW is what must bind.
	depositID := uuid.New()
	_, err = svc.DepositCredits(ctx, billingservice.DepositCreditsRequest{
		CustomerID: &orgPayer,
		Invoker:    orgPayer.UUID().String(),
		Currency:   money.DefaultCurrency,
		Amount:     1_000_000_000,
		Source:     "test_org_prepay",
		SourceID:   &depositID,
	})
	require.NoError(t, err)

	t.Cleanup(func() {
		_, _ = suite.Pool.Exec(context.Background(),
			"DELETE FROM openrails.invoker_spend_limits WHERE customer_id = $1", orgPayer.UUID())
	})

	srv := newOrgTreasuryServer(t, suite, allCustomerTreasuryPerms)

	// The org confirms its loaded balance over HTTP.
	require.GreaterOrEqual(t, getOrgBalance(t, srv, money.DefaultCurrency), int64(1_000_000_000))

	// The org grants alice a $1000-per-day window — over HTTP, the real surface.
	alice := "user:alice"
	resp := requestOrgTreasuryJSON(t, srv, http.MethodPut, orgPath("/spend-delegations"), map[string]any{
		"delegations": []map[string]any{{
			"scope":     "invoker",
			"scope_key": alice,
			"windows":   []map[string]any{{"key": "day", "window_seconds": 86400, "limit": 1000, "currency": money.DefaultCurrency}},
		}},
	})
	require.Equal(t, http.StatusOK, resp.status, resp.body)

	// Wire the spendgate-backed admitter over the SAME Postgres + Redis the org
	// just wrote its policy to (the production admission path, #513).
	adm := admission.NewAdmitter(
		money.NewMoneyService(suite.App.Runtime.DB),
		spendgate.New(suite.RedisClient),
		admission.NewSpendgatePolicyLoader(
			admission.NewPayerSpendLimitStore(suite.App.Runtime.DB),
			admission.NewInvokerSpendLimitStore(suite.App.Runtime.DB),
			nil,
		),
	)

	drawDown := func(invoker string, amount int64) admission.AdmitDecision {
		d, derr := adm.Admit(ctx, admission.AdmitRequest{
			CustomerID:      orgPayer,
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

	// alice draws the org balance down within her window.
	require.True(t, drawDown(alice, 600).Allowed, "first 600 fits alice's 1000 window")
	require.False(t, drawDown(alice, 600).Allowed, "600+600 breaches alice's 1000 window")

	// bob has no grant of his own; alice's window never widened his access
	// (per-invoker isolation, not a pooled role counter).
	bob := drawDown("user:bob", 100)
	require.False(t, bob.Allowed, "an ungranted invoker cannot spend the org balance")
	require.Equal(t, admission.DenyDelegatedSpendNotAllowed, bob.DenyCode)
}

func getOrgBalance(t *testing.T, srv *httptest.Server, currency string) int64 {
	t.Helper()
	resp := requestOrgTreasuryJSON(t, srv, http.MethodGet, orgPath("/balance?currency="+currency), nil)
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
