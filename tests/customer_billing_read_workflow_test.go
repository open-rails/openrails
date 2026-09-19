//go:build integration

package tests

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/pkg/api"
)

type listResponse[T any] struct {
	Object  string `json:"object"`
	Data    []T    `json:"data"`
	Total   int64  `json:"total"`
	Limit   int    `json:"limit"`
	Offset  int    `json:"offset"`
	HasMore bool   `json:"has_more"`
}

// The account page's read workflow uses the mounted handler and real database:
// public catalog, empty account, populated account, and another subject's view.
func TestCustomerBillingReadWorkflow(t *testing.T) {
	suite, token, userID := setupTestSuiteWithAuth(t)
	products := suite.SeedProducts()
	require.Len(t, products, 4)
	read := func(path, bearer string, out any) {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		w := httptest.NewRecorder()
		suite.Server.Handler().ServeHTTP(w, req)
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), out))
	}
	list := func(path, bearer string) []map[string]any {
		t.Helper()
		var out listResponse[map[string]any]
		read(path, bearer, &out)
		require.Equal(t, "list", out.Object)
		return out.Data
	}
	assertEmptyAccount := func(bearer string) {
		t.Helper()
		for _, path := range []string{"/v1/me/subscriptions?status=active", "/v1/me/subscriptions?status=all", "/v1/me/payments"} {
			require.Empty(t, list(path, bearer), path)
		}
		var status map[string]any
		read("/v1/me/status", bearer, &status)
		require.Nil(t, status["subscription"])
		require.Nil(t, status["next_renewal_at"])
		require.Empty(t, status["entitlements"])
	}

	var catalog listResponse[api.ProductObject]
	read("/v1/products?limit=100", "", &catalog)
	require.Equal(t, "list", catalog.Object)
	require.GreaterOrEqual(t, catalog.Total, int64(4))
	require.GreaterOrEqual(t, len(catalog.Data), 4)
	var premium *api.ProductObject
	for i := range catalog.Data {
		if catalog.Data[i].Key == "premium" {
			premium = &catalog.Data[i]
		}
	}
	require.NotNil(t, premium)
	require.Equal(t, "product", premium.Object)
	require.True(t, premium.Active)
	require.GreaterOrEqual(t, len(premium.Prices), 2)
	for _, term := range []struct {
		amount   int64
		interval string
	}{{9_990_000, "720h"}, {79_990_000, "8760h"}} {
		var found *api.PriceObject
		for i := range premium.Prices {
			p := &premium.Prices[i]
			if p.UnitAmount == term.amount && p.Currency == "USD" && p.Recurring != nil && p.Recurring.Interval == term.interval {
				found = p
			}
		}
		require.NotNil(t, found, "catalog price %+v", term)
		require.Equal(t, "price", found.Object)
	}

	assertEmptyAccount(token)
	priceID := products[0].Prices[0].ID
	active := suite.CreateTestSubscription(userID, priceID, models.StatusActive)
	cancelled := suite.CreateTestSubscriptionWithOptions(SubscriptionOptions{UserID: userID, PriceID: products[1].Prices[0].ID, Status: models.StatusCancelled})
	payment1 := suite.CreateTestPayment(userID, priceID, &active.ID)
	payment2 := suite.CreateTestPayment(userID, priceID, &active.ID)
	suite.CreateTestEntitlement(userID, "premium", &active.ID, models.EntitlementSourceSubscription)

	current := list("/v1/me/subscriptions?status=active", token)
	require.Len(t, current, 1)
	require.Equal(t, openrails.SubscriptionID(active.ID).String(), current[0]["id"])
	require.Equal(t, string(models.StatusActive), current[0]["status"])
	price, ok := current[0]["price"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, openrails.PriceID(priceID).String(), price["id"])
	require.Equal(t, "9990000", price["unit_amount"])
	require.NotContains(t, price, "amount")
	history := list("/v1/me/subscriptions?status=all", token)
	require.Len(t, history, 2)
	statuses := map[string]any{}
	for _, row := range history {
		statuses[row["id"].(string)] = row["status"]
	}
	require.Equal(t, map[string]any{
		openrails.SubscriptionID(active.ID).String():    string(models.StatusActive),
		openrails.SubscriptionID(cancelled.ID).String(): string(models.StatusCancelled),
	}, statuses)

	payments := list("/v1/me/payments", token)
	require.Len(t, payments, 2)
	ids := make([]any, 0, len(payments))
	for _, p := range payments {
		ids = append(ids, p["id"])
		require.Equal(t, "9990000", p["amount"])
		require.Equal(t, "USD", p["currency"])
	}
	require.ElementsMatch(t, []any{openrails.PaymentID(payment1.ID).String(), openrails.PaymentID(payment2.ID).String()}, ids)
	var status map[string]any
	read("/v1/me/status", token, &status)
	require.NotNil(t, status["subscription"])
	require.NotNil(t, status["next_renewal_at"])
	require.NotEmpty(t, status["entitlements"])

	_, otherToken, _ := setupTestSuiteWithAuth(t)
	assertEmptyAccount(otherToken)
	for _, route := range []struct{ method, path string }{
		{"GET", "/v1/me/balance"}, {"GET", "/v1/me/transactions"}, {"PUT", "/v1/me/collection-payment-method"},
		{"GET", "/v1/me/payment-methods"}, {"PUT", "/v1/me/payment-methods/123"}, {"DELETE", "/v1/me/payment-methods/123"},
		{"GET", "/v1/me/subscriptions"}, {"GET", "/v1/me/payments"}, {"GET", "/v1/me/status"},
	} {
		w := httptest.NewRecorder()
		suite.Server.Handler().ServeHTTP(w, httptest.NewRequest(route.method, route.path, nil))
		require.Equal(t, http.StatusUnauthorized, w.Code, "%s %s: %s", route.method, route.path, w.Body.String())
	}
}
