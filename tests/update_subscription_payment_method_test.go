//go:build integration

package tests

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/open-rails/openrails"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/controlplane"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/http/middleware"
)

func TestUpdateSubscriptionPaymentMethodCustodianHeldTarget(t *testing.T) {
	suite, mock := SetupSuiteWithMockNMI(t)
	user := uuid.NewString()
	token := suite.MintUserToken(user, "custodian-target@test.example.com")
	oldPM := suite.CreateTestPaymentMethodWithOptions(PaymentMethodOptions{UserID: user, Rail: models.RailNMI, VaultID: "old-vault"})
	newPM := suite.CreateTestPaymentMethodWithOptions(PaymentMethodOptions{UserID: user, Rail: models.RailNMI, VaultID: "historical-vault"})
	sub := suite.CreateTestSubscriptionWithOptions(SubscriptionOptions{UserID: user, PriceID: suite.SeedProducts()[0].Prices[0].ID, Status: models.StatusActive, Rail: models.RailNMI, RailSubID: "provider-sub-" + uuid.NewString(), PaymentMethodID: &oldPM.ID})
	custodian := uuid.New()
	ctx := t.Context()
	_, err := suite.MerchantPool().Exec(ctx, `INSERT INTO openrails.custodians(id,merchant_id,key,kind,account_id) VALUES($1,$2,$3,'basis_theory',$3)`, custodian, dbtest.TestMerchantID.UUID(), "source-update-"+custodian.String())
	require.NoError(t, err)
	_, err = suite.MerchantPool().Exec(ctx, `UPDATE openrails.payment_methods SET custodian='basis_theory',custodian_id=$2,rail_method_ref='custodian-token' WHERE id=$1`, newPM.ID, custodian)
	require.NoError(t, err)
	mock.Reset()
	body, err := json.Marshal(openrails.UpdateSubscriptionPaymentMethodRequest{PaymentMethodID: openrails.PaymentMethodID(newPM.ID)})
	require.NoError(t, err)
	request := httptest.NewRequest(http.MethodPut, updateSubscriptionPaymentMethodPath(openrails.SubscriptionID(sub.ID).String()), bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+token)
	response := httptest.NewRecorder()
	suite.Server.Handler().ServeHTTP(response, request)
	require.Equal(t, http.StatusConflict, response.Code, response.Body.String())
	require.Contains(t, response.Body.String(), `"code":"payment_method_not_psp_vaulted"`)
	require.Empty(t, mock.LastRequest)
	require.Equal(t, oldPM.ID, *suite.GetSubscription(sub.ID).PaymentMethodID)
}

