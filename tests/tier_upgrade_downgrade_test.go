//go:build integration

package tests

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/controlplane"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/checkout"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/pkg/api"
)

// TestTierGroupDetection tests that the checkout service correctly detects tier groups
func TestTierGroupDetection(t *testing.T) {
	suite := getSharedTestSuite(t)
	ctx := suite.MerchantCtx()

	// Seed tiered products (Premium, Premium+, Premium Ultimate)
	tieredProducts := suite.SeedTieredProducts()
	require.Len(t, tieredProducts, 3, "Should have 3 tiered products")

	premiumProduct := tieredProducts[0].Product
	premiumPlusProduct := tieredProducts[1].Product
	premiumUltimateProduct := tieredProducts[2].Product

	premiumPriceID := tieredProducts[0].Prices[0].ID
	premiumPlusPriceID := tieredProducts[1].Prices[0].ID

	userID := uuid.New().String()

	t.Run("identifies products in same tier group", func(t *testing.T) {
		// All three should have the same tier group
		assert.NotNil(t, premiumProduct.TierGroup, "Premium should have tier group")
		assert.NotNil(t, premiumPlusProduct.TierGroup, "Premium+ should have tier group")
		assert.NotNil(t, premiumUltimateProduct.TierGroup, "Premium Ultimate should have tier group")

		assert.Equal(t, *premiumProduct.TierGroup, *premiumPlusProduct.TierGroup, "Premium and Premium+ should be in same tier group")
		assert.Equal(t, *premiumProduct.TierGroup, *premiumUltimateProduct.TierGroup, "Premium and Premium Ultimate should be in same tier group")
	})

	t.Run("identifies tier rank order", func(t *testing.T) {
		// Premium (1) < Premium+ (2) < Premium Ultimate (3)
		assert.Equal(t, 1, premiumProduct.TierRank, "Premium should have rank 1")
		assert.Equal(t, 2, premiumPlusProduct.TierRank, "Premium+ should have rank 2")
		assert.Equal(t, 3, premiumUltimateProduct.TierRank, "Premium Ultimate should have rank 3")

		assert.Less(t, premiumProduct.TierRank, premiumPlusProduct.TierRank, "Premium rank should be less than Premium+")
		assert.Less(t, premiumPlusProduct.TierRank, premiumUltimateProduct.TierRank, "Premium+ rank should be less than Premium Ultimate")
	})

	t.Run("detects upgrade scenario", func(t *testing.T) {
		// Create subscription on Premium
		sub := suite.CreateTestSubscriptionWithOptions(SubscriptionOptions{
			UserID:  userID,
			PriceID: premiumPriceID,
			Status:  models.StatusActive,
			Rail:    models.RailNMI,
		})
		defer suite.CleanupSubscriptionsForUser(userID)

		// Try to purchase Premium+ - should detect as upgrade
		checkoutService := suite.App.Runtime.CheckoutService

		eligibility, err := checkoutService.CheckPurchaseEligibility(ctx, userID, premiumPlusPriceID)
		require.NoError(t, err, "Should check eligibility without error")
		assert.Equal(t, checkout.EligibilityUpgrade, eligibility.Status, "Should detect upgrade scenario")
		assert.NotNil(t, eligibility.ExistingSubscription, "Should have existing subscription")
		assert.Equal(t, sub.ID.String(), eligibility.ExistingSubscription.ID.String())
	})

	t.Run("detects downgrade scenario", func(t *testing.T) {
		// Create subscription on Premium+
		sub := suite.CreateTestSubscriptionWithOptions(SubscriptionOptions{
			UserID:  userID,
			PriceID: premiumPlusPriceID,
			Status:  models.StatusActive,
			Rail:    models.RailNMI,
		})
		defer suite.CleanupSubscriptionsForUser(userID)

		// Try to purchase Premium - should detect as downgrade
		checkoutService := suite.App.Runtime.CheckoutService

		eligibility, err := checkoutService.CheckPurchaseEligibility(ctx, userID, premiumPriceID)
		require.NoError(t, err, "Should check eligibility without error")
		assert.Equal(t, checkout.EligibilityDowngrade, eligibility.Status, "Should detect downgrade scenario")
		assert.NotNil(t, eligibility.ExistingSubscription, "Should have existing subscription")
		assert.Equal(t, sub.ID.String(), eligibility.ExistingSubscription.ID.String())
	})

	t.Run("allows purchase when no existing subscription", func(t *testing.T) {
		newUserID := uuid.New().String()

		checkoutService := suite.App.Runtime.CheckoutService

		eligibility, err := checkoutService.CheckPurchaseEligibility(ctx, newUserID, premiumPriceID)
		require.NoError(t, err, "Should check eligibility without error")
		assert.Equal(t, checkout.EligibilityAllowed, eligibility.Status, "Should allow new subscription")
		assert.Nil(t, eligibility.ExistingSubscription, "Should not have existing subscription")
	})
}

