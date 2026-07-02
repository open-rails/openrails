//go:build integration

package tests

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/pkg/api"
)

type listResponse[T any] struct {
	Object  string `json:"object"`
	Data    []T    `json:"data"`
	Total   int64  `json:"total"`
	Limit   int    `json:"limit"`
	Offset  int    `json:"offset"`
	HasMore bool   `json:"has_more"`
}

// TestGetProductsEndpoint tests the public products endpoint returns seeded products
func TestGetProductsEndpoint(t *testing.T) {
	suite := getSharedTestSuite(t)

	// Seed products
	testProducts := suite.SeedProducts()
	require.Len(t, testProducts, 4, "Should have seeded 4 test products")

	t.Run("returns seeded products", func(t *testing.T) {
		w := httptest.NewRecorder()
		req, _ := http.NewRequest("GET", "/v1/products?limit=100", nil)

		suite.Server.Handler().ServeHTTP(w, req)

		require.Equal(t, http.StatusOK, w.Code, "Should return 200 OK")

		// Parse list response with pagination
		var resp listResponse[api.ProductObject]
		err := json.Unmarshal(w.Body.Bytes(), &resp)
		require.NoError(t, err, "Should parse response JSON")

		// Verify list envelope
		assert.Equal(t, "list", resp.Object, "Should have object: list")
		assert.GreaterOrEqual(t, resp.Total, int64(2), "Should have at least 2 total items")

		// Verify products returned (at least the seeded ones)
		require.GreaterOrEqual(t, len(resp.Data), 4, "Should return at least 4 products")

		// Find Premium product and verify the monthly USD price.
		var premiumProduct *api.ProductObject
		for i, p := range resp.Data {
			if p.Key == "premium" {
				premiumProduct = &resp.Data[i]
				break
			}
		}

		require.NotNil(t, premiumProduct, "Should find Premium product; got products: %v", productNames(resp.Data))
		assert.Equal(t, "product", premiumProduct.Object)
		assert.NotEmpty(t, premiumProduct.Key)
		assert.True(t, premiumProduct.Active)
		require.GreaterOrEqual(t, len(premiumProduct.Prices), 2, "Should have monthly and yearly USD prices")
		// Intervals are hours since the hours-internally cut (c712299a): 720h = 30d.
		monthlyPrice := findPriceByAmountCurrencyInterval(premiumProduct.Prices, 9_990_000, "usd", "720h")
		require.NotNil(t, monthlyPrice, "Should find Premium 9990000 micros/720h USD price")
		assert.Equal(t, "price", monthlyPrice.Object)
	})

	t.Run("returns products with correct price details", func(t *testing.T) {
		w := httptest.NewRecorder()
		req, _ := http.NewRequest("GET", "/v1/products?limit=100", nil)

		suite.Server.Handler().ServeHTTP(w, req)

		require.Equal(t, http.StatusOK, w.Code)

		var resp listResponse[api.ProductObject]
		err := json.Unmarshal(w.Body.Bytes(), &resp)
		require.NoError(t, err)

		// Find Premium product and verify yearly USD pricing.
		var premiumProduct *api.ProductObject
		for i, p := range resp.Data {
			if p.Key == "premium" {
				premiumProduct = &resp.Data[i]
				break
			}
		}

		require.NotNil(t, premiumProduct, "Should find Premium product; got products: %v", productNames(resp.Data))
		yearlyPrice := findPriceByAmountCurrencyInterval(premiumProduct.Prices, 79_990_000, "usd", "8760h")
		require.NotNil(t, yearlyPrice, "Should find Premium 79990000 micros/8760h USD price")
	})
}

func findPriceByAmountCurrencyInterval(prices []api.PriceObject, amount int64, currency, interval string) *api.PriceObject {
	for i := range prices {
		price := &prices[i]
		if price.UnitAmount != amount || price.Currency != currency || price.Recurring == nil {
			continue
		}
		if price.Recurring.Interval == interval {
			return price
		}
	}
	return nil
}

func productNames(products []api.ProductObject) []string {
	names := make([]string, 0, len(products))
	for _, p := range products {
		names = append(names, p.Name)
	}
	return names
}