// TestUpdateSubscriptionPaymentMethodRequiresAuth tests that the endpoint requires authentication
func TestUpdateSubscriptionPaymentMethodRequiresAuth(t *testing.T) {
	suite := getSharedTestSuite(t)

	t.Run("returns 401 without auth token", func(t *testing.T) {
		subscriptionID := openrails.SubscriptionID(uuid.New()).String()
		body := map[string]string{
			"payment_method_id": openrails.PaymentMethodID(uuid.New()).String(),
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
	token := suite.MintUserToken(userID, email)

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
			"payment_method_id": openrails.PaymentMethodID(newPM.ID).String(),
		}
		jsonBody, _ := json.Marshal(body)

		w := httptest.NewRecorder()
		req, _ := http.NewRequest("PUT", updateSubscriptionPaymentMethodPath(openrails.SubscriptionID(sub.ID).String()), bytes.NewReader(jsonBody))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)

		suite.Server.Handler().ServeHTTP(w, req)

		require.Equal(t, http.StatusOK, w.Code, "Should return 200 OK, got body: %s", w.Body.String())

		var response map[string]interface{}
		err := json.Unmarshal(w.Body.Bytes(), &response)
		require.NoError(t, err)

		assert.True(t, response["success"].(bool), "Success should be true")
		assert.Equal(t, openrails.PaymentMethodID(newPM.ID).String(), response["payment_method_id"], "Response should contain new payment method ID")

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
			"payment_method_id": openrails.PaymentMethodID(newPM.ID).String(),
		}
		jsonBody, _ := json.Marshal(body)

		w := httptest.NewRecorder()
		req, _ := http.NewRequest("PUT", updateSubscriptionPaymentMethodPath(openrails.SubscriptionID(sub.ID).String()), bytes.NewReader(jsonBody))
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
	token := suite.MintUserToken(userID, email)

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
			"payment_method_id": openrails.PaymentMethodID(pm.ID).String(),
		}
		jsonBody, _ := json.Marshal(body)

		w := httptest.NewRecorder()
		req, _ := http.NewRequest("PUT", updateSubscriptionPaymentMethodPath(openrails.SubscriptionID(otherSub.ID).String()), bytes.NewReader(jsonBody))
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
	token := suite.MintUserToken(userID, email)

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
			"payment_method_id": openrails.PaymentMethodID(otherPM.ID).String(),
		}
		jsonBody, _ := json.Marshal(body)

		w := httptest.NewRecorder()
		req, _ := http.NewRequest("PUT", updateSubscriptionPaymentMethodPath(openrails.SubscriptionID(sub.ID).String()), bytes.NewReader(jsonBody))
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
	token := suite.MintUserToken(userID, email)

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
			"payment_method_id": openrails.PaymentMethodID(pm.ID).String(),
		}
		jsonBody, _ := json.Marshal(body)

		w := httptest.NewRecorder()
		req, _ := http.NewRequest("PUT", updateSubscriptionPaymentMethodPath(openrails.SubscriptionID(cancelledSub.ID).String()), bytes.NewReader(jsonBody))
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
	token := suite.MintUserToken(userID, email)

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
			"payment_method_id": openrails.PaymentMethodID(pm.ID).String(),
		}
		jsonBody, _ := json.Marshal(body)

		w := httptest.NewRecorder()
		req, _ := http.NewRequest("PUT", updateSubscriptionPaymentMethodPath(openrails.SubscriptionID(ccbillSub.ID).String()), bytes.NewReader(jsonBody))
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
	token := suite.MintUserToken(userID, email)

	// Create subscription for user
	sub := suite.CreateTestSubscription(userID, priceID, models.StatusActive)

	// Create payment method for user
	pm := suite.CreateTestPaymentMethodWithOptions(PaymentMethodOptions{
		UserID: userID,
		Rail:   models.RailNMI,
	})

	t.Run("returns 404 for non-existent subscription", func(t *testing.T) {
		body := map[string]string{
			"payment_method_id": openrails.PaymentMethodID(pm.ID).String(),
		}
		jsonBody, _ := json.Marshal(body)

		w := httptest.NewRecorder()
		req, _ := http.NewRequest("PUT", updateSubscriptionPaymentMethodPath(openrails.SubscriptionID(uuid.New()).String()), bytes.NewReader(jsonBody))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)

		suite.Server.Handler().ServeHTTP(w, req)

		assert.Equal(t, http.StatusNotFound, w.Code, "Should return 404 Not Found for non-existent subscription")
	})

	t.Run("returns 404 for non-existent payment method", func(t *testing.T) {
		body := map[string]string{
			"payment_method_id": openrails.PaymentMethodID(uuid.New()).String(),
		}
		jsonBody, _ := json.Marshal(body)

		w := httptest.NewRecorder()
		req, _ := http.NewRequest("PUT", updateSubscriptionPaymentMethodPath(openrails.SubscriptionID(sub.ID).String()), bytes.NewReader(jsonBody))
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
	token := suite.MintUserToken(userID, email)

	t.Run("returns error for invalid subscription ID", func(t *testing.T) {
		body := map[string]string{
			"payment_method_id": openrails.PaymentMethodID(uuid.New()).String(),
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
		req, _ := http.NewRequest("PUT", updateSubscriptionPaymentMethodPath(openrails.SubscriptionID(uuid.New()).String()), bytes.NewReader(jsonBody))
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
		req, _ := http.NewRequest("PUT", updateSubscriptionPaymentMethodPath(openrails.SubscriptionID(uuid.New()).String()), bytes.NewReader(jsonBody))
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
	token := suite.MintUserToken(userID, email)

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
			"payment_method_id": openrails.PaymentMethodID(newPM.ID).String(),
		}
		jsonBody, _ := json.Marshal(body)

		w := httptest.NewRecorder()
		req, _ := http.NewRequest("PUT", updateSubscriptionPaymentMethodPath(openrails.SubscriptionID(pastDueSub.ID).String()), bytes.NewReader(jsonBody))
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
	token := suite.MintUserToken(userID, email)

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
			"payment_method_id": openrails.PaymentMethodID(pm.ID).String(),
		}
		jsonBody, _ := json.Marshal(body)

		w := httptest.NewRecorder()
		req, _ := http.NewRequest("PUT", updateSubscriptionPaymentMethodPath(openrails.SubscriptionID(sub.ID).String()), bytes.NewReader(jsonBody))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)

		suite.Server.Handler().ServeHTTP(w, req)

		assert.Equal(t, http.StatusBadGateway, w.Code, "Should return 502 Bad Gateway when NMI fails")
	})
}

