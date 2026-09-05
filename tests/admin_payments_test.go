//go:build integration

package tests

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/merchants"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/controlplane"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/pkg/api"
)

// #528 hard cut: the admin payments surface is the delegated /v1/merchant model.
// Reads require merchant:payments:read; refunds require merchant:payments:refund.
// Tests authenticate as a delegated merchant principal via newHostSeamAdminRouter
// (the retired per-user admin JWT is gone).

// adminPaymentsReader mounts the delegated merchant surface with payments:read.
func adminPaymentsReader(t *testing.T, suite *TestContainerSuite) http.Handler {
	return newHostSeamAdminRouter(t, suite, "bc000000-0000-4000-8000-000000000001",
		[]string{controlplane.PermMerchantPaymentsRead})
}

// adminPaymentsWriter mounts the delegated merchant surface with refund access
// and payments:read, which a refund operator naturally also holds.
func adminPaymentsWriter(t *testing.T, suite *TestContainerSuite) http.Handler {
	return newHostSeamAdminRouter(t, suite, uuid.NewString(),
		[]string{controlplane.PermMerchantPaymentsRead, controlplane.PermMerchantPaymentsRefund})
}

// TestAdminListPayments tests GET /v1/merchant/payments
func TestAdminListPayments(t *testing.T) {
	suite := getSharedTestSuite(t)
	admin := adminPaymentsReader(t, suite)

	// Seed test data
	products := suite.SeedProducts()
	priceID := products[0].Prices[0].ID
	userID := uuid.New().String()

	// Create test subscription and payments
	sub := suite.CreateTestSubscription(userID, priceID, models.StatusActive)
	payment1 := suite.CreateTestPaymentWithOptions(PaymentOptions{
		UserID:         userID,
		PriceID:        priceID,
		SubscriptionID: &sub.ID,
		Rail:           models.RailNMI,
		Amount:         999,
		PurchasedAt:    time.Now().Add(-24 * time.Hour),
	})
	_ = suite.CreateTestPaymentWithOptions(PaymentOptions{
		UserID:         userID,
		PriceID:        priceID,
		SubscriptionID: &sub.ID,
		Rail:           models.RailNMI,
		Amount:         999,
		PurchasedAt:    time.Now(),
	})

	t.Run("returns paginated payments list with Stripe-like format", func(t *testing.T) {
		w := httptest.NewRecorder()
		req, _ := http.NewRequest("GET", "/v1/merchant/payments?limit=10", nil)
		req.Header.Set("Authorization", "Bearer "+merchantDelegatedTestToken)
		admin.ServeHTTP(w, req)

		require.Equal(t, http.StatusOK, w.Code, "Should return 200 OK, got: %s", w.Body.String())

		var response map[string]interface{}
		err := json.Unmarshal(w.Body.Bytes(), &response)
		require.NoError(t, err)

		// Should have data array
		data, ok := response["data"].([]interface{})
		require.True(t, ok, "Should have data array")
		require.GreaterOrEqual(t, len(data), 2, "Should have at least 2 payments")

		// Verify Stripe-like payment format
		payment := data[0].(map[string]interface{})
		assert.True(t, strings.HasPrefix(payment["id"].(string), "pay_"), "ID should have pay_ prefix")
		assert.Equal(t, "charge", payment["object"], "Object should be 'charge'")
		assert.NotNil(t, payment["amount"], "Should have amount")
		assert.NotNil(t, payment["currency"], "Should have currency")
		assert.True(t, strings.HasPrefix(payment["user"].(string), "usr_"), "User should have usr_ prefix")
		assert.NotNil(t, payment["rail"], "Should have rail")
		assert.NotNil(t, payment["transaction_id"], "Should have transaction_id")
		assert.NotNil(t, payment["created"], "Should have created (unix timestamp)")
		assert.NotNil(t, payment["refunded"], "Should have refunded boolean")
		assert.NotNil(t, payment["amount_refunded"], "Should have amount_refunded")

		// Should have pagination fields
		assert.NotNil(t, response["total"], "Should have total")
		assert.NotNil(t, response["limit"], "Should have limit")
		assert.NotNil(t, response["offset"], "Should have offset")
	})

	t.Run("filters by user_id", func(t *testing.T) {
		w := httptest.NewRecorder()
		req, _ := http.NewRequest("GET", fmt.Sprintf("/v1/merchant/payments?user_id=%s", userID), nil)
		req.Header.Set("Authorization", "Bearer "+merchantDelegatedTestToken)
		admin.ServeHTTP(w, req)

		require.Equal(t, http.StatusOK, w.Code)

		var response map[string]interface{}
		err := json.Unmarshal(w.Body.Bytes(), &response)
		require.NoError(t, err)

		data := response["data"].([]interface{})
		require.Len(t, data, 2, "Should return exactly 2 payments for this user")

		// All payments should belong to the user (user field has usr_ prefix)
		expectedUser := api.FormatUserID(userID)
		for _, p := range data {
			payment := p.(map[string]interface{})
			assert.Equal(t, expectedUser, payment["user"], "Payment should belong to filtered user")
		}
	})

	t.Run("filters by rail", func(t *testing.T) {
		w := httptest.NewRecorder()
		req, _ := http.NewRequest("GET", "/v1/merchant/payments?rail=nmi", nil)
		req.Header.Set("Authorization", "Bearer "+merchantDelegatedTestToken)
		admin.ServeHTTP(w, req)

		require.Equal(t, http.StatusOK, w.Code)

		var response map[string]interface{}
		err := json.Unmarshal(w.Body.Bytes(), &response)
		require.NoError(t, err)

		data := response["data"].([]interface{})
		for _, p := range data {
			payment := p.(map[string]interface{})
			assert.Equal(t, "nmi", payment["rail"], "Payment should use nmi rail")
		}
	})

	t.Run("filters by amount range", func(t *testing.T) {
		w := httptest.NewRecorder()
		req, _ := http.NewRequest("GET", "/v1/merchant/payments?min_amount=500&max_amount=1500", nil)
		req.Header.Set("Authorization", "Bearer "+merchantDelegatedTestToken)
		admin.ServeHTTP(w, req)

		require.Equal(t, http.StatusOK, w.Code)

		var response map[string]interface{}
		err := json.Unmarshal(w.Body.Bytes(), &response)
		require.NoError(t, err)

		data := response["data"].([]interface{})
		for _, p := range data {
			payment := p.(map[string]interface{})
			amount := int64(payment["amount"].(float64))
			assert.GreaterOrEqual(t, amount, int64(500), "Amount should be >= min_amount")
			assert.LessOrEqual(t, amount, int64(1500), "Amount should be <= max_amount")
		}
	})

	t.Run("filters refunds only", func(t *testing.T) {
		// Create a refund
		_ = suite.CreateTestPaymentWithOptions(PaymentOptions{
			UserID:            userID,
			PriceID:           priceID,
			RefundedPaymentID: &payment1.ID,
			Rail:              models.RailNMI,
			Amount:            -999, // Negative amount for refund
		})

		w := httptest.NewRecorder()
		req, _ := http.NewRequest("GET", "/v1/merchant/payments?refunds_only=true", nil)
		req.Header.Set("Authorization", "Bearer "+merchantDelegatedTestToken)
		admin.ServeHTTP(w, req)

		require.Equal(t, http.StatusOK, w.Code)

		var response map[string]interface{}
		err := json.Unmarshal(w.Body.Bytes(), &response)
		require.NoError(t, err)

		data := response["data"].([]interface{})
		require.GreaterOrEqual(t, len(data), 1, "Should have at least 1 refund")

		for _, p := range data {
			payment := p.(map[string]interface{})
			// Refunds have negative amounts
			amount := int64(payment["amount"].(float64))
			assert.Less(t, amount, int64(0), "Refund should have negative amount")
		}
	})

	t.Run("sorts by created descending", func(t *testing.T) {
		w := httptest.NewRecorder()
		req, _ := http.NewRequest("GET", fmt.Sprintf("/v1/merchant/payments?user_id=%s&sort_by=created_at&sort_order=desc", userID), nil)
		req.Header.Set("Authorization", "Bearer "+merchantDelegatedTestToken)
		admin.ServeHTTP(w, req)

		require.Equal(t, http.StatusOK, w.Code)

		var response map[string]interface{}
		err := json.Unmarshal(w.Body.Bytes(), &response)
		require.NoError(t, err)

		data := response["data"].([]interface{})
		require.GreaterOrEqual(t, len(data), 2)

		// Verify descending order: each payment's created should be >= the next one
		for i := 0; i < len(data)-1; i++ {
			p1 := data[i].(map[string]interface{})
			p2 := data[i+1].(map[string]interface{})
			t1 := int64(p1["created"].(float64))
			t2 := int64(p2["created"].(float64))
			assert.GreaterOrEqual(t, t1, t2, "Payments should be in descending order by created")
		}
	})

	t.Run("sorts by amount ascending", func(t *testing.T) {
		w := httptest.NewRecorder()
		req, _ := http.NewRequest("GET", "/v1/merchant/payments?sort_by=amount&sort_order=asc&limit=100", nil)
		req.Header.Set("Authorization", "Bearer "+merchantDelegatedTestToken)
		admin.ServeHTTP(w, req)

		require.Equal(t, http.StatusOK, w.Code)

		var response map[string]interface{}
		err := json.Unmarshal(w.Body.Bytes(), &response)
		require.NoError(t, err)

		data := response["data"].([]interface{})
		require.GreaterOrEqual(t, len(data), 2)

		// Verify ascending order by amount
		var prevAmount int64 = -1000000
		for _, p := range data {
			payment := p.(map[string]interface{})
			amount := int64(payment["amount"].(float64))
			assert.GreaterOrEqual(t, amount, prevAmount, "Amounts should be in ascending order")
			prevAmount = amount
		}
	})

	t.Run("filters by subscription_id", func(t *testing.T) {
		w := httptest.NewRecorder()
		req, _ := http.NewRequest("GET", fmt.Sprintf("/v1/merchant/payments?subscription_id=%s", sub.ID.String()), nil)
		req.Header.Set("Authorization", "Bearer "+merchantDelegatedTestToken)
		admin.ServeHTTP(w, req)

		require.Equal(t, http.StatusOK, w.Code)

		var response map[string]interface{}
		err := json.Unmarshal(w.Body.Bytes(), &response)
		require.NoError(t, err)

		data := response["data"].([]interface{})
		require.GreaterOrEqual(t, len(data), 2, "Should have payments for this subscription")

		expectedSubID := api.FormatSubscriptionID(sub.ID)
		for _, p := range data {
			payment := p.(map[string]interface{})
			assert.Equal(t, expectedSubID, payment["subscription"], "Payment should belong to filtered subscription")
		}
	})
}