// TestScheduledDowngrade tests that downgrades are scheduled and applied at renewal
func TestScheduledDowngrade(t *testing.T) {
	suite := getSharedTestSuite(t)
	ctx := suite.MerchantCtx()

	// Seed tiered products
	tieredProducts := suite.SeedTieredProducts()
	premiumPriceID := tieredProducts[0].Prices[0].ID     // $10/month, rank 1
	premiumPlusPriceID := tieredProducts[1].Prices[0].ID // $20/month, rank 2
	premiumProduct := tieredProducts[0].Product
	premiumPlusProduct := tieredProducts[1].Product

	t.Run("downgrade is scheduled for end of period", func(t *testing.T) {
		userID := uuid.New().String()
		now := suite.GetClock().Now()
		periodEnd := now.Add(15 * 24 * time.Hour) // 15 days remaining

		// Create Premium+ subscription
		sub := suite.CreateTestSubscriptionWithOptions(SubscriptionOptions{
			UserID:              userID,
			PriceID:             premiumPlusPriceID,
			Status:              models.StatusActive,
			Rail:                models.RailNMI,
			PeriodStart:         now.Add(-15 * 24 * time.Hour), // Started 15 days ago
			CurrentPeriodEndsAt: &periodEnd,
		})
		defer suite.CleanupSubscriptionsForUser(userID)

		// Create Premium+ entitlements
		suite.CreateTestEntitlement(userID, "premium", &sub.ID, models.EntitlementSourceSubscription)
		suite.CreateTestEntitlement(userID, "extra", &sub.ID, models.EntitlementSourceSubscription)

		// Set scheduled downgrade to Premium
		sub.ScheduledPriceID = &premiumPriceID
		_, err := suite.Pool.Exec(ctx,
			"UPDATE openrails.subscriptions SET scheduled_price_id = $1 WHERE id = $2",
			sub.ScheduledPriceID, sub.ID)
		require.NoError(t, err, "Should update scheduled price")

		// Verify subscription still has Premium+ entitlements
		ents := suite.GetEntitlementsByUserID(userID)
		entNames := make(map[string]bool)
		for _, e := range ents {
			entNames[e.Entitlement] = true
		}
		assert.True(t, entNames["premium"], "Should still have premium entitlement")
		assert.True(t, entNames["extra"], "Should still have extra entitlement (downgrade not applied yet)")

		// Verify subscription price is still Premium+
		refreshedSub := suite.GetSubscription(sub.ID)
		assert.Equal(t, premiumPlusPriceID.String(), refreshedSub.PriceID.String(), "Price should still be Premium+")
		assert.Equal(t, premiumPlusProduct.ID.String(), refreshedSub.ProductID.String(), "Product should still be Premium+")
		assert.NotNil(t, refreshedSub.ScheduledPriceID, "Should have scheduled price ID")
	})

	t.Run("downgrade is applied on renewal", func(t *testing.T) {
		userID := uuid.New().String()
		now := suite.GetClock().Now()
		periodEnd := now // Period ends now

		// Create Premium+ subscription with period ending now
		sub := suite.CreateTestSubscriptionWithOptions(SubscriptionOptions{
			UserID:              userID,
			PriceID:             premiumPlusPriceID,
			Status:              models.StatusActive,
			Rail:                models.RailNMI,
			RailSubID:           "test-renewal-" + uuid.New().String()[:8],
			PeriodStart:         now.Add(-30 * 24 * time.Hour),
			CurrentPeriodEndsAt: &periodEnd,
		})
		defer suite.CleanupSubscriptionsForUser(userID)

		// Set scheduled downgrade to Premium
		sub.ScheduledPriceID = &premiumPriceID
		_, err := suite.Pool.Exec(ctx,
			"UPDATE openrails.subscriptions SET scheduled_price_id = $1 WHERE id = $2",
			sub.ScheduledPriceID, sub.ID)
		require.NoError(t, err, "Should update scheduled price")

		// Create entitlements
		suite.CreateTestEntitlement(userID, "premium", &sub.ID, models.EntitlementSourceSubscription)
		suite.CreateTestEntitlement(userID, "extra", &sub.ID, models.EntitlementSourceSubscription)

		// Use lifecycle service from runtime
		lifecycleService := suite.App.Runtime.SubscriptionLifecycleService

		err = lifecycleService.RenewMembership(ctx, &subscriptions.RenewMembershipParams{
			Rail:               models.RailNMI,
			RailSubscriptionID: sub.RailSubscriptionID,
			TransactionID:      "renewal-txn-" + uuid.New().String()[:8],
			Amount:             1000, // $10 (Premium price)
			Currency:           "usd",
		})
		require.NoError(t, err, "Renewal should succeed")

		// Verify subscription switched to Premium
		refreshedSub := suite.GetSubscription(sub.ID)
		assert.Equal(t, premiumPriceID.String(), refreshedSub.PriceID.String(), "Price should be switched to Premium")
		assert.Equal(t, premiumProduct.ID.String(), refreshedSub.ProductID.String(), "Product should be switched to Premium")
		assert.Nil(t, refreshedSub.ScheduledPriceID, "Scheduled price should be cleared")
		assert.Equal(t, models.StatusActive, refreshedSub.Status, "Subscription should be active")

		// Entitlements cut over at the same renewal boundary: the lower tier's
		// retained entitlement remains while the higher-tier-only grant is gone.
		ents := suite.GetEntitlementsByUserID(userID)
		entNames := make(map[string]bool, len(ents))
		for _, entitlement := range ents {
			entNames[entitlement.Entitlement] = true
		}
		assert.True(t, entNames["premium"], "Should retain the lower-tier entitlement")
		assert.False(t, entNames["extra"], "Should revoke the higher-tier-only entitlement")
	})
}

