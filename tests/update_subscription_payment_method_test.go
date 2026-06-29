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
	suite := getSharedTestSuite(t)

	t.Run("returns 401 without auth token", func(t *testing.T) {
		subscriptionID := uuid.New().String()
		body := map[string]string{
			"payment_method_id": uuid.New().String(),
		}
		jsonBody, _ := json.Marshal(body)

		w := httptest.NewRecorder()
		req, _ := http.NewRequest("PUT", updateSubscriptionPaymentMethodPath(subscriptionID), bytes.NewReader(jsonBody))
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
		UserID:   userID,
		Rail:     models.RailNMI,
		VaultID:  "old-vault-" + uuid.New().String(),
		LastFour: "4242",
		CardType: "Visa",
	})

	railSubID := "sub-to-update-" + uuid.New().String()
	sub := suite.CreateTestSubscriptionWithOptions(SubscriptionOptions{
		UserID:          userID,
		PriceID:         priceID,
		Status:          models.StatusActive,
		Rail:            models.RailNMI,
		RailSubID:       railSubID,
		PaymentMethodID: &oldPM.ID,
	})

	// Create new payment method to swap to
	newPM := suite.CreateTestPaymentMethodWithOptions(PaymentMethodOptions{
		UserID:   userID,
		Rail:     models.RailNMI,
		VaultID:  "new-vault-" + uuid.New().String(),
		LastFour: "1234",
		CardType: "Mastercard",
	})

	t.Run("updates subscription payment method successfully", func(t *testing.T) {
		mock.Reset()

		body := map[string]string{
			"payment_method_id": newPM.ID.String(),
		}
		jsonBody, _ := json.Marshal(body)

		w := httptest.NewRecorder()
		req, _ := http.NewRequest("PUT", updateSubscriptionPaymentMethodPath(sub.ID.String()), bytes.NewReader(jsonBody))
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
		assert.Contains(t, mock.LastRequest["subscription_id"], railSubID, "Should send subscription ID")
		assert.Contains(t, mock.LastRequest["customer_vault_id"], newPM.RailCustomerRef, "Should send new vault ID")

		// Verify subscription was updated in database
		updatedSub := suite.GetSubscription(sub.ID)
		require.NotNil(t, updatedSub.PaymentMethodID, "Subscription should have payment method")
		assert.Equal(t, newPM.ID, *updatedSub.PaymentMethodID, "Subscription should have new payment method")
	})

	t.Run("accepts prefixed subscription and payment method IDs", func(t *testing.T) {
		mock.Reset()

		body := map[string]string{
			"payment_method_id": api.FormatPaymentMethodID(newPM.ID),
		}
		jsonBody, _ := json.Marshal(body)

		w := httptest.NewRecorder()
		req, _ := http.NewRequest("PUT", updateSubscriptionPaymentMethodPath(api.FormatSubscriptionID(sub.ID)), bytes.NewReader(jsonBody))
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
		UserID: userID,
		Rail:   models.RailNMI,
	})

	t.Run("returns 403 for subscription owned by another user", func(t *testing.T) {
		body := map[string]string{
			"payment_method_id": pm.ID.String(),
		}
		jsonBody, _ := json.Marshal(body)

		w := httptest.NewRecorder()
		req, _ := http.NewRequest("PUT", updateSubscriptionPaymentMethodPath(otherSub.ID.String()), bytes.NewReader(jsonBody))
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
		UserID: otherUserID,
		Rail:   models.RailNMI,
	})

	t.Run("returns 403 for payment method owned by another user", func(t *testing.T) {
		body := map[string]string{
			"payment_method_id": otherPM.ID.String(),
		}
		jsonBody, _ := json.Marshal(body)

		w := httptest.NewRecorder()
		req, _ := http.NewRequest("PUT", updateSubscriptionPaymentMethodPath(sub.ID.String()), bytes.NewReader(jsonBody))
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
		UserID:  userID,
		PriceID: priceID,
		Status:  models.StatusCancelled,
		Rail:    models.RailNMI,
	})

	// Create active payment method
	pm := suite.CreateTestPaymentMethodWithOptions(PaymentMethodOptions{
		UserID: userID,
		Rail:   models.RailNMI,
	})

	t.Run("returns error for cancelled subscription", func(t *testing.T) {
		body := map[string]string{
			"payment_method_id": pm.ID.String(),
		}
		jsonBody, _ := json.Marshal(body)

		w := httptest.NewRecorder()
		req, _ := http.NewRequest("PUT", updateSubscriptionPaymentMethodPath(cancelledSub.ID.String()), bytes.NewReader(jsonBody))
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
		UserID:  userID,
		PriceID: priceID,
		Status:  models.StatusActive,
		Rail:    models.RailCCBill,
	})

	// Create NMI payment method
	pm := suite.CreateTestPaymentMethodWithOptions(PaymentMethodOptions{
		UserID: userID,
		Rail:   models.RailNMI,
	})

	t.Run("returns error for CCBill subscription", func(t *testing.T) {
		body := map[string]string{
			"payment_method_id": pm.ID.String(),
		}
		jsonBody, _ := json.Marshal(body)

		w := httptest.NewRecorder()
		req, _ := http.NewRequest("PUT", updateSubscriptionPaymentMethodPath(ccbillSub.ID.String()), bytes.NewReader(jsonBody))
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
		UserID: userID,
		Rail:   models.RailNMI,
	})

	t.Run("returns 404 for non-existent subscription", func(t *testing.T) {
		body := map[string]string{
			"payment_method_id": pm.ID.String(),
		}
		jsonBody, _ := json.Marshal(body)

		w := httptest.NewRecorder()
		req, _ := http.NewRequest("PUT", updateSubscriptionPaymentMethodPath(uuid.New().String()), bytes.NewReader(jsonBody))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)

		suite.Server.Handler().ServeHTTP(w, req)

		assert.Equal(t, http.StatusNotFound, w.Code, "Should return 404 Not Found for non-existent subscription")
	})

	t.Run("returns 404 for non-existent payment method", func(t *testing.T) {
		body := map[string]string{
			"payment_method_id": uuid.New().String(),
		}
		jsonBody, _ := json.Marshal(body)

		w := httptest.NewRecorder()
		req, _ := http.NewRequest("PUT", updateSubscriptionPaymentMethodPath(sub.ID.String()), bytes.NewReader(jsonBody))
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

	t.Run("returns error for invalid subscription ID", func(t *testing.T) {
		body := map[string]string{
			"payment_method_id": uuid.New().String(),
		}
		jsonBody, _ := json.Marshal(body)

		w := httptest.NewRecorder()
		req, _ := http.NewRequest("PUT", updateSubscriptionPaymentMethodPath("not-a-uuid"), bytes.NewReader(jsonBody))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)

		suite.Server.Handler().ServeHTTP(w, req)

		assert.Equal(t, http.StatusBadRequest, w.Code, "Should return 400 Bad Request for invalid subscription ID")
	})

	t.Run("returns error for missing payment_method_id", func(t *testing.T) {
		body := map[string]string{}
		jsonBody, _ := json.Marshal(body)

		w := httptest.NewRecorder()
		req, _ := http.NewRequest("PUT", updateSubscriptionPaymentMethodPath(uuid.New().String()), bytes.NewReader(jsonBody))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)

		suite.Server.Handler().ServeHTTP(w, req)

		assert.Equal(t, http.StatusBadRequest, w.Code, "Should return 400 Bad Request for missing payment_method_id")
	})

	t.Run("returns error for invalid payment method UUID format", func(t *testing.T) {
		body := map[string]string{
			"payment_method_id": "not-a-uuid",
		}
		jsonBody, _ := json.Marshal(body)

		w := httptest.NewRecorder()
		req, _ := http.NewRequest("PUT", updateSubscriptionPaymentMethodPath(uuid.New().String()), bytes.NewReader(jsonBody))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)

		suite.Server.Handler().ServeHTTP(w, req)

		assert.Equal(t, http.StatusBadRequest, w.Code, "Should return 400 Bad Request for invalid payment method UUID format")
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
		UserID:  userID,
		PriceID: priceID,
		Status:  models.StatusPastDue,
		Rail:    models.RailNMI,
	})

	// Create new payment method
	newPM := suite.CreateTestPaymentMethodWithOptions(PaymentMethodOptions{
		UserID:  userID,
		Rail:    models.RailNMI,
		VaultID: "new-vault-pastdue-" + uuid.New().String(),
	})

	t.Run("allows updating payment method for past_due subscription", func(t *testing.T) {
		mock.Reset()

		body := map[string]string{
			"payment_method_id": newPM.ID.String(),
		}
		jsonBody, _ := json.Marshal(body)

		w := httptest.NewRecorder()
		req, _ := http.NewRequest("PUT", updateSubscriptionPaymentMethodPath(pastDueSub.ID.String()), bytes.NewReader(jsonBody))
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
		UserID: userID,
		Rail:   models.RailNMI,
	})

	t.Run("returns error when NMI API fails", func(t *testing.T) {
		mock.Reset()
		mock.ShouldFail = true
		mock.FailReason = "Subscription not found"

		body := map[string]string{
			"payment_method_id": pm.ID.String(),
		}
		jsonBody, _ := json.Marshal(body)

		w := httptest.NewRecorder()
		req, _ := http.NewRequest("PUT", updateSubscriptionPaymentMethodPath(sub.ID.String()), bytes.NewReader(jsonBody))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)

		suite.Server.Handler().ServeHTTP(w, req)

		assert.Equal(t, http.StatusBadGateway, w.Code, "Should return 502 Bad Gateway when NMI fails")
	})
}

func updateSubscriptionPaymentMethodPath(subscriptionID string) string {
	return "/v1/me/subscriptions/" + subscriptionID + "/payment-method"
}
