//go:build integration

package tests

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/controlplane"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/http/router"
	httproutes "github.com/open-rails/openrails/internal/http/routes"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/pkg/api"
	"github.com/open-rails/openrails/pkg/identity"
	"github.com/open-rails/openrails/pkg/merchant"
)

type testDelegatedResolver struct {
	subject string
	perms   []string
}

const merchantDelegatedTestToken = "aaa.bbb.ccc"

func (r testDelegatedResolver) ResolveDelegated(context.Context, string, string) (*controlplane.ResolvedDelegated, error) {
	return &controlplane.ResolvedDelegated{
		MerchantID:       dbtest.TestMerchantID,
		Merchant:         dbtest.TestMerchantSlug,
		MerchantSlug:     dbtest.TestMerchantSlug,
		DelegatedSubject: r.subject,
		Permissions:      r.perms,
	}, nil
}

// newHostSeamAdminRouter mounts the resource-named /v1/merchant support surface
// with a host-seam delegated principal carrying the given merchant permissions.
func newHostSeamAdminRouter(t *testing.T, suite *TestContainerSuite, subject string, perms []string) http.Handler {
	t.Helper()
	mux := http.NewServeMux()
	rr := router.NewMux(mux, "/v1/merchant", suite.App.Runtime)
	httproutes.RegisterMerchantActionRoutes(rr, suite.App.Runtime, httproutes.Options{
		Gate: httproutes.NewGate(httproutes.GateOptions{DelegatedResolver: testDelegatedResolver{subject: subject, perms: perms}}),
	})
	return mux
}

// TestAdminUserDetailComposite_Delegated validates the #528 delegated /v1/merchant
// surface end-to-end against Postgres: a merchant admin holding
// merchant:customer-settings:read reads a user's COMPOSITE billing detail, which
// embeds the entitlements section (plus the payment_methods + product_access
// sections introduced in #528 increment 4).
func TestAdminUserDetailComposite_Delegated(t *testing.T) {
	suite := getSharedTestSuite(t)
	admin := newHostSeamAdminRouter(t, suite, "b3333333-3333-4333-8333-333333333333",
		[]string{controlplane.PermMerchantCustomerSettingsRead})

	userID := uuid.New().String()
	now := time.Now().UTC()
	endAt := now.Add(30 * 24 * time.Hour)
	adminSourceID := uuid.New()
	// The RLS-aware Runtime.DB insert needs the merchant pinned on the context.
	mctx := merchant.WithID(context.Background(), dbtest.TestMerchantID)
	suite.InsertEntitlement(mctx, &models.Entitlement{
		ID:          uuid.New(),
		CustomerID:  suite.ensureCustomer(mctx, userID),
		Entitlement: "premium",
		StartAt:     now.Add(-time.Second),
		EndAt:       &endAt,
		SourceID:    &adminSourceID,
		SourceType:  models.EntitlementSourceAdmin,
		CreatedAt:   now,
		UpdatedAt:   now,
	})
	payer := identity.CustomerIDFromString(userID)
	_, err := suite.App.Runtime.MoneyService.Deposit(mctx, money.DepositParams{
		CustomerID: &payer,
		Invoker:    userID,
		Currency:   money.DefaultCurrency,
		Amount:     1200,
		Source:     "admin-user-detail-test",
	})
	require.NoError(t, err)
	_, err = suite.App.Runtime.MoneyService.AccrueOwed(mctx, payer, "EUR", "usage", "admin-user-detail-"+uuid.NewString(), 700)
	require.NoError(t, err)
	seededBalances, err := suite.App.Runtime.MoneyService.ListBalancesForCustomer(mctx, payer)
	require.NoError(t, err)
	require.Len(t, seededBalances, 2)
	pm := suite.CreateTestPaymentMethod(userID)

	req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/v1/merchant/customers/%s", userID), nil)
	req.Header.Set("Authorization", "Bearer "+merchantDelegatedTestToken)
	w := httptest.NewRecorder()
	admin.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	var resp map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp), w.Body.String())
	require.Equal(t, userID, resp["customer_id"])
	ents, ok := resp["entitlements"].([]any)
	require.True(t, ok, "composite must embed entitlements: %s", w.Body.String())
	require.Len(t, ents, 1, "the seeded entitlement should appear in the composite")
	_, ok = resp["payment_methods"]
	require.True(t, ok, "composite must embed a payment_methods section")
	methods, ok := resp["payment_methods"].([]any)
	require.True(t, ok, "payment_methods must be an array: %s", w.Body.String())
	require.Len(t, methods, 1)
	require.Equal(t, api.FormatPaymentMethodID(pm.ID), methods[0].(map[string]any)["id"])
	_, ok = resp["subscription"]
	require.False(t, ok, "legacy singular subscription field should be gone: %s", w.Body.String())
	subs, ok := resp["subscriptions"].([]any)
	require.True(t, ok, "composite must embed subscriptions array: %s", w.Body.String())
	require.Empty(t, subs)
	creditBalances, ok := resp["credit_balance"].([]any)
	require.True(t, ok, "composite must embed credit_balance: %s", w.Body.String())
	require.Len(t, creditBalances, 2)
	byCurrency := map[string]map[string]any{}
	for _, item := range creditBalances {
		row := item.(map[string]any)
		byCurrency[row["currency"].(string)] = row
	}
	require.EqualValues(t, 1200, byCurrency[money.DefaultCurrency]["balance"])
	require.EqualValues(t, 0, byCurrency[money.DefaultCurrency]["outstanding_owed_amount"])
	require.EqualValues(t, 0, byCurrency["EUR"]["balance"])
	require.EqualValues(t, 700, byCurrency["EUR"]["outstanding_owed_amount"])
	_, ok = resp["product_access"]
	require.True(t, ok, "composite must embed a product_access section")
}

// TestAdminSurface_RejectsWithoutDelegatedPrincipal confirms the delegated merchant
// gate fails closed: a delegated principal that lacks billing:read cannot read
// the composite (403), proving the surface is gated, not open.
func TestAdminSurface_RejectsWithoutBillingRead(t *testing.T) {
	suite := getSharedTestSuite(t)
	// A principal with only a write scope, missing billing:read.
	admin := newHostSeamAdminRouter(t, suite, "b4444444-4444-4444-8444-444444444444",
		[]string{controlplane.PermMerchantSubscriptionsUpdate})

	req := httptest.NewRequest(http.MethodGet, "/v1/merchant/customers/"+uuid.New().String(), nil)
	req.Header.Set("Authorization", "Bearer "+merchantDelegatedTestToken)
	w := httptest.NewRecorder()
	admin.ServeHTTP(w, req)
	require.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
}
