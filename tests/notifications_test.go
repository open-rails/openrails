//go:build integration

package tests

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db/models"
)

// TestNotificationsRequiresAuth tests that notification endpoints require authentication
func TestNotificationsRequiresAuth(t *testing.T) {
	suite := getSharedTestSuite(t)

	t.Run("GET notifications returns 401 without auth token", func(t *testing.T) {
		w := httptest.NewRecorder()
		req, _ := http.NewRequest("GET", "/v1/me/notifications", nil)

		suite.Server.Handler().ServeHTTP(w, req)

		assert.Equal(t, http.StatusUnauthorized, w.Code, "Should return 401 Unauthorized")
	})

	t.Run("GET unread-count returns 401 without auth token", func(t *testing.T) {
		w := httptest.NewRecorder()
		req, _ := http.NewRequest("GET", "/v1/me/notifications/unread-count", nil)

		suite.Server.Handler().ServeHTTP(w, req)

		assert.Equal(t, http.StatusUnauthorized, w.Code, "Should return 401 Unauthorized")
	})

	t.Run("POST mark read returns 401 without auth token", func(t *testing.T) {
		w := httptest.NewRecorder()
		req, _ := http.NewRequest("POST", "/v1/me/notifications/"+uuid.New().String()+"/read", nil)

		suite.Server.Handler().ServeHTTP(w, req)

		assert.Equal(t, http.StatusUnauthorized, w.Code, "Should return 401 Unauthorized")
	})
}

// TestGetNotificationsEmpty tests getting notifications for a user with no notifications
func TestGetNotificationsEmpty(t *testing.T) {
	suite, token, _ := setupTestSuiteWithAuth(t)

	t.Run("returns empty list for user with no notifications", func(t *testing.T) {
		w := httptest.NewRecorder()
		req, _ := http.NewRequest("GET", "/v1/me/notifications", nil)
		req.Header.Set("Authorization", "Bearer "+token)

		suite.Server.Handler().ServeHTTP(w, req)

		require.Equal(t, http.StatusOK, w.Code, "Should return 200 OK")

		var response map[string]interface{}
		err := json.Unmarshal(w.Body.Bytes(), &response)
		require.NoError(t, err)

		assert.Equal(t, float64(0), response["total"], "Total should be 0")

		data, ok := response["data"].([]interface{})
		require.True(t, ok, "Data should be an array")
		assert.Empty(t, data, "Data should be empty")
	})
}

