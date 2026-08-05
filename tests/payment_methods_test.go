//go:build integration

package tests

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/pkg/api"
)

// TestPaymentMethodsRequiresAuth tests that payment methods endpoints require authentication
func TestPaymentMethodsRequiresAuth(t *testing.T) {
	suite := getSharedTestSuite(t)

	t.Run("LIST returns 401 without auth token", func(t *testing.T) {
		w := httptest.NewRecorder()
		req, _ := http.NewRequest("GET", "/v1/me/payment-methods", nil)

		suite.Server.Handler().ServeHTTP(w, req)

		assert.Equal(t, http.StatusUnauthorized, w.Code, "Should return 401 Unauthorized")
	})

	t.Run("CREATE returns 401 without auth token", func(t *testing.T) {
		body := map[string]string{
			"payment_token": "test-token",
			"first_name":    "Test",
			"last_name":     "User",
			"address1":      "123 Test St",
			"city":          "Test City",
			"state":         "CA",
			"zip":           "90210",
			"country":       "US",
		}
		jsonBody, _ := json.Marshal(body)

		w := httptest.NewRecorder()
		req, _ := http.NewRequest("POST", "/v1/me/payment-methods", bytes.NewReader(jsonBody))
		req.Header.Set("Content-Type", "application/json")

		suite.Server.Handler().ServeHTTP(w, req)

		assert.Equal(t, http.StatusUnauthorized, w.Code, "Should return 401 Unauthorized")
	})

	t.Run("DELETE returns 401 without auth token", func(t *testing.T) {
		w := httptest.NewRecorder()
		req, _ := http.NewRequest("DELETE", "/v1/me/payment-methods/"+uuid.New().String(), nil)

		suite.Server.Handler().ServeHTTP(w, req)

		assert.Equal(t, http.StatusUnauthorized, w.Code, "Should return 401 Unauthorized")
	})

	t.Run("UPDATE returns 401 without auth token", func(t *testing.T) {
		body := map[string]string{
			"payment_token": "test-token",
		}
		jsonBody, _ := json.Marshal(body)

		w := httptest.NewRecorder()
		req, _ := http.NewRequest("PUT", "/v1/me/payment-methods/"+uuid.New().String(), bytes.NewReader(jsonBody))
		req.Header.Set("Content-Type", "application/json")

		suite.Server.Handler().ServeHTTP(w, req)

		assert.Equal(t, http.StatusUnauthorized, w.Code, "Should return 401 Unauthorized")
	})
}

// TestListPaymentMethodsEmpty tests listing payment methods for a user with no methods
func TestListPaymentMethodsEmpty(t *testing.T) {
	suite, token, _ := setupTestSuiteWithAuth(t)

	t.Run("returns empty list for user with no payment methods", func(t *testing.T) {
		w := httptest.NewRecorder()
		req, _ := http.NewRequest("GET", "/v1/me/payment-methods", nil)
		req.Header.Set("Authorization", "Bearer "+token)

		suite.Server.Handler().ServeHTTP(w, req)

		require.Equal(t, http.StatusOK, w.Code, "Should return 200 OK")

		var response map[string]interface{}
		err := json.Unmarshal(w.Body.Bytes(), &response)
		require.NoError(t, err)

		// Check pagination fields
		assert.Equal(t, float64(0), response["total"], "Total should be 0")

		// Check data is empty array
		data, ok := response["data"].([]interface{})
		require.True(t, ok, "Data should be an array")
		assert.Empty(t, data, "Data should be empty")
	})
}

