//go:build integration

package tests

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/controlplane"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
)

// #528 hard cut: the admin surface is the delegated /v1/merchant model — a host-app
// issuer registered as the merchant's `owner` mints delegated tokens carrying
// browser-safe merchant permissions. The retired per-user admin-JWT model
// is gone, so these tests authenticate
// as a delegated merchant principal via newHostSeamAdminRouter.

// TestAdminGetUserBillingProfile exercises GET /v1/merchant/customers/:id under the
// billing:read capability: an empty profile for a new user, and the embedded
// entitlements section for a user who holds one.
func TestAdminGetUserBillingProfile(t *testing.T) {
	suite := getSharedTestSuite(t)
	admin := newHostSeamAdminRouter(t, suite, "b3333333-3333-4333-8333-333333333333",
		[]string{controlplane.PermMerchantCustomerSettingsRead})

	t.Run("returns empty profile for new user", func(t *testing.T) {
		userID := uuid.New().String()

		w := httptest.NewRecorder()
		req, _ := http.NewRequest("GET", fmt.Sprintf("/v1/merchant/customers/%s", userID), nil)
		req.Header.Set("Authorization", "Bearer "+merchantDelegatedTestToken)
		admin.ServeHTTP(w, req)

		require.Equal(t, http.StatusOK, w.Code, w.Body.String())

		var response map[string]interface{}
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &response))
		// The profile is keyed by the payable customer (for the
		// personal/self-hosted case this is the user's own UUID).
		assert.Equal(t, userID, response["customer_id"], "customer_id should match")
	})

	t.Run("returns entitlements in user profile", func(t *testing.T) {
		userID := uuid.New().String()
		// The RLS-aware Runtime.DB insert needs the merchant pinned on the context.
		ctx := dbtest.WithTestMerchant(t.Context())

		now := time.Now().UTC()
		endAt := now.Add(30 * 24 * time.Hour)
		// #511: admin entitlement carries an admin source id (no entitlement_grants row).
		adminSourceID := uuid.New()
		suite.InsertEntitlement(ctx, &models.Entitlement{
			ID:          uuid.New(),
			CustomerID:  suite.ensureCustomer(ctx, userID),
			Entitlement: "premium",
			StartAt:     now.Add(-time.Second),
			EndAt:       &endAt,
			SourceID:    &adminSourceID,
			SourceType:  models.EntitlementSourceAdmin,
			CreatedAt:   now,
			UpdatedAt:   now,
		})

		w := httptest.NewRecorder()
		req, _ := http.NewRequest("GET", fmt.Sprintf("/v1/merchant/customers/%s", userID), nil)
		req.Header.Set("Authorization", "Bearer "+merchantDelegatedTestToken)
		admin.ServeHTTP(w, req)

		require.Equal(t, http.StatusOK, w.Code, w.Body.String())

		var response map[string]interface{}
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &response))
		assert.Equal(t, userID, response["customer_id"], "customer_id should match")
		entitlements, ok := response["entitlements"].([]interface{})
		require.True(t, ok, "response should embed entitlements array: %s", w.Body.String())
		assert.Len(t, entitlements, 1, "should have one entitlement")
	})
}

// TestAdminListSubscriptions exercises GET /v1/merchant/subscriptions under
// merchant:subscriptions:read: a seeded subscription appears in the merchant-scoped list.
func TestAdminListSubscriptions(t *testing.T) {
	suite := getSharedTestSuite(t)
	admin := newHostSeamAdminRouter(t, suite, "b6666666-6666-4666-8666-666666666666",
		[]string{controlplane.PermMerchantSubscriptionsRead})

	products := suite.SeedProducts()
	priceID := products[0].Prices[0].ID
	suite.CreateTestSubscription(uuid.New().String(), priceID, models.StatusActive)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/v1/merchant/subscriptions?limit=10", nil)
	req.Header.Set("Authorization", "Bearer "+merchantDelegatedTestToken)
	admin.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	var response map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &response))
	data, ok := response["data"].([]interface{})
	require.True(t, ok, "paginated response should carry a data array: %s", w.Body.String())
	assert.GreaterOrEqual(t, len(data), 1, "the seeded subscription should appear")
}

// TestAdminCancelSubscriptionNotFound pins the #783 fix: cancelling a
// nonexistent subscription returns 404, not a 500 leaking the raw
// "subscription not found: no rows in result set" (sql.ErrNoRows) text.
func TestAdminCancelSubscriptionNotFound(t *testing.T) {
	suite := getSharedTestSuite(t)
	admin := newHostSeamAdminRouter(t, suite, "b7777777-7777-4777-8777-777777777777",
		[]string{controlplane.PermMerchantSubscriptionsUpdate})

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST",
		fmt.Sprintf("/v1/merchant/subscriptions/%s/cancel", uuid.New().String()),
		strings.NewReader(`{"reason":"regression check"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+merchantDelegatedTestToken)
	admin.ServeHTTP(w, req)

	assert.Equal(t, http.StatusNotFound, w.Code, "missing subscription must 404, not 500: %s", w.Body.String())
	assert.NotContains(t, w.Body.String(), "no rows", "must not leak raw sql.ErrNoRows text")
	assert.NotContains(t, w.Body.String(), "SQLSTATE", "must not leak raw postgres error text")
}