// TestGetNotifications tests getting notifications for a user with notifications
func TestGetNotifications(t *testing.T) {
	suite, token, userID := setupTestSuiteWithAuth(t)

	// Create some test notifications
	notif1 := suite.CreateTestNotification(userID, models.NotificationPremiumStarted, map[string]any{
		"product_name": "Premium Monthly",
		"amount":       9.99,
	})
	notif2 := suite.CreateTestNotification(userID, models.NotificationPremiumRenewed, map[string]any{
		"product_name": "Premium Monthly",
		"next_billing": "2025-02-01",
	})

	t.Run("returns notifications for user", func(t *testing.T) {
		w := httptest.NewRecorder()
		req, _ := http.NewRequest("GET", "/v1/me/notifications", nil)
		req.Header.Set("Authorization", "Bearer "+token)

		suite.Server.Handler().ServeHTTP(w, req)

		require.Equal(t, http.StatusOK, w.Code, "Should return 200 OK")

		var response map[string]interface{}
		err := json.Unmarshal(w.Body.Bytes(), &response)
		require.NoError(t, err)

		total, _ := response["total"].(float64)
		assert.GreaterOrEqual(t, total, float64(2), "Should have at least 2 notifications")

		data, ok := response["data"].([]interface{})
		require.True(t, ok)
		require.GreaterOrEqual(t, len(data), 2, "Should have at least 2 notifications in data")

		// Verify our notifications are present
		ids := make([]string, len(data))
		for i, item := range data {
			notif := item.(map[string]interface{})
			ids[i] = notif["id"].(string)
		}
		assert.Contains(t, ids, notif1.ID.String())
		assert.Contains(t, ids, notif2.ID.String())
	})

	t.Run("filters by seen=false", func(t *testing.T) {
		w := httptest.NewRecorder()
		req, _ := http.NewRequest("GET", "/v1/me/notifications?seen=false", nil)
		req.Header.Set("Authorization", "Bearer "+token)

		suite.Server.Handler().ServeHTTP(w, req)

		require.Equal(t, http.StatusOK, w.Code, "Should return 200 OK")

		var response map[string]interface{}
		err := json.Unmarshal(w.Body.Bytes(), &response)
		require.NoError(t, err)

		// All our created notifications are unread (seen=false)
		total, _ := response["total"].(float64)
		assert.GreaterOrEqual(t, total, float64(2), "Should have at least 2 unread notifications")
	})

	t.Run("supports pagination", func(t *testing.T) {
		w := httptest.NewRecorder()
		req, _ := http.NewRequest("GET", "/v1/me/notifications?offset=0&limit=1", nil)
		req.Header.Set("Authorization", "Bearer "+token)

		suite.Server.Handler().ServeHTTP(w, req)

		require.Equal(t, http.StatusOK, w.Code, "Should return 200 OK")

		var response map[string]interface{}
		err := json.Unmarshal(w.Body.Bytes(), &response)
		require.NoError(t, err)

		// Verify pagination fields
		assert.Contains(t, response, "offset", "Response should contain offset")
		assert.Contains(t, response, "limit", "Response should contain limit")
		assert.Equal(t, float64(0), response["offset"], "Offset should be 0")
		assert.Equal(t, float64(1), response["limit"], "Limit should be 1")
	})
}

// TestGetUnreadNotificationCount tests getting the unread notification count
func TestGetUnreadNotificationCount(t *testing.T) {
	suite, token, userID := setupTestSuiteWithAuth(t)

	t.Run("returns 0 for user with no notifications", func(t *testing.T) {
		w := httptest.NewRecorder()
		req, _ := http.NewRequest("GET", "/v1/me/notifications/unread-count", nil)
		req.Header.Set("Authorization", "Bearer "+token)

		suite.Server.Handler().ServeHTTP(w, req)

		require.Equal(t, http.StatusOK, w.Code, "Should return 200 OK")

		var response map[string]interface{}
		err := json.Unmarshal(w.Body.Bytes(), &response)
		require.NoError(t, err)

		assert.Equal(t, float64(0), response["unread_count"], "Unread count should be 0")
	})

	// Create some unread notifications
	suite.CreateTestNotification(userID, models.NotificationPremiumStarted, map[string]any{"test": "data1"})
	suite.CreateTestNotification(userID, models.NotificationPremiumRenewed, map[string]any{"test": "data2"})

	t.Run("returns correct unread count", func(t *testing.T) {
		w := httptest.NewRecorder()
		req, _ := http.NewRequest("GET", "/v1/me/notifications/unread-count", nil)
		req.Header.Set("Authorization", "Bearer "+token)

		suite.Server.Handler().ServeHTTP(w, req)

		require.Equal(t, http.StatusOK, w.Code, "Should return 200 OK")

		var response map[string]interface{}
		err := json.Unmarshal(w.Body.Bytes(), &response)
		require.NoError(t, err)

		assert.GreaterOrEqual(t, response["unread_count"].(float64), float64(2), "Unread count should be at least 2")
	})
}