// A saved method vaulted by ANOTHER provider account is refused with the
// coded 409 payment_method_psp_mismatch on both the self-service HTTP path and
// the merchant Client path, and nothing reaches NMI (#657). The re-attribution
// is what a #297 custody remap writes: psp_id only, the vault handle stays.
func TestUpdateSubscriptionPaymentMethodCrossPSP(t *testing.T) {
	suite, mock := SetupSuiteWithMockNMI(t)
	products := suite.SeedProducts()
	priceID := products[0].Prices[0].ID
	userID := uuid.New().String()
	token := suite.MintUserToken(userID, "update-pm-crosspsp-"+t.Name()+"@test.example.com")

	oldPM := suite.CreateTestPaymentMethodWithOptions(PaymentMethodOptions{UserID: userID, Rail: models.RailNMI, VaultID: "old-vault-" + uuid.NewString()})
	sub := suite.CreateTestSubscriptionWithOptions(SubscriptionOptions{
		UserID: userID, PriceID: priceID, Status: models.StatusActive, Rail: models.RailNMI,
		RailSubID: "sub-crosspsp-" + uuid.NewString(), PaymentMethodID: &oldPM.ID,
	})
	newPM := suite.CreateTestPaymentMethodWithOptions(PaymentMethodOptions{UserID: userID, Rail: models.RailNMI, VaultID: "new-vault-" + uuid.NewString()})

	ctx := context.Background()
	var homePSP uuid.UUID
	require.NoError(t, suite.MerchantPool().QueryRow(ctx, `SELECT psp_id FROM openrails.payment_methods WHERE id = $1`, oldPM.ID).Scan(&homePSP))
	otherPSP := uuid.New()
	_, err := suite.MerchantPool().Exec(ctx,
		`INSERT INTO openrails.psps (id, merchant_id, rail, environment, account_id, archived) VALUES ($1, $2, 'nmi', 'test', $3, true)`,
		otherPSP, dbtest.TestMerchantID.UUID(), "other-"+uuid.NewString()[:8])
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = suite.MerchantPool().Exec(ctx, `UPDATE openrails.payment_methods SET psp_id = $1 WHERE id = $2`, homePSP, newPM.ID)
		_, _ = suite.MerchantPool().Exec(ctx, `DELETE FROM openrails.psps WHERE id = $1`, otherPSP)
	})
	_, err = suite.MerchantPool().Exec(ctx, `UPDATE openrails.payment_methods SET psp_id = $1 WHERE id = $2`, otherPSP, newPM.ID)
	require.NoError(t, err)

	t.Run("self-service HTTP answers the coded 409", func(t *testing.T) {
		mock.Reset()
		jsonBody, _ := json.Marshal(map[string]string{"payment_method_id": openrails.PaymentMethodID(newPM.ID).String()})
		w := httptest.NewRecorder()
		req, _ := http.NewRequest("PUT", updateSubscriptionPaymentMethodPath(openrails.SubscriptionID(sub.ID).String()), bytes.NewReader(jsonBody))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)
		suite.Server.Handler().ServeHTTP(w, req)

		require.Equal(t, http.StatusConflict, w.Code, w.Body.String())
		var envelope struct {
			Error openrails.ErrorDetails `json:"error"`
		}
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &envelope))
		require.Equal(t, openrails.CodePaymentMethodPSPMismatch, envelope.Error.Code)
		require.Empty(t, mock.LastRequest["recurring"], "nothing reaches NMI")
		require.Equal(t, oldPM.ID, *suite.GetSubscription(sub.ID).PaymentMethodID)
	})

	t.Run("a re-attribution committed while the request is in flight is refused", func(t *testing.T) {
		mock.Reset()
		_, err := suite.MerchantPool().Exec(ctx, `UPDATE openrails.payment_methods SET psp_id = $1 WHERE id = $2`, homePSP, newPM.ID)
		require.NoError(t, err)

		// The HTTP pre-check sees a same-PSP method; the durable seam's FOR
		// SHARE then waits behind this flip (a #297 remap's FOR UPDATE) and
		// re-validates against what it committed.
		flip, err := suite.MerchantPool().Begin(ctx)
		require.NoError(t, err)
		defer func() { _ = flip.Rollback(context.Background()) }()
		_, err = flip.Exec(ctx, `SELECT 1 FROM openrails.payment_methods WHERE id = $1 FOR UPDATE`, newPM.ID)
		require.NoError(t, err)

		jsonBody, _ := json.Marshal(map[string]string{"payment_method_id": openrails.PaymentMethodID(newPM.ID).String()})
		w := httptest.NewRecorder()
		req, _ := http.NewRequest("PUT", updateSubscriptionPaymentMethodPath(openrails.SubscriptionID(sub.ID).String()), bytes.NewReader(jsonBody))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)
		served := make(chan struct{})
		go func() {
			defer close(served)
			suite.Server.Handler().ServeHTTP(w, req)
		}()
		require.Eventually(t, func() bool {
			var waiting bool
			require.NoError(t, suite.MerchantPool().QueryRow(ctx,
				`SELECT EXISTS (SELECT 1 FROM pg_stat_activity WHERE datname = current_database() AND wait_event_type = 'Lock' AND query ILIKE '%FOR SHARE%')`).Scan(&waiting))
			return waiting
		}, 10*time.Second, 20*time.Millisecond, "the durable seam must wait on the instrument's row lock")
		_, err = flip.Exec(ctx, `UPDATE openrails.payment_methods SET psp_id = $1 WHERE id = $2`, otherPSP, newPM.ID)
		require.NoError(t, err)
		require.NoError(t, flip.Commit(ctx))
		<-served

		require.Equal(t, http.StatusConflict, w.Code, w.Body.String())
		var envelope struct {
			Error openrails.ErrorDetails `json:"error"`
		}
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &envelope))
		require.Equal(t, openrails.CodePaymentMethodPSPMismatch, envelope.Error.Code)
		require.Empty(t, mock.LastRequest["recurring"], "nothing reaches NMI")
		require.Equal(t, oldPM.ID, *suite.GetSubscription(sub.ID).PaymentMethodID)
	})

	t.Run("merchant Client classifies the refusal", func(t *testing.T) {
		mock.Reset()
		admin := newHostSeamAdminRouter(t, suite, uuid.NewString(), []string{controlplane.PermMerchantSubscriptionsUpdate})
		srv := httptest.NewServer(middleware.ChainHTTP(admin, middleware.ResolveMerchantHTTP(middleware.StaticMerchant(dbtest.TestMerchantID))))
		t.Cleanup(srv.Close)
		client, err := openrails.NewRemote(srv.URL, openrails.WithTokenProvider(func(context.Context) (string, error) { return merchantDelegatedTestToken, nil }))
		require.NoError(t, err)

		err = client.UpdateSubscriptionPaymentMethod(ctx, openrails.SubscriptionID(sub.ID), openrails.UpdateSubscriptionPaymentMethodRequest{PaymentMethodID: openrails.PaymentMethodID(newPM.ID)})
		require.ErrorIs(t, err, openrails.ErrPaymentMethodPSPMismatch)
		require.ErrorIs(t, err, openrails.ErrConflict)
		require.Empty(t, mock.LastRequest["recurring"], "nothing reaches NMI")
		require.Equal(t, oldPM.ID, *suite.GetSubscription(sub.ID).PaymentMethodID)
	})
}