// TestAdminGetPayment tests GET /v1/merchant/payments/:id
func TestAdminGetPayment(t *testing.T) {
	suite := getSharedTestSuite(t)
	admin := adminPaymentsReader(t, suite)

	// Seed test data
	products := suite.SeedProducts()
	priceID := products[0].Prices[0].ID
	userID := uuid.New().String()

	// Create test subscription and payment
	sub := suite.CreateTestSubscription(userID, priceID, models.StatusActive)
	payment := suite.CreateTestPaymentWithOptions(PaymentOptions{
		UserID:         userID,
		PriceID:        priceID,
		SubscriptionID: &sub.ID,
		Rail:           models.RailNMI,
		Amount:         999,
	})

	t.Run("returns payment with Stripe-like format", func(t *testing.T) {
		w := httptest.NewRecorder()
		req, _ := http.NewRequest("GET", fmt.Sprintf("/v1/merchant/payments/%s", payment.ID.String()), nil)
		req.Header.Set("Authorization", "Bearer "+merchantDelegatedTestToken)
		admin.ServeHTTP(w, req)

		require.Equal(t, http.StatusOK, w.Code, "Should return 200 OK, got: %s", w.Body.String())

		var response map[string]interface{}
		err := json.Unmarshal(w.Body.Bytes(), &response)
		require.NoError(t, err)

		// Verify Stripe-like format
		assert.Equal(t, api.FormatPaymentID(payment.ID), response["id"], "Payment ID should have pay_ prefix")
		assert.Equal(t, "charge", response["object"], "Object should be 'charge'")
		assert.Equal(t, float64(999), response["amount"], "Amount should match")
		assert.Equal(t, "USD", response["currency"], "Currency should match")
		assert.Equal(t, api.FormatUserID(userID), response["user"], "User should have usr_ prefix")
		assert.Equal(t, "nmi", response["rail"], "Rail should match")
		assert.NotNil(t, response["subscription"], "Should include subscription ID")
		assert.Equal(t, false, response["refunded"], "Should not be refunded")
		assert.Equal(t, float64(0), response["amount_refunded"], "Amount refunded should be 0")

		// Should include expanded price
		assert.NotNil(t, response["price"], "Should include price details")
		price := response["price"].(map[string]interface{})
		assert.True(t, strings.HasPrefix(price["id"].(string), "price_"), "Price ID should have prefix")

		// Should have refunds list object (empty since no refunds created yet)
		refunds, ok := response["refunds"].(map[string]interface{})
		require.True(t, ok, "Should have refunds object")
		assert.Equal(t, "list", refunds["object"], "Refunds should be a list object")
		refundData := refunds["data"].([]interface{})
		assert.Empty(t, refundData, "Should have no refunds")
	})

	t.Run("returns payment with refunds and amount_refunded", func(t *testing.T) {
		// Create a refund for the payment
		refund := suite.CreateTestPaymentWithOptions(PaymentOptions{
			UserID:            userID,
			PriceID:           priceID,
			RefundedPaymentID: &payment.ID,
			Rail:              models.RailNMI,
			Amount:            -500, // Partial refund (negative)
			TransactionID:     "refund-txn-" + uuid.New().String()[:8],
		})

		w := httptest.NewRecorder()
		req, _ := http.NewRequest("GET", fmt.Sprintf("/v1/merchant/payments/%s", payment.ID.String()), nil)
		req.Header.Set("Authorization", "Bearer "+merchantDelegatedTestToken)
		admin.ServeHTTP(w, req)

		require.Equal(t, http.StatusOK, w.Code)

		var response map[string]interface{}
		err := json.Unmarshal(w.Body.Bytes(), &response)
		require.NoError(t, err)

		// Should show partial refund status
		assert.Equal(t, false, response["refunded"], "Should not be fully refunded (partial)")
		assert.Equal(t, float64(500), response["amount_refunded"], "Amount refunded should be 500")

		// Should have refunds list with the refund
		refunds := response["refunds"].(map[string]interface{})
		assert.Equal(t, "list", refunds["object"])
		refundData := refunds["data"].([]interface{})
		require.Len(t, refundData, 1, "Should have 1 refund")

		refundObj := refundData[0].(map[string]interface{})
		assert.Equal(t, api.FormatPaymentID(refund.ID), refundObj["id"], "Refund ID should match")
		assert.Equal(t, float64(-500), refundObj["amount"], "Refund amount should be negative")
	})

	t.Run("returns 404 for non-existent payment", func(t *testing.T) {
		nonExistentID := uuid.New()

		w := httptest.NewRecorder()
		req, _ := http.NewRequest("GET", fmt.Sprintf("/v1/merchant/payments/%s", nonExistentID.String()), nil)
		req.Header.Set("Authorization", "Bearer "+merchantDelegatedTestToken)
		admin.ServeHTTP(w, req)

		assert.Equal(t, http.StatusNotFound, w.Code, "Should return 404 Not Found")
	})

	t.Run("returns 400 for invalid payment ID", func(t *testing.T) {
		w := httptest.NewRecorder()
		req, _ := http.NewRequest("GET", "/v1/merchant/payments/not-a-uuid", nil)
		req.Header.Set("Authorization", "Bearer "+merchantDelegatedTestToken)
		admin.ServeHTTP(w, req)

		assert.Equal(t, http.StatusBadRequest, w.Code, "Should return 400 Bad Request")
	})
}