// TestMarkNotificationRead tests marking a notification as read
func TestMarkNotificationRead(t *testing.T) {
	suite, token, userID := setupTestSuiteWithAuth(t)

	t.Run("marks notification as read successfully", func(t *testing.T) {
		// Create an unread notification
		notif := suite.CreateTestNotification(userID, models.NotificationPremiumStarted, map[string]any{
			"product_name": "Test Product",
		})

		// Verify it's initially unread
		initialCount := suite.CountUnreadNotifications(userID)
		assert.Greater(t, initialCount, 0, "Should have unread notifications")

		// Mark as read
		w := httptest.NewRecorder()
		req, _ := http.NewRequest("POST", fmt.Sprintf("/v1/me/notifications/%s/read", notif.ID.String()), nil)
		req.Header.Set("Authorization", "Bearer "+token)

		suite.Server.Handler().ServeHTTP(w, req)

		require.Equal(t, http.StatusOK, w.Code, "Should return 200 OK")

		var response map[string]interface{}
		err := json.Unmarshal(w.Body.Bytes(), &response)
		require.NoError(t, err)

		assert.Contains(t, response["message"], "read", "Response should confirm notification marked as read")
	})

	t.Run("returns error for invalid notification ID", func(t *testing.T) {
		w := httptest.NewRecorder()
		req, _ := http.NewRequest("POST", "/v1/me/notifications/not-a-uuid/read", nil)
		req.Header.Set("Authorization", "Bearer "+token)

		suite.Server.Handler().ServeHTTP(w, req)

		assert.Equal(t, http.StatusBadRequest, w.Code, "Should return 400 Bad Request")
	})

	t.Run("returns error for non-existent notification", func(t *testing.T) {
		w := httptest.NewRecorder()
		req, _ := http.NewRequest("POST", fmt.Sprintf("/v1/me/notifications/%s/read", uuid.New().String()), nil)
		req.Header.Set("Authorization", "Bearer "+token)

		suite.Server.Handler().ServeHTTP(w, req)

		// Should return error (500 based on handler implementation)
		assert.Equal(t, http.StatusInternalServerError, w.Code, "Should return 500 for non-existent notification")
	})
}

// TestNotificationsIsolation tests that users can only see their own notifications
func TestNotificationsIsolation(t *testing.T) {
	suite, token, userID := setupTestSuiteWithAuth(t)

	// Create notification for the test user
	suite.CreateTestNotification(userID, models.NotificationPremiumStarted, map[string]any{
		"product_name": "User's Product",
	})

	// Create notification for a different user
	otherUserID := uuid.New().String()
	otherNotif := suite.CreateTestNotification(otherUserID, models.NotificationPremiumStarted, map[string]any{
		"product_name": "Other User's Product",
	})

	t.Run("user cannot see other user's notifications", func(t *testing.T) {
		w := httptest.NewRecorder()
		req, _ := http.NewRequest("GET", "/v1/me/notifications", nil)
		req.Header.Set("Authorization", "Bearer "+token)

		suite.Server.Handler().ServeHTTP(w, req)

		require.Equal(t, http.StatusOK, w.Code, "Should return 200 OK")

		var response map[string]interface{}
		err := json.Unmarshal(w.Body.Bytes(), &response)
		require.NoError(t, err)

		data, ok := response["data"].([]interface{})
		require.True(t, ok)

		// Verify other user's notification is not present
		for _, item := range data {
			notif := item.(map[string]interface{})
			assert.NotEqual(t, otherNotif.ID.String(), notif["id"], "Should not see other user's notification")
		}
	})

	t.Run("user cannot mark other user's notification as read", func(t *testing.T) {
		w := httptest.NewRecorder()
		req, _ := http.NewRequest("POST", fmt.Sprintf("/v1/me/notifications/%s/read", otherNotif.ID.String()), nil)
		req.Header.Set("Authorization", "Bearer "+token)

		suite.Server.Handler().ServeHTTP(w, req)

		// Should fail - either 404 (not found for this user) or 500 (internal error)
		// Based on implementation, this will return 500 with "notification not found"
		assert.Equal(t, http.StatusInternalServerError, w.Code, "Should not allow marking other user's notification as read")
	})
}