// TestGetActiveSubscriptionEndpoint tests retrieving the current user's subscription
func TestGetActiveSubscriptionEndpoint(t *testing.T) {
	suite, token, userID := setupTestSuiteWithAuth(t)

	// Seed products first
	testProducts := suite.SeedProducts()
	priceID := testProducts[0].Prices[0].ID

	t.Run("returns no subscription for new user", func(t *testing.T) {
		w := httptest.NewRecorder()
		req, _ := http.NewRequest("GET", "/v1/me/subscriptions?status=active", nil)
		req.Header.Set("Authorization", "Bearer "+token)

		suite.Server.Handler().ServeHTTP(w, req)

		// User without subscription should get 200 with empty list
		assert.Equal(t, http.StatusOK, w.Code)

		var resp listResponse[any]
		err := json.Unmarshal(w.Body.Bytes(), &resp)
		require.NoError(t, err)

		assert.Equal(t, "list", resp.Object, "Should have object: list")
		assert.Empty(t, resp.Data, "Should have no active subscriptions for new user")
	})

	t.Run("returns active subscription details", func(t *testing.T) {
		// Create active subscription for user
		sub := suite.CreateTestSubscription(userID, priceID, models.StatusActive)

		w := httptest.NewRecorder()
		req, _ := http.NewRequest("GET", "/v1/me/subscriptions?status=active", nil)
		req.Header.Set("Authorization", "Bearer "+token)

		suite.Server.Handler().ServeHTTP(w, req)

		require.Equal(t, http.StatusOK, w.Code)

		// Parse list response
		var resp listResponse[json.RawMessage]
		err := json.Unmarshal(w.Body.Bytes(), &resp)
		require.NoError(t, err)

		assert.Equal(t, "list", resp.Object, "Should have object: list")
		require.Len(t, resp.Data, 1, "Should have 1 active subscription")

		// Extract subscription data
		var subscriptions []map[string]any
		dataBytes, err := json.Marshal(resp.Data)
		require.NoError(t, err)
		err = json.Unmarshal(dataBytes, &subscriptions)
		require.NoError(t, err)

		// Verify subscription data
		assert.Equal(t, sub.ID.String(), subscriptions[0]["id"])
		assert.Equal(t, string(models.StatusActive), subscriptions[0]["status"])
		price, ok := subscriptions[0]["price"].(map[string]any)
		require.True(t, ok, "Should include price details")
		assert.Equal(t, float64(9_990_000), price["unit_amount"], "unit_amount should be 9990000 micros")
		assert.NotContains(t, price, "amount", "public subscription price should not expose amount")
	})

	t.Run("requires authentication", func(t *testing.T) {
		w := httptest.NewRecorder()
		req, _ := http.NewRequest("GET", "/v1/me/subscriptions", nil)
		// No auth header

		suite.Server.Handler().ServeHTTP(w, req)

		assert.Equal(t, http.StatusUnauthorized, w.Code)
	})
}

// TestGetSubscriptionHistoryEndpoint tests retrieving subscription history
func TestGetSubscriptionHistoryEndpoint(t *testing.T) {
	suite, token, userID := setupTestSuiteWithAuth(t)

	// Seed products
	testProducts := suite.SeedProducts()
	monthlyPriceID := testProducts[0].Prices[0].ID
	yearlyPriceID := testProducts[1].Prices[0].ID

	t.Run("returns empty history for new user", func(t *testing.T) {
		w := httptest.NewRecorder()
		req, _ := http.NewRequest("GET", "/v1/me/subscriptions?status=all", nil)
		req.Header.Set("Authorization", "Bearer "+token)

		suite.Server.Handler().ServeHTTP(w, req)

		assert.Equal(t, http.StatusOK, w.Code)

		var resp listResponse[any]
		err := json.Unmarshal(w.Body.Bytes(), &resp)
		require.NoError(t, err)

		assert.Equal(t, "list", resp.Object, "Should have object: list")
		assert.Empty(t, resp.Data, "Should have no subscriptions for new user")
	})

	t.Run("returns subscription history with multiple subscriptions", func(t *testing.T) {
		// Create cancelled subscription
		cancelledSub := suite.CreateTestSubscriptionWithOptions(SubscriptionOptions{
			UserID:  userID,
			PriceID: monthlyPriceID,
			Status:  models.StatusCancelled,
		})

		// Create active subscription
		activeSub := suite.CreateTestSubscription(userID, yearlyPriceID, models.StatusActive)

		w := httptest.NewRecorder()
		req, _ := http.NewRequest("GET", "/v1/me/subscriptions?status=all", nil)
		req.Header.Set("Authorization", "Bearer "+token)

		suite.Server.Handler().ServeHTTP(w, req)

		require.Equal(t, http.StatusOK, w.Code)

		var resp listResponse[map[string]any]
		err := json.Unmarshal(w.Body.Bytes(), &resp)
		require.NoError(t, err)

		assert.Equal(t, "list", resp.Object, "Should have object: list")
		require.Len(t, resp.Data, 2, "Should have 2 subscriptions in history")

		// Verify we have both active and cancelled subscriptions
		var hasActive, hasCancelled bool
		for _, sub := range resp.Data {
			status := sub["status"].(string)
			if status == string(models.StatusActive) {
				hasActive = true
				assert.Equal(t, activeSub.ID.String(), sub["id"])
			}
			if status == string(models.StatusCancelled) {
				hasCancelled = true
				assert.Equal(t, cancelledSub.ID.String(), sub["id"])
			}
		}
		assert.True(t, hasActive, "Should have active subscription")
		assert.True(t, hasCancelled, "Should have cancelled subscription")
	})
}