// TestChangeTierEndpoint tests the POST /v1/me/subscriptions/:id/change-tier endpoint
func TestChangeTierEndpoint(t *testing.T) {
	suite, mock := SetupSuiteWithMockNMI(t)

	// Seed tiered products
	tieredProducts := suite.SeedTieredProducts()
	premiumPriceID := tieredProducts[0].Prices[0].ID     // $10/month, rank 1
	premiumPlusPriceID := tieredProducts[1].Prices[0].ID // $20/month, rank 2

	t.Run("requires authentication", func(t *testing.T) {
		dummySubID := "sub_" + uuid.New().String()
		body := map[string]string{
			"price_id": premiumPlusPriceID.String(),
		}
		jsonBody, _ := json.Marshal(body)

		w := httptest.NewRecorder()
		req, _ := http.NewRequest("POST", "/v1/me/subscriptions/"+dummySubID+"/change-tier", bytes.NewReader(jsonBody))
		req.Header.Set("Content-Type", "application/json")

		suite.Server.Handler().ServeHTTP(w, req)

		assert.Equal(t, http.StatusUnauthorized, w.Code, "Should return 401 without auth")
	})

	t.Run("returns 404 when subscription not found", func(t *testing.T) {
		userID := uuid.New().String()
		email := userID + "@test.example.com"
		token := suite.MintUserToken(userID, email)

		nonExistentSubID := "sub_" + uuid.New().String()
		body := map[string]string{
			"price_id": premiumPlusPriceID.String(),
		}
		jsonBody, _ := json.Marshal(body)

		w := httptest.NewRecorder()
		req, _ := http.NewRequest("POST", "/v1/me/subscriptions/"+nonExistentSubID+"/change-tier", bytes.NewReader(jsonBody))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)

		suite.Server.Handler().ServeHTTP(w, req)

		assert.Equal(t, http.StatusNotFound, w.Code, "Should return 404 when subscription not found")
	})

	t.Run("returns 409 when already on same plan", func(t *testing.T) {
		userID := uuid.New().String()
		email := userID + "@test.example.com"
		token := suite.MintUserToken(userID, email)

		// Create subscription on Premium
		sub := suite.CreateTestSubscriptionWithOptions(SubscriptionOptions{
			UserID:  userID,
			PriceID: premiumPriceID,
			Status:  models.StatusActive,
			Rail:    models.RailNMI,
		})
		defer suite.CleanupSubscriptionsForUser(userID)

		// Try to change to same price
		body := map[string]string{
			"price_id": premiumPriceID.String(),
		}
		jsonBody, _ := json.Marshal(body)

		w := httptest.NewRecorder()
		req, _ := http.NewRequest("POST", "/v1/me/subscriptions/sub_"+sub.ID.String()+"/change-tier", bytes.NewReader(jsonBody))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)

		suite.Server.Handler().ServeHTTP(w, req)

		assert.Equal(t, http.StatusConflict, w.Code, "Should return 409 when already on same plan")
	})

	t.Run("Mobius upgrade succeeds with proration", func(t *testing.T) {
		userID := uuid.New().String()
		email := userID + "@test.example.com"
		token := suite.MintUserToken(userID, email)

		now := suite.GetClock().Now()
		periodEnd := now.Add(15 * 24 * time.Hour) // 15 days remaining

		// Create Premium subscription with payment method
		pm := suite.CreateTestPaymentMethodWithOptions(PaymentMethodOptions{
			UserID:  userID,
			Rail:    models.RailNMI,
			VaultID: "vault-" + uuid.New().String()[:8],
		})

		sub := suite.CreateTestSubscriptionWithOptions(SubscriptionOptions{
			UserID:              userID,
			PriceID:             premiumPriceID,
			Status:              models.StatusActive,
			Rail:                models.RailNMI,
			RailSubID:           "nmi-sub-" + uuid.New().String()[:8],
			PaymentMethodID:     &pm.ID,
			CurrentPeriodEndsAt: &periodEnd,
		})
		defer suite.CleanupSubscriptionsForUser(userID)

		mock.Reset()

		// Request upgrade to Premium+
		body := map[string]string{
			"price_id": premiumPlusPriceID.String(),
		}
		jsonBody, _ := json.Marshal(body)

		w := httptest.NewRecorder()
		req, _ := http.NewRequest("POST", "/v1/me/subscriptions/sub_"+sub.ID.String()+"/change-tier", bytes.NewReader(jsonBody))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)

		suite.Server.Handler().ServeHTTP(w, req)

		require.Equal(t, http.StatusOK, w.Code, "Should return 200 OK, got body: %s", w.Body.String())

		var resp map[string]any
		err := json.Unmarshal(w.Body.Bytes(), &resp)
		require.NoError(t, err)

		assert.Equal(t, "tier_change", resp["object"], "Object should be tier_change")
		assert.Equal(t, "succeeded", resp["status"], "Status should be succeeded")
		assert.Equal(t, "tier_change", resp["mode"], "Mode should be tier_change")
		assert.Equal(t, "upgrade", resp["action"], "Action should be upgrade")
		assert.NotEmpty(t, resp["subscription_id"], "Should include subscription_id")

		// Verify NMI calls were made (sale for proration + new subscription)
		assert.GreaterOrEqual(t, int(mock.RequestCount), 1, "Should have made NMI API calls")

		// Verify old subscription was cancelled
		refreshedSub := suite.GetSubscription(sub.ID)
		assert.Equal(t, models.StatusCancelled, refreshedSub.Status, "Old subscription should be cancelled")
	})

	t.Run("Mobius downgrade is scheduled", func(t *testing.T) {
		userID := uuid.New().String()
		email := userID + "@test.example.com"
		token := suite.MintUserToken(userID, email)

		now := suite.GetClock().Now()
		periodEnd := now.Add(15 * 24 * time.Hour)

		// Create Premium+ subscription
		sub := suite.CreateTestSubscriptionWithOptions(SubscriptionOptions{
			UserID:              userID,
			PriceID:             premiumPlusPriceID,
			Status:              models.StatusActive,
			Rail:                models.RailNMI,
			RailSubID:           "nmi-sub-" + uuid.New().String()[:8],
			CurrentPeriodEndsAt: &periodEnd,
		})
		defer suite.CleanupSubscriptionsForUser(userID)

		// Request downgrade to Premium
		body := map[string]string{
			"price_id": premiumPriceID.String(),
		}
		jsonBody, _ := json.Marshal(body)

		w := httptest.NewRecorder()
		req, _ := http.NewRequest("POST", "/v1/me/subscriptions/sub_"+sub.ID.String()+"/change-tier", bytes.NewReader(jsonBody))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)

		suite.Server.Handler().ServeHTTP(w, req)

		require.Equal(t, http.StatusOK, w.Code, "Should return 200 OK, got body: %s", w.Body.String())

		var resp map[string]any
		err := json.Unmarshal(w.Body.Bytes(), &resp)
		require.NoError(t, err)

		assert.Equal(t, "tier_change", resp["object"], "Object should be tier_change")
		assert.Equal(t, "succeeded", resp["status"], "Status should be succeeded")
		assert.Equal(t, "downgrade", resp["action"], "Action should be downgrade")
		assert.NotEmpty(t, resp["delayed_start"], "Should include delayed_start for scheduled downgrade")
		assert.Contains(t, resp["message"].(string), "scheduled", "Message should mention scheduled")

		// Verify subscription has scheduled price change
		refreshedSub := suite.GetSubscription(sub.ID)
		assert.NotNil(t, refreshedSub.ScheduledPriceID, "Should have scheduled price ID")
		assert.Equal(t, premiumPriceID.String(), refreshedSub.ScheduledPriceID.String(), "Scheduled price should be Premium")
	})
}

