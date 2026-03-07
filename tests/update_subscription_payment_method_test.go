//go:build integration

package tests

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/pkg/api"
)

// TestUpdateSubscriptionPaymentMethodRequiresAuth tests that the endpoint requires authentication
func TestUpdateSubscriptionPaymentMethodRequiresAuth(t *testing.T) {
	suite := setupTestSuite(t)

	t.Run("returns 401 without auth token", func(t *testing.T) {
		body := map[string]string{
			"subscription_id":   uuid.New().String(),
			"payment_method_id": uuid.New().String(),
		}
		jsonBody, _ := json.Marshal(body)

		w := httptest.NewRecorder()
		req, _ := http.NewRequest("PUT", "/v1/me/subscriptions/payment-method", bytes.NewReader(jsonBody))
		req.Header.Set("Content-Type", "application/json")

		suite.Server.Handler().ServeHTTP(w, req)

		assert.Equal(t, http.StatusUnauthorized, w.Code, "Should return 401 Unauthorized")
	})
}

// TestUpdateSubscriptionPaymentMethodSuccess tests successful payment method update
func TestUpdateSubscriptionPaymentMethodSuccess(t *testing.T) {
	suite, mock := SetupSuiteWithMockNMI(t)

	// Seed products and prices
	products := suite.SeedProducts()
	priceID := products[0].Prices[0].ID

	// Create auth token for test user
	userID := uuid.New().String()
	email := "update-pm-success-" + t.Name() + "@test.example.com"
	token := getTestIssuer().CreateToken(userID, email)

	// Create an active subscription for the user
	oldPM := suite.CreateTestPaymentMethodWithOptions(PaymentMethodOptions{
		UserID:    userID,
		Processor: models.ProcessorMobius,
		VaultID:   "old-vault-123",
		LastFour:  "4242",
		CardType:  "Visa",
	})

	sub := suite.CreateTestSubscriptionWithOptions(SubscriptionOptions{
		UserID:          userID,
		PriceID:         priceID,
		Status:          models.StatusActive,
		Processor:       models.ProcessorMobius,
		ProcessorSubID:  "sub-to-update-123",
		PaymentMethodID: &oldPM.ID,
	})

	// Create new payment method to swap to
	newPM := suite.CreateTestPaymentMethodWithOptions(PaymentMethodOptions{
		UserID:    userID,
		Processor: models.ProcessorMobius,
		VaultID:   "new-vault-456",
		LastFour:  "1234",
		CardType:  "Mastercard",
	})

	t.Run("updates subscription payment method successfully", func(t *testing.T) {
		mock.Reset()

		body := map[string]string{
			"subscription_id":   sub.ID.String(),
			"payment_method_id": newPM.ID.String(),
		}
		jsonBody, _ := json.Marshal(body)

		w := httptest.NewRecorder()
		req, _ := http.NewRequest("PUT", "/v1/me/subscriptions/payment-method", bytes.NewReader(jsonBody))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)

		suite.Server.Handler().ServeHTTP(w, req)

		require.Equal(t, http.StatusOK, w.Code, "Should return 200 OK, got body: %s", w.Body.String())

		var response map[string]interface{}
		err := json.Unmarshal(w.Body.Bytes(), &response)
		require.NoError(t, err)

		assert.True(t, response["success"].(bool), "Success should be true")
		assert.Equal(t, newPM.ID.String(), response["payment_method_id"], "Response should contain new payment method ID")

		// Verify NMI was called with update_subscription
		assert.Contains(t, mock.LastRequest["recurring"], "update_subscription", "Should call NMI with update_subscription")
		assert.Contains(t, mock.LastRequest["subscription_id"], "sub-to-update-123", "Should send subscription ID")
		assert.Contains(t, mock.LastRequest["customer_vault_id"], "new-vault-456", "Should send new vault ID")

		// Verify subscription was updated in database
		updatedSub := suite.GetSubscription(sub.ID)
		require.NotNil(t, updatedSub.PaymentMethodID, "Subscription should have payment method")
		assert.Equal(t, newPM.ID, *updatedSub.PaymentMethodID, "Subscription should have new payment method")
	})

	t.Run("accepts prefixed subscription and payment method IDs", func(t *testing.T) {
		mock.Reset()

		body := map[string]string{
			"subscription_id":   api.FormatSubscriptionID(sub.ID),
			"payment_method_id": api.FormatPaymentMethodID(newPM.ID),
		}
		jsonBody, _ := json.Marshal(body)

		w := httptest.NewRecorder()
		req, _ := http.NewRequest("PUT", "/v1/me/subscriptions/payment-method", bytes.NewReader(jsonBody))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)

		suite.Server.Handler().ServeHTTP(w, req)

		require.Equal(t, http.StatusOK, w.Code, "Should accept prefixed IDs, body: %s", w.Body.String())
	})
}