// TestGetUserPaymentsEndpoint tests retrieving payment history
func TestGetUserPaymentsEndpoint(t *testing.T) {
	suite, token, userID := setupTestSuiteWithAuth(t)

	// Seed products
	testProducts := suite.SeedProducts()
	priceID := testProducts[0].Prices[0].ID

	t.Run("returns empty payments for new user", func(t *testing.T) {
		w := httptest.NewRecorder()
		req, _ := http.NewRequest("GET", "/v1/me/payments", nil)
		req.Header.Set("Authorization", "Bearer "+token)

		suite.Server.Handler().ServeHTTP(w, req)

		assert.Equal(t, http.StatusOK, w.Code)

		var resp listResponse[any]
		err := json.Unmarshal(w.Body.Bytes(), &resp)
		require.NoError(t, err)

		assert.Equal(t, "list", resp.Object, "Should have object: list")
		assert.Empty(t, resp.Data, "Should have no payments for new user")
	})

	t.Run("returns payment history", func(t *testing.T) {
		// Create subscription and payments
		sub := suite.CreateTestSubscription(userID, priceID, models.StatusActive)
		payment1 := suite.CreateTestPayment(userID, priceID, &sub.ID)
		payment2 := suite.CreateTestPayment(userID, priceID, &sub.ID)

		w := httptest.NewRecorder()
		req, _ := http.NewRequest("GET", "/v1/me/payments", nil)
		req.Header.Set("Authorization", "Bearer "+token)

		suite.Server.Handler().ServeHTTP(w, req)

		require.Equal(t, http.StatusOK, w.Code)

		var resp listResponse[map[string]any]
		err := json.Unmarshal(w.Body.Bytes(), &resp)
		require.NoError(t, err)

		assert.Equal(t, "list", resp.Object, "Should have object: list")
		require.Len(t, resp.Data, 2, "Should have 2 payments")

		// Verify payment details
		paymentIDs := make(map[string]bool)
		for _, p := range resp.Data {
			paymentIDs[p["id"].(string)] = true
			// JSON unmarshals numbers as float64, but we compare against int64 value
			assert.Equal(t, float64(9_990_000), p["amount"], "Amount should be 9990000 micros")
			assert.Equal(t, "usd", p["currency"])
		}
		assert.True(t, paymentIDs[api.FormatPaymentID(payment1.ID)], "Should include payment 1")
		assert.True(t, paymentIDs[api.FormatPaymentID(payment2.ID)], "Should include payment 2")
	})
}

// TestGetMyBillingStatusEndpoint tests the user's billing status
func TestGetMyBillingStatusEndpoint(t *testing.T) {
	suite, token, userID := setupTestSuiteWithAuth(t)

	// Seed products
	testProducts := suite.SeedProducts()
	priceID := testProducts[0].Prices[0].ID

	t.Run("returns non-premium status for user without subscription", func(t *testing.T) {
		w := httptest.NewRecorder()
		req, _ := http.NewRequest("GET", "/v1/me/status", nil)
		req.Header.Set("Authorization", "Bearer "+token)

		suite.Server.Handler().ServeHTTP(w, req)

		require.Equal(t, http.StatusOK, w.Code)

		var response map[string]any
		err := json.Unmarshal(w.Body.Bytes(), &response)
		require.NoError(t, err)

		assert.Nil(t, response["subscription"], "Should have no subscription")
		assert.Nil(t, response["next_renewal_at"], "Should have no renewal date")

		assert.Empty(t, response["entitlements"], "Should have no entitlements")
	})

	t.Run("returns premium status for user with active subscription", func(t *testing.T) {
		// Create active subscription
		sub := suite.CreateTestSubscription(userID, priceID, models.StatusActive)

		// Create entitlement
		suite.CreateTestEntitlement(userID, "premium", &sub.ID, models.EntitlementSourceSubscription)

		w := httptest.NewRecorder()
		req, _ := http.NewRequest("GET", "/v1/me/status", nil)
		req.Header.Set("Authorization", "Bearer "+token)

		suite.Server.Handler().ServeHTTP(w, req)

		require.Equal(t, http.StatusOK, w.Code)

		var response map[string]any
		err := json.Unmarshal(w.Body.Bytes(), &response)
		require.NoError(t, err)

		assert.NotNil(t, response["subscription"], "Should have active subscription")
		assert.NotNil(t, response["next_renewal_at"], "Should have renewal date")
		assert.NotNil(t, response["entitlements"], "Should have entitlements")
	})

	t.Run("requires authentication", func(t *testing.T) {
		w := httptest.NewRecorder()
		req, _ := http.NewRequest("GET", "/v1/me/status", nil)
		// No auth header

		suite.Server.Handler().ServeHTTP(w, req)

		assert.Equal(t, http.StatusUnauthorized, w.Code)
	})
}