// TestAdminChangeTierParity proves the merchant-admin routes delegate to the
// same preview/action path as self-service. The delegated admin subject is
// deliberately different from the subscription customer: the handler must
// resolve the customer from the subscription rather than charge as the actor.
func TestAdminChangeTierParity(t *testing.T) {
	suite, _ := SetupSuiteWithMockNMI(t)
	admin := newHostSeamAdminRouter(t, suite, "b8048048-0480-4804-8804-804804804804",
		[]string{controlplane.PermMerchantSubscriptionsUpdate})

	tieredProducts := suite.SeedTieredProducts()
	premiumPriceID := tieredProducts[0].Prices[0].ID
	premiumPlusPriceID := tieredProducts[1].Prices[0].ID

	preview := func(
		t *testing.T,
		handler http.Handler,
		path, token, priceID string,
	) checkout.TierChangePreviewResponse {
		t.Helper()
		body, err := json.Marshal(map[string]string{"price_id": priceID})
		require.NoError(t, err)

		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)
		handler.ServeHTTP(w, req)
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())

		var response checkout.TierChangePreviewResponse
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &response))
		return response
	}

	for _, tc := range []struct {
		name           string
		currentPrice   uuid.UUID
		targetPrice    uuid.UUID
		expectedAction string
	}{
		{name: "upgrade", currentPrice: premiumPriceID, targetPrice: premiumPlusPriceID, expectedAction: "upgrade"},
		{name: "downgrade", currentPrice: premiumPlusPriceID, targetPrice: premiumPriceID, expectedAction: "downgrade"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			userID := uuid.NewString()
			email := userID + "@test.example.com"
			periodEnd := suite.GetClock().Now().Add(15 * 24 * time.Hour)
			var paymentMethodID *uuid.UUID
			if tc.expectedAction == "upgrade" {
				paymentMethod := suite.CreateTestPaymentMethodWithOptions(PaymentMethodOptions{
					UserID:  userID,
					Rail:    models.RailNMI,
					VaultID: "vault-" + uuid.NewString()[:8],
				})
				paymentMethodID = &paymentMethod.ID
			}
			sub := suite.CreateTestSubscriptionWithOptions(SubscriptionOptions{
				UserID:              userID,
				PriceID:             tc.currentPrice,
				Status:              models.StatusActive,
				Rail:                models.RailNMI,
				RailSubID:           "admin-tier-change-" + uuid.NewString()[:8],
				PaymentMethodID:     paymentMethodID,
				CurrentPeriodEndsAt: &periodEnd,
			})
			defer suite.CleanupSubscriptionsForUser(userID)
			_, err := suite.Pool.Exec(suite.MerchantCtx(),
				"UPDATE openrails.subscriptions SET user_email = $1 WHERE id = $2", email, sub.ID)
			require.NoError(t, err)

			self := preview(t, suite.Server.Handler(),
				"/v1/me/subscriptions/sub_"+sub.ID.String()+"/change-tier/preview",
				suite.MintUserToken(userID, email),
				tc.targetPrice.String())
			merchant := preview(t, admin,
				"/v1/merchant/subscriptions/sub_"+sub.ID.String()+"/change-tier/preview",
				merchantDelegatedTestToken,
				tc.targetPrice.String())

			require.Equal(t, tc.expectedAction, merchant.Action)
			require.Equal(t, self.Action, merchant.Action)
			require.Equal(t, self.PriceID, merchant.PriceID)
			require.Equal(t, self.Rail, merchant.Rail)
			require.Equal(t, self.Currency, merchant.Currency)
			require.Equal(t, self.AmountDueNow, merchant.AmountDueNow)
			require.Equal(t, self.NextChargeAmount, merchant.NextChargeAmount)
			require.NotNil(t, self.NextChargeDate)
			require.NotNil(t, merchant.NextChargeDate)
			require.WithinDuration(t, *self.NextChargeDate, *merchant.NextChargeDate, time.Second)
			require.Equal(t, self.Effective, merchant.Effective)
			require.Equal(t, self.IsEstimate, merchant.IsEstimate)

			body, err := json.Marshal(map[string]string{"price_id": tc.targetPrice.String()})
			require.NoError(t, err)
			w := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost,
				"/v1/merchant/subscriptions/sub_"+sub.ID.String()+"/change-tier",
				bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer "+merchantDelegatedTestToken)
			admin.ServeHTTP(w, req)
			require.Equal(t, http.StatusOK, w.Code, w.Body.String())

			var changed checkout.TierChangeResponse
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &changed))
			require.Equal(t, "succeeded", changed.Status)
			require.Equal(t, tc.expectedAction, changed.Action)
			if tc.expectedAction == "downgrade" {
				updated := suite.GetSubscription(sub.ID)
				require.NotNil(t, updated.ScheduledPriceID)
				require.Equal(t, tc.targetPrice, *updated.ScheduledPriceID)
			} else {
				require.NotNil(t, changed.SubscriptionID)
				newSubscriptionID, err := api.ParseSubscriptionID(*changed.SubscriptionID)
				require.NoError(t, err)
				updated := suite.GetSubscription(newSubscriptionID)
				require.NotNil(t, updated.UserEmail)
				require.Equal(t, email, *updated.UserEmail)
			}
		})
	}
}