// TestUpdateSubscriptionPaymentMethodNotOwned tests that users can't update other users' subscriptions
func TestUpdateSubscriptionPaymentMethodNotOwned(t *testing.T) {
	suite, _ := SetupSuiteWithMockNMI(t)

	// Seed products and prices
	products := suite.SeedProducts()
	priceID := products[0].Prices[0].ID

	// Create auth token for test user
	userID := uuid.New().String()
	email := "update-pm-not-owned-" + t.Name() + "@test.example.com"
	token := getTestIssuer().CreateToken(userID, email)

	// Create subscription owned by different user
	otherUserID := uuid.New().String()
	otherSub := suite.CreateTestSubscription(otherUserID, priceID, models.StatusActive)

	// Create payment method for current user
	pm := suite.CreateTestPaymentMethodWithOptions(PaymentMethodOptions{
		UserID:    userID,
		Processor: models.ProcessorMobius,
	})

	t.Run("returns 403 for subscription owned by another user", func(t *testing.T) {
		body := map[string]string{
			"subscription_id":   otherSub.ID.String(),
			"payment_method_id": pm.ID.String(),
		}
		jsonBody, _ := json.Marshal(body)

		w := httptest.NewRecorder()
		req, _ := http.NewRequest("PUT", "/v1/me/subscriptions/payment-method", bytes.NewReader(jsonBody))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)

		suite.Server.Handler().ServeHTTP(w, req)

		assert.Equal(t, http.StatusForbidden, w.Code, "Should return 403 Forbidden")
	})
}

// TestUpdateSubscriptionPaymentMethodNotOwnedPM tests that users can't use other users' payment methods
func TestUpdateSubscriptionPaymentMethodNotOwnedPM(t *testing.T) {
	suite, _ := SetupSuiteWithMockNMI(t)

	// Seed products and prices
	products := suite.SeedProducts()
	priceID := products[0].Prices[0].ID

	// Create auth token for test user
	userID := uuid.New().String()
	email := "update-pm-not-owned-pm-" + t.Name() + "@test.example.com"
	token := getTestIssuer().CreateToken(userID, email)

	// Create subscription for current user
	sub := suite.CreateTestSubscription(userID, priceID, models.StatusActive)

	// Create payment method owned by different user
	otherUserID := uuid.New().String()
	otherPM := suite.CreateTestPaymentMethodWithOptions(PaymentMethodOptions{
		UserID:    otherUserID,
		Processor: models.ProcessorMobius,
	})

	t.Run("returns 403 for payment method owned by another user", func(t *testing.T) {
		body := map[string]string{
			"subscription_id":   sub.ID.String(),
			"payment_method_id": otherPM.ID.String(),
		}
		jsonBody, _ := json.Marshal(body)

		w := httptest.NewRecorder()
		req, _ := http.NewRequest("PUT", "/v1/me/subscriptions/payment-method", bytes.NewReader(jsonBody))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)

		suite.Server.Handler().ServeHTTP(w, req)

		assert.Equal(t, http.StatusForbidden, w.Code, "Should return 403 Forbidden")
	})
}

// TestUpdateSubscriptionPaymentMethodCancelledSub tests that cancelled subscriptions can't be updated
func TestUpdateSubscriptionPaymentMethodCancelledSub(t *testing.T) {
	suite, _ := SetupSuiteWithMockNMI(t)

	// Seed products and prices
	products := suite.SeedProducts()
	priceID := products[0].Prices[0].ID

	// Create auth token for test user
	userID := uuid.New().String()
	email := "update-pm-cancelled-" + t.Name() + "@test.example.com"
	token := getTestIssuer().CreateToken(userID, email)

	// Create cancelled subscription
	cancelledSub := suite.CreateTestSubscriptionWithOptions(SubscriptionOptions{
		UserID:    userID,
		PriceID:   priceID,
		Status:    models.StatusCancelled,
		Processor: models.ProcessorMobius,
	})

	// Create active payment method
	pm := suite.CreateTestPaymentMethodWithOptions(PaymentMethodOptions{
		UserID:    userID,
		Processor: models.ProcessorMobius,
	})

	t.Run("returns error for cancelled subscription", func(t *testing.T) {
		body := map[string]string{
			"subscription_id":   cancelledSub.ID.String(),
			"payment_method_id": pm.ID.String(),
		}
		jsonBody, _ := json.Marshal(body)

		w := httptest.NewRecorder()
		req, _ := http.NewRequest("PUT", "/v1/me/subscriptions/payment-method", bytes.NewReader(jsonBody))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)

		suite.Server.Handler().ServeHTTP(w, req)

		assert.Equal(t, http.StatusBadRequest, w.Code, "Should return 400 Bad Request for cancelled subscription")
	})
}

