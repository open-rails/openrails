//go:build integration

package tests

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/controlplane"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
)

// #528 hard cut: the admin surface is the delegated /v1/admin model — a host-app
// issuer registered as the merchant's `owner` mints delegated tokens carrying
// browser-safe org permissions. The retired per-user admin-JWT model
// is gone, so these tests authenticate
// as a delegated merchant principal via newHostSeamAdminRouter.

// TestAdminGetUserBillingProfile exercises GET /v1/admin/users/:id under the
// billing:read capability: an empty profile for a new user, and the embedded
// entitlements section for a user who holds one.
func TestAdminGetUserBillingProfile(t *testing.T) {
	suite := getSharedTestSuite(t)
	admin := newHostSeamAdminRouter(t, suite, "b3333333-3333-4333-8333-333333333333",
		[]string{controlplane.PermMerchantCustomersRead})

	t.Run("returns empty profile for new user", func(t *testing.T) {
		userID := uuid.New().String()

		w := httptest.NewRecorder()
		req, _ := http.NewRequest("GET", fmt.Sprintf("/v1/admin/users/%s", userID), nil)
		req.Header.Set("Authorization", "Bearer host-credential")
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
		req, _ := http.NewRequest("GET", fmt.Sprintf("/v1/admin/users/%s", userID), nil)
		req.Header.Set("Authorization", "Bearer host-credential")
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

// TestAdminListSubscriptions exercises GET /v1/admin/subscriptions under
// billing:read: a seeded subscription appears in the merchant-scoped list.
func TestAdminListSubscriptions(t *testing.T) {
	suite := getSharedTestSuite(t)
	admin := newHostSeamAdminRouter(t, suite, "b6666666-6666-4666-8666-666666666666",
		[]string{controlplane.PermMerchantCustomersRead})

	products := suite.SeedProducts()
	priceID := products[0].Prices[0].ID
	suite.CreateTestSubscription(uuid.New().String(), priceID, models.StatusActive)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/v1/admin/subscriptions?limit=10", nil)
	req.Header.Set("Authorization", "Bearer host-credential")
	admin.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	var response map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &response))
	data, ok := response["data"].([]interface{})
	require.True(t, ok, "paginated response should carry a data array: %s", w.Body.String())
	assert.GreaterOrEqual(t, len(data), 1, "the seeded subscription should appear")
}

// TestRemovedAdminSubscriptionExtendRoute is a regression guard: the old
// PUT /subscriptions/:id/extend route stays removed. Cancellation is the only
// subscription mutation on the delegated surface (DELETE /subscriptions/:id).
func TestRemovedAdminSubscriptionExtendRoute(t *testing.T) {
	suite := getSharedTestSuite(t)
	admin := newHostSeamAdminRouter(t, suite, "b7777777-7777-4777-8777-777777777777",
		[]string{controlplane.PermMerchantSubscriptionsUpdate})

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("PUT", "/v1/admin/subscriptions/"+uuid.New().String()+"/extend", nil)
	req.Header.Set("Authorization", "Bearer host-credential")
	admin.ServeHTTP(w, req)

	assert.Equal(t, http.StatusNotFound, w.Code, "extend route must stay removed")
}

// TestAdminHealth tests the public health endpoint (no auth required). It runs
// against the production server handler, not the admin surface.
func TestAdminHealth(t *testing.T) {
	suite := getSharedTestSuite(t)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/health/live", nil)
	suite.Server.Handler().ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code, "Should return 200 OK")

	var response map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &response))
	assert.Equal(t, "ok", response["status"], "Status should be ok")
	assert.Equal(t, "billing", response["service"], "Service should be billing")
}