// TestListPaymentMethods tests listing payment methods for a user with methods
func TestListPaymentMethods(t *testing.T) {
	suite, token, userID := setupTestSuiteWithAuth(t)

	// Create some test payment methods
	pm1 := suite.CreateTestPaymentMethodWithOptions(PaymentMethodOptions{
		UserID:   userID,
		Rail:     models.RailNMI,
		LastFour: "4242",
		CardType: "Visa",
	})

	pm2 := suite.CreateTestPaymentMethodWithOptions(PaymentMethodOptions{
		UserID:   userID,
		Rail:     models.RailNMI,
		LastFour: "1234",
		CardType: "Mastercard",
	})

	t.Run("returns payment methods for user", func(t *testing.T) {
		w := httptest.NewRecorder()
		req, _ := http.NewRequest("GET", "/v1/me/payment-methods", nil)
		req.Header.Set("Authorization", "Bearer "+token)

		suite.Server.Handler().ServeHTTP(w, req)

		require.Equal(t, http.StatusOK, w.Code, "Should return 200 OK")

		var response map[string]interface{}
		err := json.Unmarshal(w.Body.Bytes(), &response)
		require.NoError(t, err)

		// Should return all active methods for this user
		total, _ := response["total"].(float64)
		assert.GreaterOrEqual(t, int(total), 2, "Should have at least 2 payment methods")

		data, ok := response["data"].([]interface{})
		require.True(t, ok)
		require.Len(t, data, int(total), "Data length should match total")

		// Verify our created methods are present
		ids := make([]string, len(data))
		for i, item := range data {
			method := item.(map[string]interface{})
			ids[i] = method["id"].(string)
		}
		assert.Contains(t, ids, api.FormatPaymentMethodID(pm1.ID))
		assert.Contains(t, ids, api.FormatPaymentMethodID(pm2.ID))
	})

	t.Run("supports pagination parameters", func(t *testing.T) {
		w := httptest.NewRecorder()
		req, _ := http.NewRequest("GET", "/v1/me/payment-methods?offset=0&limit=20", nil)
		req.Header.Set("Authorization", "Bearer "+token)

		suite.Server.Handler().ServeHTTP(w, req)

		require.Equal(t, http.StatusOK, w.Code, "Should return 200 OK")

		var response map[string]interface{}
		err := json.Unmarshal(w.Body.Bytes(), &response)
		require.NoError(t, err)

		assert.Equal(t, float64(2), response["total"], "Total should include both seeded methods")
		assert.Equal(t, float64(20), response["limit"], "Limit should match request")
		assert.Equal(t, float64(0), response["offset"], "Offset should match request")
		assert.Equal(t, false, response["has_more"], "Should not have another page")
	})
}

// TestCreatePaymentMethod tests creating payment methods
func TestCreatePaymentMethod(t *testing.T) {
	suite, mock := SetupSuiteWithMockNMI(t)

	// Create auth token for test user
	userID := uuid.New().String()
	email := "pm-create-" + t.Name() + "@test.example.com"
	token := suite.MintUserToken(userID, email)

	t.Run("creates payment method successfully", func(t *testing.T) {
		mock.Reset()

		body := map[string]string{
			"payment_token": "test-token-create",
			"first_name":    "Test",
			"last_name":     "User",
			"address1":      "123 Test St",
			"city":          "Test City",
			"state":         "CA",
			"zip":           "90210",
			"country":       "US",
			"email":         email,
			"provider":      "nmi",
		}
		jsonBody, _ := json.Marshal(body)

		w := httptest.NewRecorder()
		req, _ := http.NewRequest("POST", "/v1/me/payment-methods", bytes.NewReader(jsonBody))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)

		suite.Server.Handler().ServeHTTP(w, req)

		require.Equal(t, http.StatusOK, w.Code, "Should return 200 OK, got body: %s", w.Body.String())

		var response map[string]interface{}
		err := json.Unmarshal(w.Body.Bytes(), &response)
		require.NoError(t, err)

		assert.NotEmpty(t, response["id"], "Should return payment method ID")
		assert.Equal(t, "nmi", response["rail"], "Rail should be nmi")
	})

	t.Run("returns error without payment_token", func(t *testing.T) {
		body := map[string]string{
			"first_name": "Test",
			"last_name":  "User",
			// No payment_token
		}
		jsonBody, _ := json.Marshal(body)

		w := httptest.NewRecorder()
		req, _ := http.NewRequest("POST", "/v1/me/payment-methods", bytes.NewReader(jsonBody))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)

		suite.Server.Handler().ServeHTTP(w, req)

		assert.Equal(t, http.StatusBadRequest, w.Code, "Should return 400 Bad Request")
	})
}