// TestAdminPaymentsTransactionIDFilter tests filtering by transaction_id
func TestAdminPaymentsTransactionIDFilter(t *testing.T) {
	suite := getSharedTestSuite(t)
	admin := adminPaymentsReader(t, suite)

	// Seed test data
	products := suite.SeedProducts()
	priceID := products[0].Prices[0].ID
	userID := uuid.New().String()

	transactionID := "unique-txn-" + uuid.New().String()[:8]
	payment := suite.CreateTestPaymentWithOptions(PaymentOptions{
		UserID:        userID,
		PriceID:       priceID,
		Rail:          models.RailNMI,
		Amount:        999,
		TransactionID: transactionID,
	})

	t.Run("finds payment by transaction_id", func(t *testing.T) {
		w := httptest.NewRecorder()
		req, _ := http.NewRequest("GET", fmt.Sprintf("/v1/merchant/payments?transaction_id=%s", transactionID), nil)
		req.Header.Set("Authorization", "Bearer "+merchantDelegatedTestToken)
		admin.ServeHTTP(w, req)

		require.Equal(t, http.StatusOK, w.Code)

		var response map[string]interface{}
		err := json.Unmarshal(w.Body.Bytes(), &response)
		require.NoError(t, err)

		data := response["data"].([]interface{})
		require.Len(t, data, 1, "Should find exactly 1 payment")

		foundPayment := data[0].(map[string]interface{})
		assert.Equal(t, api.FormatPaymentID(payment.ID), foundPayment["id"], "Should find the correct payment")
		assert.Equal(t, transactionID, foundPayment["transaction_id"], "Transaction ID should match")
	})

	t.Run("returns empty for non-existent transaction_id", func(t *testing.T) {
		w := httptest.NewRecorder()
		req, _ := http.NewRequest("GET", "/v1/merchant/payments?transaction_id=non-existent-txn", nil)
		req.Header.Set("Authorization", "Bearer "+merchantDelegatedTestToken)
		admin.ServeHTTP(w, req)

		require.Equal(t, http.StatusOK, w.Code)

		var response map[string]interface{}
		err := json.Unmarshal(w.Body.Bytes(), &response)
		require.NoError(t, err)

		data := response["data"].([]interface{})
		assert.Empty(t, data, "Should return empty array for non-existent transaction")
	})
}