func TestAdminChangeTierGuards(t *testing.T) {
	suite, mock := SetupSuiteWithMockNMI(t)
	admin := newHostSeamAdminRouter(t, suite, "b8048048-0480-4804-8804-804804804805",
		[]string{controlplane.PermMerchantSubscriptionsUpdate})
	tieredProducts := suite.SeedTieredProducts()
	premiumPriceID := tieredProducts[0].Prices[0].ID
	premiumPlusPriceID := tieredProducts[1].Prices[0].ID

	post := func(t *testing.T, subID uuid.UUID, suffix string) *httptest.ResponseRecorder {
		t.Helper()
		body, err := json.Marshal(map[string]string{"price_id": premiumPlusPriceID.String()})
		require.NoError(t, err)
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost,
			"/v1/merchant/subscriptions/sub_"+subID.String()+"/change-tier"+suffix,
			bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+merchantDelegatedTestToken)
		admin.ServeHTTP(rec, req)
		return rec
	}

	t.Run("refuses a cancelled subscription before charging", func(t *testing.T) {
		userID := uuid.NewString()
		sub := suite.CreateTestSubscriptionWithOptions(SubscriptionOptions{
			UserID: userID, PriceID: premiumPriceID, Status: models.StatusCancelled, Rail: models.RailNMI,
		})
		defer suite.CleanupSubscriptionsForUser(userID)
		mock.Reset()

		rec := post(t, sub.ID, "")
		require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())

		serviceRequest := &checkout.TierChangeRequest{
			PriceID:        premiumPlusPriceID.String(),
			SubscriptionID: sub.ID,
		}
		customer := &checkout.UserIdentity{ID: userID}
		_, err := suite.App.Runtime.CheckoutService.TierChange(suite.MerchantCtx(), serviceRequest, customer)
		require.ErrorContains(t, err, "only active or past-due subscriptions")
		_, err = suite.App.Runtime.CheckoutService.TierChangePreview(suite.MerchantCtx(), serviceRequest, customer)
		require.ErrorContains(t, err, "only active or past-due subscriptions")
		require.Zero(t, mock.RequestCount)
	})

	t.Run("refuses an existing scheduled tier change", func(t *testing.T) {
		userID := uuid.NewString()
		sub := suite.CreateTestSubscriptionWithOptions(SubscriptionOptions{
			UserID: userID, PriceID: premiumPriceID, Status: models.StatusActive, Rail: models.RailNMI,
		})
		defer suite.CleanupSubscriptionsForUser(userID)
		_, err := suite.Pool.Exec(suite.MerchantCtx(),
			"UPDATE openrails.subscriptions SET scheduled_price_id = $1 WHERE id = $2", premiumPlusPriceID, sub.ID)
		require.NoError(t, err)

		rec := post(t, sub.ID, "/preview")
		require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
		require.Contains(t, rec.Body.String(), "tier change scheduled")
	})

	t.Run("refuses an existing scheduled reprice", func(t *testing.T) {
		userID := uuid.NewString()
		sub := suite.CreateTestSubscriptionWithOptions(SubscriptionOptions{
			UserID: userID, PriceID: premiumPriceID, Status: models.StatusActive, Rail: models.RailNMI,
		})
		defer suite.CleanupSubscriptionsForUser(userID)
		repriceRepo := subscriptions.NewRepriceRepo(suite.App.Runtime.DB)
		_, err := repriceRepo.CreateSubscriptionReprice(suite.MerchantCtx(), sub.ID,
			premiumPriceID, premiumPlusPriceID, suite.GetClock().Now().Add(24*time.Hour), nil, false)
		require.NoError(t, err)

		rec := post(t, sub.ID, "/preview")
		require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
		require.Contains(t, rec.Body.String(), "scheduled price change")
	})

	for _, rail := range []models.Rail{models.RailCCBill, models.RailSolana} {
		t.Run("requires customer action on "+string(rail), func(t *testing.T) {
			userID := uuid.NewString()
			sub := suite.CreateTestSubscriptionWithOptions(SubscriptionOptions{
				UserID: userID, PriceID: premiumPriceID, Status: models.StatusActive, Rail: rail,
			})
			defer suite.CleanupSubscriptionsForUser(userID)

			rec := post(t, sub.ID, "/preview")
			require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
			require.Contains(t, rec.Body.String(), "require")
		})
	}
}

