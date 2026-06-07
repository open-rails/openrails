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
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/pkg/identity"
)

// setupAdminTestSuite sets up the test suite for admin tests.
// Admin endpoints require JWT with "admin" role (via AuthRequired + AdminRequired middleware).
func setupAdminTestSuite(t *testing.T) (*TestContainerSuite, string) {
	suite, token, _ := setupTestSuiteWithAdminAuth(t)
	return suite, token
}

// TestAdminEndpointsRequireAuth tests that admin endpoints require JWT with admin role
func TestAdminEndpointsRequireAuth(t *testing.T) {
	suite, _ := setupAdminTestSuite(t)

	t.Run("GET subscriptions returns 401 without auth", func(t *testing.T) {
		w := httptest.NewRecorder()
		req, _ := http.NewRequest("GET", "/v1/admin/subscriptions", nil)

		suite.Server.Handler().ServeHTTP(w, req)

		assert.Equal(t, http.StatusUnauthorized, w.Code, "Should return 401 Unauthorized")
	})

	t.Run("GET metrics summary returns 401 without auth", func(t *testing.T) {
		w := httptest.NewRecorder()
		req, _ := http.NewRequest("GET", "/v1/admin/metrics/summary", nil)

		suite.Server.Handler().ServeHTTP(w, req)

		assert.Equal(t, http.StatusUnauthorized, w.Code, "Should return 401 Unauthorized")
	})

	t.Run("GET user billing profile returns 401 without auth", func(t *testing.T) {
		w := httptest.NewRecorder()
		req, _ := http.NewRequest("GET", "/v1/admin/users/"+uuid.New().String(), nil)

		suite.Server.Handler().ServeHTTP(w, req)

		assert.Equal(t, http.StatusUnauthorized, w.Code, "Should return 401 Unauthorized")
	})

	t.Run("returns 403 with non-admin JWT", func(t *testing.T) {
		// Create a regular user token (no admin role)
		userID := uuid.New().String()
		userToken := CreateUserToken(t, userID)

		w := httptest.NewRecorder()
		req, _ := http.NewRequest("GET", "/v1/admin/subscriptions", nil)
		req.Header.Set("Authorization", "Bearer "+userToken)

		suite.Server.Handler().ServeHTTP(w, req)

		assert.Equal(t, http.StatusForbidden, w.Code, "Should return 403 Forbidden for non-admin user")
	})
}

func TestRemovedAdminSubscriptionExtendRoute(t *testing.T) {
	suite, adminToken := setupAdminTestSuite(t)

	t.Run("returns 404 without auth", func(t *testing.T) {
		w := httptest.NewRecorder()
		req, _ := http.NewRequest("PUT", "/v1/admin/subscriptions/"+uuid.New().String()+"/extend", nil)

		suite.Server.Handler().ServeHTTP(w, req)

		assert.Equal(t, http.StatusNotFound, w.Code)
	})

	t.Run("returns 404 with admin auth", func(t *testing.T) {
		w := httptest.NewRecorder()
		req, _ := http.NewRequest("PUT", "/v1/admin/subscriptions/"+uuid.New().String()+"/extend", nil)
		req.Header.Set("Authorization", "Bearer "+adminToken)

		suite.Server.Handler().ServeHTTP(w, req)

		assert.Equal(t, http.StatusNotFound, w.Code)
	})
}

// TestAdminGetUserBillingProfile tests the GET user billing profile endpoint
func TestAdminGetUserBillingProfile(t *testing.T) {
	suite, adminToken := setupAdminTestSuite(t)

	t.Run("returns empty profile for new user", func(t *testing.T) {
		userID := uuid.New().String()

		w := httptest.NewRecorder()
		req, _ := http.NewRequest("GET", fmt.Sprintf("/v1/admin/users/%s", userID), nil)
		req.Header.Set("Authorization", "Bearer "+adminToken)

		suite.Server.Handler().ServeHTTP(w, req)

		require.Equal(t, http.StatusOK, w.Code, "Should return 200 OK")

		var response map[string]interface{}
		err := json.Unmarshal(w.Body.Bytes(), &response)
		require.NoError(t, err)

		assert.Equal(t, userID, response["user_id"], "User ID should match")
	})

	t.Run("returns entitlements in user profile", func(t *testing.T) {
		userID := uuid.New().String()

		now := time.Now().UTC()
		endAt := now.Add(30 * 24 * time.Hour)
		adminGrant := &models.AdminGrant{
			ID:              uuid.New(),
			TenantSubjectID: identity.TenantSubjectIDFromString(userID).UUID(),
			GrantedBy:       "test-admin",
			Reason:          "test_admin_entitlement",
			DurationDays:    nil,
			CreatedAt:       now,
		}
		_, err := suite.BunDB.NewInsert().Model(adminGrant).Exec(context.Background())
		require.NoError(t, err)
		_, err = suite.BunDB.NewInsert().Model(&models.Entitlement{
			ID:              uuid.New(),
			TenantSubjectID: identity.TenantSubjectIDFromString(userID).UUID(),
			Entitlement:     "premium",
			StartAt:         now.Add(-time.Second),
			EndAt:           &endAt,
			SourceID:        &adminGrant.ID,
			SourceType:      models.EntitlementSourceAdmin,
			CreatedAt:       now,
			UpdatedAt:       now,
		}).Exec(context.Background())
		require.NoError(t, err)

		w := httptest.NewRecorder()
		req, _ := http.NewRequest("GET", fmt.Sprintf("/v1/admin/users/%s", userID), nil)
		req.Header.Set("Authorization", "Bearer "+adminToken)

		suite.Server.Handler().ServeHTTP(w, req)

		require.Equal(t, http.StatusOK, w.Code, "Should return 200 OK")

		var response map[string]interface{}
		err = json.Unmarshal(w.Body.Bytes(), &response)
		require.NoError(t, err)

		assert.Equal(t, userID, response["user_id"], "User ID should match")
		// The profile includes entitlements as part of the response
		entitlements, ok := response["entitlements"].([]interface{})
		require.True(t, ok, "Response should have entitlements array")
		assert.Len(t, entitlements, 1, "Should have one entitlement")
	})
}

// TestAdminHealth tests the health endpoint (public, no auth required)
func TestAdminHealth(t *testing.T) {
	suite, _ := setupAdminTestSuite(t)

	t.Run("health/live endpoint returns ok without auth", func(t *testing.T) {
		w := httptest.NewRecorder()
		req, _ := http.NewRequest("GET", "/health/live", nil)
		// No auth header - health endpoint should be public

		suite.Server.Handler().ServeHTTP(w, req)

		assert.Equal(t, http.StatusOK, w.Code, "Should return 200 OK")

		var response map[string]interface{}
		err := json.Unmarshal(w.Body.Bytes(), &response)
		require.NoError(t, err)

		assert.Equal(t, "ok", response["status"], "Status should be ok")
		assert.Equal(t, "billing", response["service"], "Service should be billing")
	})
}