// TestAdminRefund_RequiresPaymentsWrite proves the delegated gate fails closed:
// a principal holding only billing:read cannot drive a refund (403), even though
// it can read payments.
func TestAdminRefund_RequiresPaymentsWrite(t *testing.T) {
	suite := getSharedTestSuite(t)
	reader := adminPaymentsReader(t, suite)

	products := suite.SeedProducts()
	priceID := products[0].Prices[0].ID
	payment := suite.CreateTestPaymentWithOptions(PaymentOptions{
		UserID:  uuid.New().String(),
		PriceID: priceID,
		Rail:    models.RailNMI,
		Amount:  1000,
	})

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", fmt.Sprintf("/v1/merchant/payments/%s/refunds", payment.ID.String()),
		strings.NewReader(`{"amount": 500}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+merchantDelegatedTestToken)
	reader.ServeHTTP(w, req)

	assert.Equal(t, http.StatusForbidden, w.Code, "billing:read must not authorize a refund")
}

// TestAdminRefundPayment tests POST /v1/merchant/payments/:id/refunds
func TestAdminRefundPayment(t *testing.T) {
	suite := getSharedTestSuite(t)

	// Seed test data
	products := suite.SeedProducts()
	priceID := products[0].Prices[0].ID
	userID := uuid.New().String()

	t.Run("returns 404 for non-existent payment", func(t *testing.T) {
		admin := adminPaymentsWriter(t, suite)
		nonExistentID := uuid.New()

		w := httptest.NewRecorder()
		body := `{"amount": 500}`
		req, _ := http.NewRequest("POST", fmt.Sprintf("/v1/merchant/payments/%s/refunds", nonExistentID.String()), strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+merchantDelegatedTestToken)
		req.Header.Set("Idempotency-Key", "refund-missing-payment-"+uuid.NewString())
		admin.ServeHTTP(w, req)

		assert.Equal(t, http.StatusNotFound, w.Code, "Should return 404 for non-existent payment")
	})

	t.Run("returns 400 for invalid payment ID", func(t *testing.T) {
		admin := adminPaymentsWriter(t, suite)
		w := httptest.NewRecorder()
		body := `{"amount": 500}`
		req, _ := http.NewRequest("POST", "/v1/merchant/payments/not-a-uuid/refunds", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+merchantDelegatedTestToken)
		admin.ServeHTTP(w, req)

		assert.Equal(t, http.StatusBadRequest, w.Code, "Should return 400 for invalid payment ID")
	})

	t.Run("returns 400 for missing amount", func(t *testing.T) {
		admin := adminPaymentsWriter(t, suite)
		payment := suite.CreateTestPaymentWithOptions(PaymentOptions{
			UserID:  userID,
			PriceID: priceID,
			Rail:    models.RailNMI,
			Amount:  1000,
		})

		w := httptest.NewRecorder()
		body := `{}`
		req, _ := http.NewRequest("POST", fmt.Sprintf("/v1/merchant/payments/%s/refunds", payment.ID.String()), strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+merchantDelegatedTestToken)
		req.Header.Set("Idempotency-Key", "refund-missing-amount-"+uuid.NewString())
		admin.ServeHTTP(w, req)

		assert.Equal(t, http.StatusBadRequest, w.Code, "Should return 400 for missing amount")
	})

	t.Run("returns 400 for zero amount", func(t *testing.T) {
		admin := adminPaymentsWriter(t, suite)
		payment := suite.CreateTestPaymentWithOptions(PaymentOptions{
			UserID:  userID,
			PriceID: priceID,
			Rail:    models.RailNMI,
			Amount:  1000,
		})

		w := httptest.NewRecorder()
		body := `{"amount": 0}`
		req, _ := http.NewRequest("POST", fmt.Sprintf("/v1/merchant/payments/%s/refunds", payment.ID.String()), strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+merchantDelegatedTestToken)
		req.Header.Set("Idempotency-Key", "refund-zero-amount-"+uuid.NewString())
		admin.ServeHTTP(w, req)

		assert.Equal(t, http.StatusBadRequest, w.Code, "Should return 400 for zero amount")
	})

	t.Run("returns 400 for sub-cent refund micros", func(t *testing.T) {
		admin := adminPaymentsWriter(t, suite)
		// #671: RefundPayload.AmountCents is true cents; a refund amount with a
		// sub-cent micros remainder is rejected, never rounded.
		payment := suite.CreateTestPaymentWithOptions(PaymentOptions{
			UserID:  userID,
			PriceID: priceID,
			Rail:    models.RailNMI,
			Amount:  10_000_000,
		})

		w := httptest.NewRecorder()
		body := `{"amount": 5000}` // 5,000 micros = $0.005: not a whole cent
		req, _ := http.NewRequest("POST", fmt.Sprintf("/v1/merchant/payments/%s/refunds", payment.ID.String()), strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+merchantDelegatedTestToken)
		req.Header.Set("Idempotency-Key", "refund-subcent-"+uuid.NewString())
		admin.ServeHTTP(w, req)

		require.Equal(t, http.StatusBadRequest, w.Code, "sub-cent refund micros must be a 400")

		var response map[string]interface{}
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &response))
		errorObj := response["error"].(map[string]interface{})
		message := errorObj["message"].(string)
		assert.Contains(t, message, "whole number of cents", "error must explain the sub-cent rejection")
	})

	t.Run("returns actionable 400 for stripe historical non-refundable id", func(t *testing.T) {
		admin := adminPaymentsWriter(t, suite)
		payment := suite.CreateTestPaymentWithOptions(PaymentOptions{
			UserID:        userID,
			PriceID:       priceID,
			Rail:          models.RailStripe,
			TransactionID: "cs_test_old_checkout_" + uuid.NewString()[:8],
			Amount:        10_000_000,
		})

		w := httptest.NewRecorder()
		body := `{"amount": 5000000}` // whole-cent micros ($5) so the rail branch is reached
		req, _ := http.NewRequest("POST", fmt.Sprintf("/v1/merchant/payments/%s/refunds", payment.ID.String()), strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+merchantDelegatedTestToken)
		req.Header.Set("Idempotency-Key", "refund-stripe-old-"+uuid.NewString())
		admin.ServeHTTP(w, req)

		assert.Equal(t, http.StatusBadRequest, w.Code, "Should return actionable 400 for unsupported Stripe transaction ID")

		var response map[string]interface{}
		err := json.Unmarshal(w.Body.Bytes(), &response)
		require.NoError(t, err)

		errorObj := response["error"].(map[string]interface{})
		message := errorObj["message"].(string)
		assert.Contains(t, message, "charge/payment_intent")
	})

	t.Run("returns 400 for CCBill payments without a linked subscription", func(t *testing.T) {
		admin := adminPaymentsWriter(t, suite)
		// Missing subscription linkage does not change the unsupported rail
		// capability or encourage fixing coordinates to enable an unsafe refund.
		payment := suite.CreateTestPaymentWithOptions(PaymentOptions{
			UserID:  userID,
			PriceID: priceID,
			Rail:    models.RailCCBill,
			Amount:  10_000_000,
		})

		w := httptest.NewRecorder()
		body := `{"amount": 5000000}` // whole-cent micros so the rail branch is reached
		req, _ := http.NewRequest("POST", fmt.Sprintf("/v1/merchant/payments/%s/refunds", payment.ID.String()), strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+merchantDelegatedTestToken)
		req.Header.Set("Idempotency-Key", "refund-ccbill-"+uuid.NewString())
		admin.ServeHTTP(w, req)

		assert.Equal(t, http.StatusBadRequest, w.Code, "Should return 400 for a CCBill payment with no subscription link")

		var response map[string]interface{}
		err := json.Unmarshal(w.Body.Bytes(), &response)
		require.NoError(t, err)

		errorObj := response["error"].(map[string]interface{})
		message := errorObj["message"].(string)
		assert.Contains(t, message, "CCBill", "Error should mention CCBill")
		assert.Contains(t, message, "automatic CCBill refunds are unavailable")
		assert.Contains(t, message, "admin portal", "Error should direct to the manual fallback")
	})
}

// TestAdminRefundReachesAnyUserInMerchant confirms a merchant admin holding
// payments:write can drive a refund against ANY user's payment within its
// merchant — the operator is merchant-scoped, not user-scoped. (The unlinked
// CCBill payments draw the no-subscription-link rail 400, which proves the
// request reached the handler, not an auth gate.)
func TestAdminRefundReachesAnyUserInMerchant(t *testing.T) {
	suite := getSharedTestSuite(t)
	admin := adminPaymentsWriter(t, suite)

	products := suite.SeedProducts()
	priceID := products[0].Prices[0].ID

	paymentA := suite.CreateTestPaymentWithOptions(PaymentOptions{
		UserID:  uuid.New().String(),
		PriceID: priceID,
		Rail:    models.RailCCBill, // CCBill so we don't need a rail mock
		Amount:  20_000_000,
	})
	paymentB := suite.CreateTestPaymentWithOptions(PaymentOptions{
		UserID:  uuid.New().String(),
		PriceID: priceID,
		Rail:    models.RailCCBill,
		Amount:  30_000_000,
	})

	for _, payment := range []*models.Payment{paymentA, paymentB} {
		w := httptest.NewRecorder()
		body := `{"amount": 5000000}` // whole-cent micros so the rail branch is reached
		req, _ := http.NewRequest("POST", fmt.Sprintf("/v1/merchant/payments/%s/refunds", payment.ID.String()), strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+merchantDelegatedTestToken)
		req.Header.Set("Idempotency-Key", "refund-boundary-"+uuid.NewString())
		admin.ServeHTTP(w, req)

		// Should get the CCBill rail error, not an auth error.
		assert.Equal(t, http.StatusBadRequest, w.Code, "Admin should be able to reach payment")

		var response map[string]interface{}
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &response))
		errorObj := response["error"].(map[string]interface{})
		message := errorObj["message"].(string)
		assert.Contains(t, message, "CCBill", "Should get CCBill error, not auth error")
	}
}

// TestAdminRefundPaymentThroughIntentLedger drives the full HTTP admin refund
// path over the provider intent ledger (#358 phase B): the provider-side
// money movement is a durable nmi_refund intent executed synchronously, the
// caller operation identity makes replays return the recorded refund while
// distinct keys allow intentional equal-sized partial refunds.
func TestAdminRefundPaymentThroughIntentLedger(t *testing.T) {
	suite := getSharedTestSuite(t)
	admin := adminPaymentsWriter(t, suite)
	products := suite.SeedProducts()
	priceID := products[0].Prices[0].ID
	userID := uuid.New().String()

	payment := suite.CreateTestPaymentWithOptions(PaymentOptions{
		UserID:        userID,
		PriceID:       priceID,
		Rail:          models.RailNMI,
		TransactionID: "txn-ledger-" + uuid.NewString()[:8],
		Amount:        10_000_000, // $10 in micros
	})

	// Fake NMI gateway approving refunds (v5); records the wire body so the
	// micros→cents conversion (#671) is pinned to the exact provider amount.
	var refundCalls atomic.Int64
	var refundBody atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/refund") {
			call := refundCalls.Add(1)
			b, _ := io.ReadAll(r.Body)
			refundBody.Store(string(b))
			fmt.Fprintf(w, `{"object":"transaction","id":"txn_refund_http_%d","response":"1","response_text":"SUCCESS"}`, call)
			return
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)
	// #788: NMI clients arm per charge from the armed rail state; point them
	// at the fake gateway via the endpoint override.
	suite.SetNMIGateway(srv.URL)
	t.Cleanup(func() { suite.SetNMIGateway("") })

	refundReq := func(idempotencyKey string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		req, _ := http.NewRequest("POST", fmt.Sprintf("/v1/merchant/payments/%s/refunds", payment.ID.String()),
			strings.NewReader(`{"amount": 4000000}`)) // $4 in micros
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+merchantDelegatedTestToken)
		req.Header.Set("Idempotency-Key", idempotencyKey)
		admin.ServeHTTP(w, req)
		return w
	}

	w := refundReq("ledger-key-1")
	require.Equal(t, http.StatusCreated, w.Code, "synchronous ledger execution completes inline: %s", w.Body.String())
	assert.EqualValues(t, 1, refundCalls.Load())
	// #671: 4,000,000 micros crosses the boundary as exactly 400 cents.
	assert.Contains(t, refundBody.Load().(string), `"amount":4.00`, "provider must see the exact cents amount")

	// The durable intent records the execution.
	var intentStatus string
	require.NoError(t, suite.MerchantPool().QueryRow(context.Background(),
		"SELECT status FROM openrails.rail_intents WHERE intent_type = 'nmi_refund' AND payment_id = $1",
		payment.ID).Scan(&intentStatus))
	assert.Equal(t, "succeeded", intentStatus)

	// Replay with the SAME admin idempotency key returns the recorded refund.
	w = refundReq("ledger-key-1")
	assert.Equal(t, http.StatusCreated, w.Code, "admin-key replay returns the recorded refund: %s", w.Body.String())
	assert.EqualValues(t, 1, refundCalls.Load(), "replay never re-refunds")

	// A fresh caller key authorizes another partial refund within the balance.
	w = refundReq("ledger-key-2")
	assert.Equal(t, http.StatusCreated, w.Code, "second partial refund must complete: %s", w.Body.String())
	assert.EqualValues(t, 2, refundCalls.Load(), "two intentional partial refunds")
}

// seedCCBillPaymentWithSubscription creates a settled charge linked to an active
// subscription, proving valid coordinates do not enable automatic CCBill refunds.
func seedCCBillPaymentWithSubscription(suite *TestContainerSuite, priceID uuid.UUID) *models.Payment {
	userID := uuid.New().String()
	sub := suite.CreateTestSubscriptionWithOptions(SubscriptionOptions{
		UserID: userID, PriceID: priceID, Status: models.StatusActive,
		Rail: models.RailCCBill, RailSubID: "ccsub-" + uuid.NewString()[:8],
	})
	return suite.CreateTestPaymentWithOptions(PaymentOptions{
		UserID: userID, PriceID: priceID, SubscriptionID: &sub.ID,
		Rail: models.RailCCBill, TransactionID: "cctxn-" + uuid.NewString()[:8],
		Amount: 10_000_000,
	})
}

func ccbillAdminRefundReq(t *testing.T, admin http.Handler, paymentID uuid.UUID, idempotencyKey string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", fmt.Sprintf("/v1/merchant/payments/%s/refunds", paymentID.String()),
		strings.NewReader(`{"amount": 5000000}`)) // $5 in micros
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+merchantDelegatedTestToken)
	req.Header.Set("Idempotency-Key", idempotencyKey)
	admin.ServeHTTP(w, req)
	return w
}

// Configured credentials and a valid linked charge still cannot enable an
// unsupported refund. Repeated caller keys must not create an operation either.
func TestAdminRefundCCBillRefusedBeforeDataLink(t *testing.T) {
	suite := getSharedTestSuite(t)
	admin := adminPaymentsWriter(t, suite)
	products := suite.SeedProducts()
	payment := seedCCBillPaymentWithSubscription(suite, products[0].Prices[0].ID)

	var providerCalls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		providerCalls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	suite.App.Runtime.CCBillDataLinkEndpoint = srv.URL
	t.Cleanup(func() { suite.App.Runtime.CCBillDataLinkEndpoint = "" })

	for _, key := range []string{"ccbill-key-1", "ccbill-key-1", "ccbill-key-2"} {
		w := ccbillAdminRefundReq(t, admin, payment.ID, key)
		require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
		require.Contains(t, w.Body.String(), "automatic CCBill refunds are unavailable")
	}
	require.Zero(t, providerCalls.Load(), "neither refund nor cancel nor provider read may be sent")
	var count int
	require.NoError(t, suite.MerchantPool().QueryRow(context.Background(),
		"SELECT count(*) FROM openrails.rail_intents WHERE payment_id=$1", payment.ID).Scan(&count))
	require.Zero(t, count)
	require.NoError(t, suite.MerchantPool().QueryRow(context.Background(),
		"SELECT count(*) FROM openrails.payments WHERE refunded_payment_id=$1", payment.ID).Scan(&count))
	require.Zero(t, count)
	var status string
	require.NoError(t, suite.MerchantPool().QueryRow(context.Background(),
		"SELECT status FROM openrails.subscriptions WHERE id=$1", payment.SubscriptionID).Scan(&status))
	require.Equal(t, "active", status)
}

// Removing credentials must not turn an unsupported refund into a queued
// operation waiting for configuration repair.
func TestAdminRefundCCBillRefusedWhenDataLinkUnconfigured(t *testing.T) {
	suite := getSharedTestSuite(t)
	admin := adminPaymentsWriter(t, suite)
	products := suite.SeedProducts()
	payment := seedCCBillPaymentWithSubscription(suite, products[0].Prices[0].ID)

	// #788: "unconfigured" = the merchant's armed ccbill account carries no
	// DataLink credentials. Remove them for the duration of the test.
	ctxBg := dbtest.WithTestMerchant(context.Background())
	env := config.ExpectedProviderEnvironment(suite.Config.IsTestMode())
	store := suite.App.Runtime.Merchants.Secrets()
	for _, key := range []string{"datalink_username", "datalink_password"} {
		name, err := merchants.PSPSecretName("ccbill", env, "945280-0000", key)
		require.NoError(t, err)
		require.NoError(t, store.Delete(ctxBg, dbtest.TestMerchantID, name))
	}
	t.Cleanup(func() {
		for key, value := range map[string]string{"datalink_username": "dl-user", "datalink_password": "dl-pass"} {
			name, err := merchants.PSPSecretName("ccbill", env, "945280-0000", key)
			require.NoError(t, err)
			_, err = store.Put(ctxBg, dbtest.TestMerchantID, name, value)
			require.NoError(t, err)
		}
	})

	w := ccbillAdminRefundReq(t, admin, payment.ID, "ccbill-unconfigured-key")
	require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	require.Contains(t, w.Body.String(), "automatic CCBill refunds are unavailable")
	var count int
	require.NoError(t, suite.MerchantPool().QueryRow(context.Background(),
		"SELECT count(*) FROM openrails.rail_intents WHERE payment_id=$1", payment.ID).Scan(&count))
	require.Zero(t, count)
	require.NoError(t, suite.MerchantPool().QueryRow(context.Background(),
		"SELECT count(*) FROM openrails.payments WHERE refunded_payment_id=$1", payment.ID).Scan(&count))
	require.Zero(t, count)
}

// TestAdminPaymentsListValidatesPagination pins the #785 fix: a negative limit
// or offset returns 400 (mirroring ListAdminCustomers), instead of a 200 with
// an inconsistent {"limit":-1,"has_more":true,…} envelope. A valid request
// still succeeds.
func TestAdminPaymentsListValidatesPagination(t *testing.T) {
	suite := getSharedTestSuite(t)
	reader := adminPaymentsReader(t, suite)

	do := func(query string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		req, _ := http.NewRequest("GET", "/v1/merchant/payments"+query, nil)
		req.Header.Set("Authorization", "Bearer "+merchantDelegatedTestToken)
		reader.ServeHTTP(w, req)
		return w
	}

	assert.Equal(t, http.StatusBadRequest, do("?limit=-1").Code, "negative limit must 400")
	assert.Equal(t, http.StatusBadRequest, do("?offset=-1").Code, "negative offset must 400")
	assert.Equal(t, http.StatusOK, do("").Code, "a valid list request still succeeds")
}