// TestCheckoutBlocksTierChanges tests that /v1/checkout returns an error for tier changes
func TestCheckoutBlocksTierChanges(t *testing.T) {
	suite, mock := SetupSuiteWithMockNMI(t)

	// Seed tiered products
	tieredProducts := suite.SeedTieredProducts()
	premiumPriceID := tieredProducts[0].Prices[0].ID     // rank 1
	premiumPlusPriceID := tieredProducts[1].Prices[0].ID // rank 2

	t.Run("checkout blocks upgrade attempts", func(t *testing.T) {
		userID := uuid.New().String()
		email := userID + "@test.example.com"
		token := suite.MintUserToken(userID, email)

		// Create Premium subscription
		suite.CreateTestSubscriptionWithOptions(SubscriptionOptions{
			UserID:  userID,
			PriceID: premiumPriceID,
			Status:  models.StatusActive,
			Rail:    models.RailNMI,
		})
		defer suite.CleanupSubscriptionsForUser(userID)

		mock.Reset()

		// Try to checkout Premium+ (should be blocked)
		body := map[string]any{
			"price_id": premiumPlusPriceID.String(),
			"payment": map[string]any{
				"rail":          "nmi",
				"payment_token": "tok_test_123",
				"email":         email,
				"first_name":    "Test",
				"last_name":     "User",
				"address1":      "123 Test St",
				"city":          "Test City",
				"state":         "CA",
				"zip":           "90210",
				"country":       "US",
			},
		}
		jsonBody, _ := json.Marshal(body)

		w := httptest.NewRecorder()
		req, _ := http.NewRequest("POST", "/v1/checkout", bytes.NewReader(jsonBody))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)

		suite.Server.Handler().ServeHTTP(w, req)

		require.Equal(t, http.StatusConflict, w.Code, "Should return 409 Conflict")
		assert.Contains(t, w.Body.String(), "change-tier", "Message should direct to change-tier endpoint")
	})

	t.Run("checkout blocks downgrade attempts", func(t *testing.T) {
		userID := uuid.New().String()
		email := userID + "@test.example.com"
		token := suite.MintUserToken(userID, email)

		// Create Premium+ subscription
		suite.CreateTestSubscriptionWithOptions(SubscriptionOptions{
			UserID:  userID,
			PriceID: premiumPlusPriceID,
			Status:  models.StatusActive,
			Rail:    models.RailNMI,
		})
		defer suite.CleanupSubscriptionsForUser(userID)

		mock.Reset()

		// Try to checkout Premium (should be blocked)
		body := map[string]any{
			"price_id": premiumPriceID.String(),
			"payment": map[string]any{
				"rail":          "nmi",
				"payment_token": "tok_test_123",
				"email":         email,
				"first_name":    "Test",
				"last_name":     "User",
				"address1":      "123 Test St",
				"city":          "Test City",
				"state":         "CA",
				"zip":           "90210",
				"country":       "US",
			},
		}
		jsonBody, _ := json.Marshal(body)

		w := httptest.NewRecorder()
		req, _ := http.NewRequest("POST", "/v1/checkout", bytes.NewReader(jsonBody))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)

		suite.Server.Handler().ServeHTTP(w, req)

		require.Equal(t, http.StatusConflict, w.Code, "Should return 409 Conflict")
		assert.Contains(t, w.Body.String(), "change-tier", "Message should direct to change-tier endpoint")
	})

	t.Run("checkout still works for new subscriptions", func(t *testing.T) {
		userID := uuid.New().String()
		email := userID + "@test.example.com"
		token := suite.MintUserToken(userID, email)

		mock.Reset()

		// Checkout Premium (new subscription, should work)
		body := map[string]any{
			"price_id": premiumPriceID.String(),
			"payment": map[string]any{
				"rail":          "nmi",
				"payment_token": "tok_test_123",
				"email":         email,
				"first_name":    "Test",
				"last_name":     "User",
				"address1":      "123 Test St",
				"city":          "Test City",
				"state":         "CA",
				"zip":           "90210",
				"country":       "US",
			},
		}
		jsonBody, _ := json.Marshal(body)

		w := httptest.NewRecorder()
		req, _ := http.NewRequest("POST", "/v1/checkout", bytes.NewReader(jsonBody))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)

		suite.Server.Handler().ServeHTTP(w, req)
		defer suite.CleanupSubscriptionsForUser(userID)

		require.Equal(t, http.StatusOK, w.Code, "Should return 200 OK, got body: %s", w.Body.String())

		var resp map[string]any
		err := json.Unmarshal(w.Body.Bytes(), &resp)
		require.NoError(t, err)

		assert.Equal(t, "succeeded", resp["status"], "Status should be succeeded for new subscription")
		assert.NotEmpty(t, resp["subscription_id"], "Should include subscription_id")
	})
}