// Zero and blank identifiers are refused with the coded invalid-parameter
// envelope before anything is looked up, on the HTTP route and through the
// Client, and nothing reaches NMI (#657 review).
func TestUpdateSubscriptionPaymentMethodZeroIdentifiers(t *testing.T) {
	suite, mock := SetupSuiteWithMockNMI(t)
	products := suite.SeedProducts()
	userID := uuid.New().String()
	token := suite.MintUserToken(userID, "update-pm-zeroid-"+t.Name()+"@test.example.com")
	pm := suite.CreateTestPaymentMethodWithOptions(PaymentMethodOptions{UserID: userID, Rail: models.RailNMI, VaultID: "vault-" + uuid.NewString()})
	sub := suite.CreateTestSubscriptionWithOptions(SubscriptionOptions{
		UserID: userID, PriceID: products[0].Prices[0].ID, Status: models.StatusActive, Rail: models.RailNMI,
		RailSubID: "sub-zeroid-" + uuid.NewString(), PaymentMethodID: &pm.ID,
	})
	zeroSub := openrails.SubscriptionIDPrefix + uuid.Nil.String()
	zeroMethod := openrails.PaymentMethodIDPrefix + uuid.Nil.String()

	for _, tc := range []struct {
		name         string
		subscription string
		method       string
		// want is the refusal status; an empty path segment never addresses
		// the route at all, so the router answers before any handler runs.
		want int
	}{
		{"zero subscription uuid", zeroSub, openrails.PaymentMethodID(pm.ID).String(), http.StatusBadRequest},
		{"zero payment method uuid", openrails.SubscriptionID(sub.ID).String(), zeroMethod, http.StatusBadRequest},
		{"empty payment method", openrails.SubscriptionID(sub.ID).String(), "", http.StatusBadRequest},
		{"whitespace payment method", openrails.SubscriptionID(sub.ID).String(), "   ", http.StatusBadRequest},
		{"empty subscription", "", openrails.PaymentMethodID(pm.ID).String(), http.StatusTemporaryRedirect},
		{"whitespace subscription", "%20%20", openrails.PaymentMethodID(pm.ID).String(), http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mock.Reset()
			jsonBody, _ := json.Marshal(map[string]string{"payment_method_id": tc.method})
			w := httptest.NewRecorder()
			req, _ := http.NewRequest("PUT", updateSubscriptionPaymentMethodPath(tc.subscription), bytes.NewReader(jsonBody))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer "+token)
			suite.Server.Handler().ServeHTTP(w, req)

			require.Equal(t, tc.want, w.Code, w.Body.String())
			if tc.want == http.StatusBadRequest {
				var envelope struct {
					Error openrails.ErrorDetails `json:"error"`
				}
				require.NoError(t, json.Unmarshal(w.Body.Bytes(), &envelope))
				require.Equal(t, "invalid_param", envelope.Error.Code)
			}
			require.Empty(t, mock.LastRequest["recurring"], "nothing reaches NMI")
			require.Equal(t, pm.ID, *suite.GetSubscription(sub.ID).PaymentMethodID)
		})
	}

	t.Run("client refuses zero identifiers before I/O", func(t *testing.T) {
		mock.Reset()
		client, err := openrails.NewRemote("https://unreachable.invalid", openrails.WithTokenProvider(func(context.Context) (string, error) {
			return merchantDelegatedTestToken, nil
		}))
		require.NoError(t, err)
		err = client.UpdateSubscriptionPaymentMethod(context.Background(), openrails.SubscriptionID{},
			openrails.UpdateSubscriptionPaymentMethodRequest{PaymentMethodID: openrails.PaymentMethodID(pm.ID)})
		require.ErrorIs(t, err, openrails.ErrInvalid)
		err = client.UpdateSubscriptionPaymentMethod(context.Background(), openrails.SubscriptionID(sub.ID),
			openrails.UpdateSubscriptionPaymentMethodRequest{})
		require.ErrorIs(t, err, openrails.ErrInvalid)
		require.Empty(t, mock.LastRequest["recurring"])
	})
}

func updateSubscriptionPaymentMethodPath(subscriptionID string) string {
	return "/v1/me/subscriptions/" + subscriptionID + "/payment-method"
}