// TestDeletePaymentMethod tests deleting payment methods
func TestDeletePaymentMethod(t *testing.T) {
	suite, token, userID := setupTestSuiteWithAuth(t)

	t.Run("deletes payment method successfully", func(t *testing.T) {
		// Create a payment method to delete
		pm := suite.CreateTestPaymentMethodWithOptions(PaymentMethodOptions{
			UserID: userID,
			Rail:   models.RailNMI,
		})

		w := httptest.NewRecorder()
		req, _ := http.NewRequest("DELETE", "/v1/me/payment-methods/"+pm.ID.String(), nil)
		req.Header.Set("Authorization", "Bearer "+token)

		suite.Server.Handler().ServeHTTP(w, req)

		require.Equal(t, http.StatusOK, w.Code, "Should return 200 OK")

		var response map[string]interface{}
		err := json.Unmarshal(w.Body.Bytes(), &response)
		require.NoError(t, err)

		assert.True(t, response["success"].(bool), "Success should be true")
		assert.Contains(t, response["message"], "deleted", "Message should mention deleted")

		// Verify payment method is actually deleted
		pms := suite.GetPaymentMethodsByUserID(userID)
		for _, p := range pms {
			assert.NotEqual(t, pm.ID, p.ID, "Deleted payment method should not be in list")
		}
	})

	t.Run("returns 404 for non-existent payment method", func(t *testing.T) {
		w := httptest.NewRecorder()
		req, _ := http.NewRequest("DELETE", "/v1/me/payment-methods/"+uuid.New().String(), nil)
		req.Header.Set("Authorization", "Bearer "+token)

		suite.Server.Handler().ServeHTTP(w, req)

		assert.Equal(t, http.StatusNotFound, w.Code, "Should return 404 Not Found")
	})

	t.Run("returns 403 for payment method owned by another user", func(t *testing.T) {
		// Create a payment method owned by a different user
		otherUserID := uuid.New().String()
		pm := suite.CreateTestPaymentMethodWithOptions(PaymentMethodOptions{
			UserID: otherUserID,
			Rail:   models.RailNMI,
		})

		w := httptest.NewRecorder()
		req, _ := http.NewRequest("DELETE", "/v1/me/payment-methods/"+pm.ID.String(), nil)
		req.Header.Set("Authorization", "Bearer "+token)

		suite.Server.Handler().ServeHTTP(w, req)

		assert.Equal(t, http.StatusForbidden, w.Code, "Should return 403 Forbidden")
	})

	t.Run("returns 400 for invalid UUID", func(t *testing.T) {
		w := httptest.NewRecorder()
		req, _ := http.NewRequest("DELETE", "/v1/me/payment-methods/not-a-uuid", nil)
		req.Header.Set("Authorization", "Bearer "+token)

		suite.Server.Handler().ServeHTTP(w, req)

		assert.Equal(t, http.StatusBadRequest, w.Code, "Should return 400 Bad Request")
	})
}

