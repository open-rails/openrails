//go:build integration

package tests

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/modules/paymentmethods"
	"github.com/open-rails/openrails/pkg/api"
)

type fakePaymentMethodDeleteExecutor struct {
	outcome paymentmethods.PaymentMethodDeleteOutcome
	err     error
}

func (f fakePaymentMethodDeleteExecutor) ExecutePaymentMethodDelete(context.Context, *models.PaymentMethod) (paymentmethods.PaymentMethodDeleteOutcome, error) {
	return f.outcome, f.err
}

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
	suite, _ := SetupSuiteWithMockNMI(t)
	userID := uuid.NewString()
	token := suite.MintUserToken(userID, "payment-method-delete-"+uuid.NewString()+"@test.example")
	originalDeleteIntents := suite.App.Runtime.RailPaymentMethodService.DeleteIntents
	dbtest.ArmDestructiveActions(context.Background(), t, dbtest.TestMerchantID.UUID())
	t.Cleanup(func() {
		suite.App.Runtime.RailPaymentMethodService.DeleteIntents = originalDeleteIntents
		dbtest.DisarmDestructiveActions(context.Background(), t, suite.MerchantPool())
	})
	paymentMethodExists := func(id uuid.UUID) bool {
		for _, pm := range suite.GetPaymentMethodsByUserID(userID) {
			if pm.ID == id {
				return true
			}
		}
		return false
	}

	t.Run("deletes payment method successfully", func(t *testing.T) {
		// Create a payment method to delete
		pm := suite.CreateTestPaymentMethodWithOptions(PaymentMethodOptions{
			UserID: userID,
			Rail:   models.RailNMI,
		})
		var methodPSPID uuid.UUID
		require.NoError(t, suite.MerchantPool().QueryRow(context.Background(),
			`SELECT psp_id FROM openrails.payment_methods WHERE id = $1`, pm.ID,
		).Scan(&methodPSPID))

		w := httptest.NewRecorder()
		req, _ := http.NewRequest("DELETE", "/v1/me/payment-methods/"+pm.ID.String(), nil)
		req.Header.Set("Authorization", "Bearer "+token)

		suite.Server.Handler().ServeHTTP(w, req)

		require.Equal(t, http.StatusNoContent, w.Code, "completed deletion returns 204 No Content")
		assert.Empty(t, w.Body.String())

		// Verify payment method is actually deleted
		pms := suite.GetPaymentMethodsByUserID(userID)
		for _, p := range pms {
			assert.NotEqual(t, pm.ID, p.ID, "Deleted payment method should not be in list")
		}

		var intentStatus string
		var intentPSPID uuid.UUID
		err := suite.MerchantPool().QueryRow(context.Background(), `
			SELECT status, psp_id
			FROM openrails.rail_intents
			WHERE intent_type = $1 AND idempotency_key = $2`,
			intents.TypeNMIPaymentMethodDelete,
			intents.NMIPaymentMethodDeleteIdempotencyKey(pm.ID),
		).Scan(&intentStatus, &intentPSPID)
		require.NoError(t, err)
		assert.Equal(t, string(intents.StatusSucceeded), intentStatus)
		assert.Equal(t, methodPSPID, intentPSPID, "delete intent must preserve the method's exact PSP binding")
	})

	t.Run("returns 202 while durable deletion is processing", func(t *testing.T) {
		pm := suite.CreateTestPaymentMethodWithOptions(PaymentMethodOptions{UserID: userID, Rail: models.RailNMI})
		suite.App.Runtime.RailPaymentMethodService.DeleteIntents = fakePaymentMethodDeleteExecutor{}
		defer func() { suite.App.Runtime.RailPaymentMethodService.DeleteIntents = originalDeleteIntents }()

		w := httptest.NewRecorder()
		req, _ := http.NewRequest("DELETE", "/v1/me/payment-methods/"+pm.ID.String(), nil)
		req.Header.Set("Authorization", "Bearer "+token)
		suite.Server.Handler().ServeHTTP(w, req)

		require.Equal(t, http.StatusAccepted, w.Code, w.Body.String())
		assert.Empty(t, w.Body.String())
		assert.True(t, paymentMethodExists(pm.ID), "processing must keep the local mirror visible")
	})

	t.Run("returns 502 for a terminal provider failure", func(t *testing.T) {
		pm := suite.CreateTestPaymentMethodWithOptions(PaymentMethodOptions{UserID: userID, Rail: models.RailNMI})
		suite.App.Runtime.RailPaymentMethodService.DeleteIntents = fakePaymentMethodDeleteExecutor{
			outcome: paymentmethods.PaymentMethodDeleteOutcome{Terminal: true, Reason: "provider detail must stay private"},
		}
		defer func() { suite.App.Runtime.RailPaymentMethodService.DeleteIntents = originalDeleteIntents }()

		w := httptest.NewRecorder()
		req, _ := http.NewRequest("DELETE", "/v1/me/payment-methods/"+pm.ID.String(), nil)
		req.Header.Set("Authorization", "Bearer "+token)
		suite.Server.Handler().ServeHTTP(w, req)

		require.Equal(t, http.StatusBadGateway, w.Code, w.Body.String())
		assert.Contains(t, w.Body.String(), "payment_method_delete_failed")
		assert.NotContains(t, w.Body.String(), "provider detail must stay private")
		assert.True(t, paymentMethodExists(pm.ID))
	})

	t.Run("returns 429 when the destructive ceiling refuses deletion", func(t *testing.T) {
		pm := suite.CreateTestPaymentMethodWithOptions(PaymentMethodOptions{UserID: userID, Rail: models.RailNMI})
		suite.App.Runtime.RailPaymentMethodService.DeleteIntents = fakePaymentMethodDeleteExecutor{err: intents.ErrRateCeilingTripped}
		defer func() { suite.App.Runtime.RailPaymentMethodService.DeleteIntents = originalDeleteIntents }()

		w := httptest.NewRecorder()
		req, _ := http.NewRequest("DELETE", "/v1/me/payment-methods/"+pm.ID.String(), nil)
		req.Header.Set("Authorization", "Bearer "+token)
		suite.Server.Handler().ServeHTTP(w, req)

		require.Equal(t, http.StatusTooManyRequests, w.Code, w.Body.String())
		assert.Contains(t, w.Body.String(), api.CodeRateLimitExceeded)
		assert.True(t, paymentMethodExists(pm.ID))
	})

	t.Run("rejects deletion for portal-managed Stripe methods", func(t *testing.T) {
		pm := suite.CreateTestPaymentMethodWithOptions(PaymentMethodOptions{UserID: userID, Rail: models.RailStripe})

		w := httptest.NewRecorder()
		req, _ := http.NewRequest("DELETE", "/v1/me/payment-methods/"+pm.ID.String(), nil)
		req.Header.Set("Authorization", "Bearer "+token)
		suite.Server.Handler().ServeHTTP(w, req)

		require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
		assert.Contains(t, w.Body.String(), "payment_method_delete_unsupported")
		assert.Contains(t, w.Body.String(), "Billing Portal")
		assert.True(t, paymentMethodExists(pm.ID))
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
			UserID:     userID,
			Rail:       models.RailNMI,
			BillingID:  "B1",
			LastFour:   "1111",
			CardType:   "Visa",
			ExpiryDate: "01/29",
		})
		mock.SetCardReplacement(pm.RailCustomerRef,
			mockNMICard{LastFour: "1111", CardType: "Visa", Expiry: "0129"},
			mockNMICard{LastFour: "4242", CardType: "Mastercard", Expiry: "1230"},
		)

		body := map[string]string{
			"payment_token": "new-token",
			"first_name":    "Updated",
			"last_name":     "User",
			"last_four":     "4242",
			"card_type":     "Mastercard",
			"expiry_date":   "12/30",
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
		card, ok := response["card"].(map[string]interface{})
		require.True(t, ok)
		assert.Equal(t, "4242", card["last4"])
		assert.Equal(t, "Mastercard", card["brand"])
		assert.Equal(t, float64(12), card["exp_month"])
		assert.Equal(t, float64(2030), card["exp_year"])
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

	t.Run("requires tokenization metadata", func(t *testing.T) {
		mock.Reset()
		pm := suite.CreateTestPaymentMethodWithOptions(PaymentMethodOptions{
			UserID: userID,
			Rail:   models.RailNMI,
		})
		jsonBody, _ := json.Marshal(map[string]string{"payment_token": "new-token"})

		w := httptest.NewRecorder()
		req, _ := http.NewRequest("PUT", fmt.Sprintf("/v1/me/payment-methods/%s", pm.ID.String()), bytes.NewReader(jsonBody))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)

		suite.Server.Handler().ServeHTTP(w, req)

		require.Equal(t, http.StatusBadRequest, w.Code)
		assert.Contains(t, w.Body.String(), "last_four, card_type, and expiry_date")
		assert.Zero(t, mock.RequestCount, "invalid requests must not reach NMI")
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