// TestUpdateSubscriptionPaymentMethodCCBillNotSupported tests that CCBill subscriptions can't be updated
func TestUpdateSubscriptionPaymentMethodCCBillNotSupported(t *testing.T) {
	suite, _ := SetupSuiteWithMockNMI(t)

	// Seed products and prices
	products := suite.SeedProducts()
	priceID := products[0].Prices[0].ID

	// Create auth token for test user
	userID := uuid.New().String()
	email := "update-pm-ccbill-" + t.Name() + "@test.example.com"
	token := getTestIssuer().CreateToken(userID, email)

	// Create CCBill subscription (can't have payment method updated)
	ccbillSub := suite.CreateTestSubscriptionWithOptions(SubscriptionOptions{
		UserID:    userID,
		PriceID:   priceID,
		Status:    models.StatusActive,
		Processor: models.ProcessorCCBill,
	})

	// Create NMI payment method
	pm := suite.CreateTestPaymentMethodWithOptions(PaymentMethodOptions{
		UserID:    userID,
		Processor: models.ProcessorMobius,
	})

	t.Run("returns error for CCBill subscription", func(t *testing.T) {
		body := map[string]string{
			"subscription_id":   ccbillSub.ID.String(),
			"payment_method_id": pm.ID.String(),
		}
		jsonBody, _ := json.Marshal(body)

		w := httptest.NewRecorder()
		req, _ := http.NewRequest("PUT", "/v1/me/subscriptions/payment-method", bytes.NewReader(jsonBody))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)

		suite.Server.Handler().ServeHTTP(w, req)

		assert.Equal(t, http.StatusBadRequest, w.Code, "Should return 400 Bad Request for CCBill subscription")
	})
}

// TestUpdateSubscriptionPaymentMethodNotFound tests non-existent subscription/payment method
func TestUpdateSubscriptionPaymentMethodNotFound(t *testing.T) {
	suite, _ := SetupSuiteWithMockNMI(t)

	// Seed products and prices
	products := suite.SeedProducts()
	priceID := products[0].Prices[0].ID

	// Create auth token for test user
	userID := uuid.New().String()
	email := "update-pm-notfound-" + t.Name() + "@test.example.com"
	token := getTestIssuer().CreateToken(userID, email)

	// Create subscription for user
	sub := suite.CreateTestSubscription(userID, priceID, models.StatusActive)

	// Create payment method for user
	pm := suite.CreateTestPaymentMethodWithOptions(PaymentMethodOptions{
		UserID:    userID,
		Processor: models.ProcessorMobius,
	})

	t.Run("returns 404 for non-existent subscription", func(t *testing.T) {
		body := map[string]string{
			"subscription_id":   uuid.New().String(),
			"payment_method_id": pm.ID.String(),
		}
		jsonBody, _ := json.Marshal(body)

		w := httptest.NewRecorder()
		req, _ := http.NewRequest("PUT", "/v1/me/subscriptions/payment-method", bytes.NewReader(jsonBody))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)

		suite.Server.Handler().ServeHTTP(w, req)

		assert.Equal(t, http.StatusNotFound, w.Code, "Should return 404 Not Found for non-existent subscription")
	})

	t.Run("returns 404 for non-existent payment method", func(t *testing.T) {
		body := map[string]string{
			"subscription_id":   sub.ID.String(),
			"payment_method_id": uuid.New().String(),
		}
		jsonBody, _ := json.Marshal(body)

		w := httptest.NewRecorder()
		req, _ := http.NewRequest("PUT", "/v1/me/subscriptions/payment-method", bytes.NewReader(jsonBody))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)

		suite.Server.Handler().ServeHTTP(w, req)

		assert.Equal(t, http.StatusNotFound, w.Code, "Should return 404 Not Found for non-existent payment method")
	})
}