// TestUpdatePaymentMethod tests updating payment methods
func TestUpdatePaymentMethod(t *testing.T) {
	suite, mock := SetupSuiteWithMockNMI(t)

	// Create auth token for test user
	userID := uuid.New().String()
	email := "pm-update-" + t.Name() + "@test.example.com"
	token := suite.MintUserToken(userID, email)

	t.Run("updates payment method successfully", func(t *testing.T) {
		mock.Reset()

		// Create a payment method to update
		pm := suite.CreateTestPaymentMethodWithOptions(PaymentMethodOptions{
			UserID: userID,
			Rail:   models.RailNMI,
		})

		body := map[string]string{
			"payment_token": "new-token",
			"first_name":    "Updated",
			"last_name":     "User",
		}
		jsonBody, _ := json.Marshal(body)

		w := httptest.NewRecorder()
		req, _ := http.NewRequest("PUT", fmt.Sprintf("/v1/me/payment-methods/%s", pm.ID.String()), bytes.NewReader(jsonBody))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)

		suite.Server.Handler().ServeHTTP(w, req)

		require.Equal(t, http.StatusOK, w.Code, "Should return 200 OK, got body: %s", w.Body.String())

		var response map[string]interface{}
		err := json.Unmarshal(w.Body.Bytes(), &response)
		require.NoError(t, err)

		assert.Equal(t, api.FormatPaymentMethodID(pm.ID), response["id"], "Should return same payment method ID")
	})

	t.Run("returns error without payment_token", func(t *testing.T) {
		pm := suite.CreateTestPaymentMethodWithOptions(PaymentMethodOptions{
			UserID: userID,
			Rail:   models.RailNMI,
		})

		body := map[string]string{
			"first_name": "Updated",
			// No payment_token
		}
		jsonBody, _ := json.Marshal(body)

		w := httptest.NewRecorder()
		req, _ := http.NewRequest("PUT", fmt.Sprintf("/v1/me/payment-methods/%s", pm.ID.String()), bytes.NewReader(jsonBody))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)

		suite.Server.Handler().ServeHTTP(w, req)

		assert.Equal(t, http.StatusBadRequest, w.Code, "Should return 400 Bad Request")
	})

	t.Run("returns 404 for non-existent payment method", func(t *testing.T) {
		body := map[string]string{
			"payment_token": "new-token",
		}
		jsonBody, _ := json.Marshal(body)

		w := httptest.NewRecorder()
		req, _ := http.NewRequest("PUT", fmt.Sprintf("/v1/me/payment-methods/%s", uuid.New().String()), bytes.NewReader(jsonBody))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)

		suite.Server.Handler().ServeHTTP(w, req)

		assert.Equal(t, http.StatusNotFound, w.Code, "Should return 404 Not Found")
	})

	t.Run("returns 403 for payment method owned by another user", func(t *testing.T) {
		// Create a payment method owned by a different user
		otherUserID := uuid.New().String()
		pm := suite.CreateTestPaymentMethodWithOptions(PaymentMethodOptions{
			UserID: otherUserID,
			Rail:   models.RailNMI,
		})

		body := map[string]string{
			"payment_token": "new-token",
		}
		jsonBody, _ := json.Marshal(body)

		w := httptest.NewRecorder()
		req, _ := http.NewRequest("PUT", fmt.Sprintf("/v1/me/payment-methods/%s", pm.ID.String()), bytes.NewReader(jsonBody))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)

		suite.Server.Handler().ServeHTTP(w, req)

		assert.Equal(t, http.StatusForbidden, w.Code, "Should return 403 Forbidden")
	})
}

// or#896: a payment-method create on a rail OpenRails does not vault for used
// to answer "PSP 'stripe' is not configured" — a credential complaint about a
// surface that does not exist. It now names the rail and where the instrument
// actually lives.
func TestCreatePaymentMethodUnsupportedRailIsHonest(t *testing.T) {
	suite, _ := SetupSuiteWithMockNMI(t)
	userID := uuid.New().String()
	token := suite.MintUserToken(userID, "pm-unsupported-"+uuid.NewString()+"@test.example.com")

	cases := map[string]string{
		"stripe": "Billing Portal",
		"ccbill": "CCBill owns the vault",
		"solana": "wallet",
	}
	for psp, expect := range cases {
		t.Run(psp, func(t *testing.T) {
			jsonBody, _ := json.Marshal(map[string]string{
				"payment_token": "test-token-" + psp,
				"first_name":    "Test",
				"last_name":     "User",
				"provider":      psp,
			})
			w := httptest.NewRecorder()
			req, _ := http.NewRequest("POST", "/v1/me/payment-methods", bytes.NewReader(jsonBody))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer "+token)
			suite.Server.Handler().ServeHTTP(w, req)

			require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
			body := w.Body.String()
			assert.Contains(t, body, "not managed by OpenRails on this rail")
			assert.Contains(t, body, expect)
			assert.NotContains(t, body, "is not configured",
				"an unsupported surface must never read as a misconfiguration")
		})
	}
}