// TestUpdateSubscriptionPaymentMethodInvalidRequest tests invalid request body
func TestUpdateSubscriptionPaymentMethodInvalidRequest(t *testing.T) {
	suite, _ := SetupSuiteWithMockNMI(t)

	// Create auth token for test user
	userID := uuid.New().String()
	email := "update-pm-invalid-" + t.Name() + "@test.example.com"
	token := getTestIssuer().CreateToken(userID, email)

	t.Run("returns error for missing subscription_id", func(t *testing.T) {
		body := map[string]string{
			"payment_method_id": uuid.New().String(),
		}
		jsonBody, _ := json.Marshal(body)

		w := httptest.NewRecorder()
		req, _ := http.NewRequest("PUT", "/v1/me/subscriptions/payment-method", bytes.NewReader(jsonBody))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)

		suite.Server.Handler().ServeHTTP(w, req)

		assert.Equal(t, http.StatusBadRequest, w.Code, "Should return 400 Bad Request for missing subscription_id")
	})

	t.Run("returns error for missing payment_method_id", func(t *testing.T) {
		body := map[string]string{
			"subscription_id": uuid.New().String(),
		}
		jsonBody, _ := json.Marshal(body)

		w := httptest.NewRecorder()
		req, _ := http.NewRequest("PUT", "/v1/me/subscriptions/payment-method", bytes.NewReader(jsonBody))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)

		suite.Server.Handler().ServeHTTP(w, req)

		assert.Equal(t, http.StatusBadRequest, w.Code, "Should return 400 Bad Request for missing payment_method_id")
	})

	t.Run("returns error for invalid UUID format", func(t *testing.T) {
		body := map[string]string{
			"subscription_id":   "not-a-uuid",
			"payment_method_id": uuid.New().String(),
		}
		jsonBody, _ := json.Marshal(body)

		w := httptest.NewRecorder()
		req, _ := http.NewRequest("PUT", "/v1/me/subscriptions/payment-method", bytes.NewReader(jsonBody))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)

		suite.Server.Handler().ServeHTTP(w, req)

		assert.Equal(t, http.StatusBadRequest, w.Code, "Should return 400 Bad Request for invalid UUID format")
	})
}

// TestUpdateSubscriptionPaymentMethodPastDue tests that past_due subscriptions CAN be updated
func TestUpdateSubscriptionPaymentMethodPastDue(t *testing.T) {
	suite, mock := SetupSuiteWithMockNMI(t)

	// Seed products and prices
	products := suite.SeedProducts()
	priceID := products[0].Prices[0].ID

	// Create auth token for test user
	userID := uuid.New().String()
	email := "update-pm-pastdue-" + t.Name() + "@test.example.com"
	token := getTestIssuer().CreateToken(userID, email)

	// Create past_due subscription (payment failed but still retrying)
	pastDueSub := suite.CreateTestSubscriptionWithOptions(SubscriptionOptions{
		UserID:    userID,
		PriceID:   priceID,
		Status:    models.StatusPastDue,
		Processor: models.ProcessorMobius,
	})

	// Create new payment method
	newPM := suite.CreateTestPaymentMethodWithOptions(PaymentMethodOptions{
		UserID:    userID,
		Processor: models.ProcessorMobius,
		VaultID:   "new-vault-pastdue",
	})

	t.Run("allows updating payment method for past_due subscription", func(t *testing.T) {
		mock.Reset()

		body := map[string]string{
			"subscription_id":   pastDueSub.ID.String(),
			"payment_method_id": newPM.ID.String(),
		}
		jsonBody, _ := json.Marshal(body)

		w := httptest.NewRecorder()
		req, _ := http.NewRequest("PUT", "/v1/me/subscriptions/payment-method", bytes.NewReader(jsonBody))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)

		suite.Server.Handler().ServeHTTP(w, req)

		require.Equal(t, http.StatusOK, w.Code, "Should return 200 OK for past_due subscription, got body: %s", w.Body.String())

		var response map[string]interface{}
		err := json.Unmarshal(w.Body.Bytes(), &response)
		require.NoError(t, err)

		assert.True(t, response["success"].(bool), "Success should be true")
	})
}

// TestUpdateSubscriptionPaymentMethodNMIFailure tests NMI API failure handling
func TestUpdateSubscriptionPaymentMethodNMIFailure(t *testing.T) {
	suite, mock := SetupSuiteWithMockNMI(t)

	// Seed products and prices
	products := suite.SeedProducts()
	priceID := products[0].Prices[0].ID

	// Create auth token for test user
	userID := uuid.New().String()
	email := "update-pm-nmifail-" + t.Name() + "@test.example.com"
	token := getTestIssuer().CreateToken(userID, email)

	// Create subscription
	sub := suite.CreateTestSubscription(userID, priceID, models.StatusActive)

	// Create payment method
	pm := suite.CreateTestPaymentMethodWithOptions(PaymentMethodOptions{
		UserID:    userID,
		Processor: models.ProcessorMobius,
	})

	t.Run("returns error when NMI API fails", func(t *testing.T) {
		mock.Reset()
		mock.ShouldFail = true
		mock.FailReason = "Subscription not found"

		body := map[string]string{
			"subscription_id":   sub.ID.String(),
			"payment_method_id": pm.ID.String(),
		}
		jsonBody, _ := json.Marshal(body)

		w := httptest.NewRecorder()
		req, _ := http.NewRequest("PUT", "/v1/me/subscriptions/payment-method", bytes.NewReader(jsonBody))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)

		suite.Server.Handler().ServeHTTP(w, req)

		assert.Equal(t, http.StatusBadGateway, w.Code, "Should return 502 Bad Gateway when NMI fails")
	})
}
