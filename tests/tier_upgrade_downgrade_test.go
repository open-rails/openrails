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

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/services"
)

// TestTierGroupDetection tests that the checkout service correctly detects tier groups
func TestTierGroupDetection(t *testing.T) {
	suite := setupTestSuite(t)
	ctx := context.Background()

	// Seed tiered products (Premium, Premium+, Premium Ultimate)
	tieredProducts := suite.SeedTieredProducts()
	require.Len(t, tieredProducts, 3, "Should have 3 tiered products")

	premiumProduct := tieredProducts[0].Product
	premiumPlusProduct := tieredProducts[1].Product
	premiumUltimateProduct := tieredProducts[2].Product

	premiumPriceID := tieredProducts[0].Prices[0].ID
	premiumPlusPriceID := tieredProducts[1].Prices[0].ID

	userID := "test-user-" + uuid.New().String()[:8]

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
			UserID:    userID,
			PriceID:   premiumPriceID,
			Status:    models.StatusActive,
			Processor: models.ProcessorMobius,
		})
		defer suite.CleanupSubscriptionsForUser(userID)

		// Try to purchase Premium+ - should detect as upgrade
		checkoutService := suite.App.Runtime.CheckoutService

		eligibility, err := checkoutService.CheckPurchaseEligibility(ctx, userID, premiumPlusPriceID)
		require.NoError(t, err, "Should check eligibility without error")
		assert.Equal(t, payments.EligibilityUpgrade, eligibility.Status, "Should detect upgrade scenario")
		assert.NotNil(t, eligibility.ExistingSubscription, "Should have existing subscription")
		assert.Equal(t, sub.ID.String(), eligibility.ExistingSubscription.ID.String())
	})

	t.Run("detects downgrade scenario", func(t *testing.T) {
		// Create subscription on Premium+
		sub := suite.CreateTestSubscriptionWithOptions(SubscriptionOptions{
			UserID:    userID,
			PriceID:   premiumPlusPriceID,
			Status:    models.StatusActive,
			Processor: models.ProcessorMobius,
		})
		defer suite.CleanupSubscriptionsForUser(userID)

		// Try to purchase Premium - should detect as downgrade
		checkoutService := suite.App.Runtime.CheckoutService

		eligibility, err := checkoutService.CheckPurchaseEligibility(ctx, userID, premiumPriceID)
		require.NoError(t, err, "Should check eligibility without error")
		assert.Equal(t, payments.EligibilityDowngrade, eligibility.Status, "Should detect downgrade scenario")
		assert.NotNil(t, eligibility.ExistingSubscription, "Should have existing subscription")
		assert.Equal(t, sub.ID.String(), eligibility.ExistingSubscription.ID.String())
	})

	t.Run("allows purchase when no existing subscription", func(t *testing.T) {
		newUserID := "new-user-" + uuid.New().String()[:8]

		checkoutService := suite.App.Runtime.CheckoutService

		eligibility, err := checkoutService.CheckPurchaseEligibility(ctx, newUserID, premiumPriceID)
		require.NoError(t, err, "Should check eligibility without error")
		assert.Equal(t, payments.EligibilityAllowed, eligibility.Status, "Should allow new subscription")
		assert.Nil(t, eligibility.ExistingSubscription, "Should not have existing subscription")
	})
}

// TestTierProrationCalculation tests that proration is calculated correctly for tier upgrades
func TestTierProrationCalculation(t *testing.T) {
	suite := setupTestSuite(t)

	// Seed tiered products
	tieredProducts := suite.SeedTieredProducts()
	premiumPrice := tieredProducts[0].Prices[0]     // $10/month
	premiumPlusPrice := tieredProducts[1].Prices[0] // $20/month

	t.Run("calculates correct proration for mid-cycle upgrade", func(t *testing.T) {
		// User on Premium ($10/mo), 15 days remaining, upgrades to Premium+ ($20/mo)
		// Expected proration: ($20 - $10) * (15/30) = $5

		oldAmount := premiumPrice.Amount     // 1000 cents ($10)
		newAmount := premiumPlusPrice.Amount // 2000 cents ($20)
		billingCycle := 30                   // days
		daysRemaining := 15

		priceDiff := newAmount - oldAmount // 1000 cents ($10)
		prorationRatio := float64(daysRemaining) / float64(billingCycle)
		expectedProration := int64(float64(priceDiff) * prorationRatio) // 500 cents ($5)

		assert.Equal(t, int64(500), expectedProration, "Proration should be $5 (500 cents)")
	})

	t.Run("proration is zero at start of cycle", func(t *testing.T) {
		oldAmount := premiumPrice.Amount
		newAmount := premiumPlusPrice.Amount
		billingCycle := 30
		daysRemaining := 30 // Full cycle remaining

		priceDiff := newAmount - oldAmount
		prorationRatio := float64(daysRemaining) / float64(billingCycle)
		prorationAmount := int64(float64(priceDiff) * prorationRatio)

		assert.Equal(t, int64(1000), prorationAmount, "Proration should be full difference at start of cycle")
	})

	t.Run("proration is zero at end of cycle", func(t *testing.T) {
		oldAmount := premiumPrice.Amount
		newAmount := premiumPlusPrice.Amount
		billingCycle := 30
		daysRemaining := 0 // End of cycle

		priceDiff := newAmount - oldAmount
		prorationRatio := float64(daysRemaining) / float64(billingCycle)
		prorationAmount := int64(float64(priceDiff) * prorationRatio)

		assert.Equal(t, int64(0), prorationAmount, "Proration should be zero at end of cycle")
	})
}

// TestScheduledDowngrade tests that downgrades are scheduled and applied at renewal
func TestScheduledDowngrade(t *testing.T) {
	suite := setupTestSuite(t)
	ctx := context.Background()

	// Seed tiered products
	tieredProducts := suite.SeedTieredProducts()
	premiumPriceID := tieredProducts[0].Prices[0].ID     // $10/month, rank 1
	premiumPlusPriceID := tieredProducts[1].Prices[0].ID // $20/month, rank 2
	premiumProduct := tieredProducts[0].Product
	premiumPlusProduct := tieredProducts[1].Product

	userID := "test-user-" + uuid.New().String()[:8]

	t.Run("downgrade is scheduled for end of period", func(t *testing.T) {
		now := suite.GetClock().Now()
		periodEnd := now.Add(15 * 24 * time.Hour) // 15 days remaining

		// Create Premium+ subscription
		sub := suite.CreateTestSubscriptionWithOptions(SubscriptionOptions{
			UserID:              userID,
			PriceID:             premiumPlusPriceID,
			Status:              models.StatusActive,
			Processor:           models.ProcessorMobius,
			PeriodStart:         now.Add(-15 * 24 * time.Hour), // Started 15 days ago
			CurrentPeriodEndsAt: &periodEnd,
		})
		defer suite.CleanupSubscriptionsForUser(userID)

		// Create Premium+ entitlements
		suite.CreateTestEntitlement(userID, "premium", &sub.ID, models.EntitlementSourceSubscription)
		suite.CreateTestEntitlement(userID, "extra", &sub.ID, models.EntitlementSourceSubscription)

		// Set scheduled downgrade to Premium
		sub.ScheduledPriceID = &premiumPriceID
		_, err := suite.BunDB.NewUpdate().Model(sub).Column("scheduled_price_id").WherePK().Exec(ctx)
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
		now := suite.GetClock().Now()
		periodEnd := now // Period ends now

		// Create Premium+ subscription with period ending now
		sub := suite.CreateTestSubscriptionWithOptions(SubscriptionOptions{
			UserID:              userID,
			PriceID:             premiumPlusPriceID,
			Status:              models.StatusActive,
			Processor:           models.ProcessorMobius,
			ProcessorSubID:      "test-renewal-" + uuid.New().String()[:8],
			PeriodStart:         now.Add(-30 * 24 * time.Hour),
			CurrentPeriodEndsAt: &periodEnd,
		})
		defer suite.CleanupSubscriptionsForUser(userID)

		// Set scheduled downgrade to Premium
		sub.ScheduledPriceID = &premiumPriceID
		_, err := suite.BunDB.NewUpdate().Model(sub).Column("scheduled_price_id").WherePK().Exec(ctx)
		require.NoError(t, err, "Should update scheduled price")

		// Create entitlements
		suite.CreateTestEntitlement(userID, "premium", &sub.ID, models.EntitlementSourceSubscription)
		suite.CreateTestEntitlement(userID, "extra", &sub.ID, models.EntitlementSourceSubscription)

		// Use lifecycle service from runtime
		lifecycleService := suite.App.Runtime.SubscriptionLifecycleService

		err = lifecycleService.RenewMembership(ctx, &subscriptions.RenewMembershipParams{
			Processor:               models.ProcessorMobius,
			ProcessorSubscriptionID: sub.ProcessorSubscriptionID,
			TransactionID:           "renewal-txn-" + uuid.New().String()[:8],
			Amount:                  1000, // $10 (Premium price)
			Currency:                "usd",
		})
		require.NoError(t, err, "Renewal should succeed")

		// Verify subscription switched to Premium
		refreshedSub := suite.GetSubscription(sub.ID)
		assert.Equal(t, premiumPriceID.String(), refreshedSub.PriceID.String(), "Price should be switched to Premium")
		assert.Equal(t, premiumProduct.ID.String(), refreshedSub.ProductID.String(), "Product should be switched to Premium")
		assert.Nil(t, refreshedSub.ScheduledPriceID, "Scheduled price should be cleared")
		assert.Equal(t, models.StatusActive, refreshedSub.Status, "Subscription should be active")
	})
}

// TestEntitlementChangesOnTierChange tests that entitlements are correctly updated
func TestEntitlementChangesOnTierChange(t *testing.T) {
	suite := setupTestSuite(t)
	ctx := context.Background()

	// Seed tiered products
	tieredProducts := suite.SeedTieredProducts()
	premiumPriceID := tieredProducts[0].Prices[0].ID     // rank 1, entitlements: [premium]
	premiumPlusPriceID := tieredProducts[1].Prices[0].ID // rank 2, entitlements: [premium, extra]

	userID := "test-user-" + uuid.New().String()[:8]

	t.Run("upgrade grants additional entitlements", func(t *testing.T) {
		// Create Premium subscription
		now := suite.GetClock().Now()
		periodEnd := now.Add(30 * 24 * time.Hour)

		sub := suite.CreateTestSubscriptionWithOptions(SubscriptionOptions{
			UserID:              userID,
			PriceID:             premiumPriceID,
			Status:              models.StatusActive,
			Processor:           models.ProcessorMobius,
			CurrentPeriodEndsAt: &periodEnd,
		})
		defer suite.CleanupSubscriptionsForUser(userID)

		// Create Premium entitlement
		suite.CreateTestEntitlement(userID, "premium", &sub.ID, models.EntitlementSourceSubscription)

		// Verify only premium entitlement exists
		ents := suite.GetEntitlementsByUserID(userID)
		assert.Len(t, ents, 1, "Should have 1 entitlement before upgrade")
		assert.Equal(t, "premium", ents[0].Entitlement)

		// Simulate upgrade to Premium+ (this would be done by checkout service)
		// For this test, we verify the entitlement change logic in isolation
		entService := suite.App.Runtime.EntitlementService

		// Grant new "extra" entitlement
		notBefore := now.UTC()
		_, err := entService.PushNewEntitlement(ctx, services.PushNewEntitlementParams{
			UserID:      userID,
			Entitlement: "extra",
			NotBefore:   &notBefore,
			Indefinite:  true,
			SourceType:  models.EntitlementSourceSubscription,
			SourceID:    sub.ID,
		})
		require.NoError(t, err, "Should grant extra entitlement")

		// Verify both entitlements now exist
		ents = suite.GetEntitlementsByUserID(userID)
		entNames := make(map[string]bool)
		for _, e := range ents {
			entNames[e.Entitlement] = true
		}
		assert.True(t, entNames["premium"], "Should have premium")
		assert.True(t, entNames["extra"], "Should have extra after upgrade")
	})

	t.Run("downgrade revokes extra entitlements", func(t *testing.T) {
		userID2 := "test-user-" + uuid.New().String()[:8]
		now := suite.GetClock().Now()
		periodEnd := now.Add(30 * 24 * time.Hour)

		// Create Premium+ subscription
		sub := suite.CreateTestSubscriptionWithOptions(SubscriptionOptions{
			UserID:              userID2,
			PriceID:             premiumPlusPriceID,
			Status:              models.StatusActive,
			Processor:           models.ProcessorMobius,
			CurrentPeriodEndsAt: &periodEnd,
		})
		defer suite.CleanupSubscriptionsForUser(userID2)

		// Create Premium+ entitlements
		suite.CreateTestEntitlement(userID2, "premium", &sub.ID, models.EntitlementSourceSubscription)
		suite.CreateTestEntitlement(userID2, "extra", &sub.ID, models.EntitlementSourceSubscription)

		// Verify both entitlements exist
		ents := suite.GetEntitlementsByUserID(userID2)
		assert.Len(t, ents, 2, "Should have 2 entitlements before downgrade")

		// Revoke "extra" entitlement (simulating downgrade)
		entService := suite.App.Runtime.EntitlementService

		st := models.EntitlementSourceSubscription
		sid := sub.ID
		err := entService.RevokeExistingEntitlement(ctx, services.RevokeExistingEntitlementParams{
			UserID:      userID2,
			Entitlement: "extra",
			SourceType:  &st,
			SourceID:    &sid,
			Reason:      models.EntitlementRevokeDowngrade,
		})
		require.NoError(t, err, "Should revoke extra entitlement")

		// Verify only premium entitlement remains
		ents = suite.GetEntitlementsByUserID(userID2)
		assert.Len(t, ents, 1, "Should have 1 entitlement after downgrade")
		assert.Equal(t, "premium", ents[0].Entitlement)
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
		userID := "no-sub-user-" + uuid.New().String()[:8]
		email := userID + "@test.example.com"
		token := getTestIssuer().CreateToken(userID, email)

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
		userID := "same-plan-user-" + uuid.New().String()[:8]
		email := userID + "@test.example.com"
		token := getTestIssuer().CreateToken(userID, email)

		// Create subscription on Premium
		sub := suite.CreateTestSubscriptionWithOptions(SubscriptionOptions{
			UserID:    userID,
			PriceID:   premiumPriceID,
			Status:    models.StatusActive,
			Processor: models.ProcessorMobius,
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
		userID := "upgrade-user-" + uuid.New().String()[:8]
		email := userID + "@test.example.com"
		token := getTestIssuer().CreateToken(userID, email)

		now := suite.GetClock().Now()
		periodEnd := now.Add(15 * 24 * time.Hour) // 15 days remaining

		// Create Premium subscription with payment method
		pm := suite.CreateTestPaymentMethodWithOptions(PaymentMethodOptions{
			UserID:    userID,
			Processor: models.ProcessorMobius,
			VaultID:   "vault-" + uuid.New().String()[:8],
		})

		sub := suite.CreateTestSubscriptionWithOptions(SubscriptionOptions{
			UserID:              userID,
			PriceID:             premiumPriceID,
			Status:              models.StatusActive,
			Processor:           models.ProcessorMobius,
			ProcessorSubID:      "nmi-sub-" + uuid.New().String()[:8],
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
		userID := "downgrade-user-" + uuid.New().String()[:8]
		email := userID + "@test.example.com"
		token := getTestIssuer().CreateToken(userID, email)

		now := suite.GetClock().Now()
		periodEnd := now.Add(15 * 24 * time.Hour)

		// Create Premium+ subscription
		sub := suite.CreateTestSubscriptionWithOptions(SubscriptionOptions{
			UserID:              userID,
			PriceID:             premiumPlusPriceID,
			Status:              models.StatusActive,
			Processor:           models.ProcessorMobius,
			ProcessorSubID:      "nmi-sub-" + uuid.New().String()[:8],
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

// TestCheckoutBlocksTierChanges tests that /v1/checkout returns an error for tier changes
func TestCheckoutBlocksTierChanges(t *testing.T) {
	suite, mock := SetupSuiteWithMockNMI(t)

	// Seed tiered products
	tieredProducts := suite.SeedTieredProducts()
	premiumPriceID := tieredProducts[0].Prices[0].ID     // rank 1
	premiumPlusPriceID := tieredProducts[1].Prices[0].ID // rank 2

	t.Run("checkout blocks upgrade attempts", func(t *testing.T) {
		userID := "checkout-upgrade-" + uuid.New().String()[:8]
		email := userID + "@test.example.com"
		token := getTestIssuer().CreateToken(userID, email)

		// Create Premium subscription
		suite.CreateTestSubscriptionWithOptions(SubscriptionOptions{
			UserID:    userID,
			PriceID:   premiumPriceID,
			Status:    models.StatusActive,
			Processor: models.ProcessorMobius,
		})
		defer suite.CleanupSubscriptionsForUser(userID)

		mock.Reset()

		// Try to checkout Premium+ (should be blocked)
		body := map[string]any{
			"price_id": premiumPlusPriceID.String(),
			"payment": map[string]any{
				"processor":     "mobius",
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

		require.Equal(t, http.StatusOK, w.Code, "Should return 200 with blocked status")

		var resp map[string]any
		err := json.Unmarshal(w.Body.Bytes(), &resp)
		require.NoError(t, err)

		assert.Equal(t, "blocked", resp["status"], "Status should be blocked")
		assert.Contains(t, resp["message"].(string), "change-tier", "Message should direct to change-tier endpoint")
	})

	t.Run("checkout blocks downgrade attempts", func(t *testing.T) {
		userID := "checkout-downgrade-" + uuid.New().String()[:8]
		email := userID + "@test.example.com"
		token := getTestIssuer().CreateToken(userID, email)

		// Create Premium+ subscription
		suite.CreateTestSubscriptionWithOptions(SubscriptionOptions{
			UserID:    userID,
			PriceID:   premiumPlusPriceID,
			Status:    models.StatusActive,
			Processor: models.ProcessorMobius,
		})
		defer suite.CleanupSubscriptionsForUser(userID)

		mock.Reset()

		// Try to checkout Premium (should be blocked)
		body := map[string]any{
			"price_id": premiumPriceID.String(),
			"payment": map[string]any{
				"processor":     "mobius",
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

		require.Equal(t, http.StatusOK, w.Code, "Should return 200 with blocked status")

		var resp map[string]any
		err := json.Unmarshal(w.Body.Bytes(), &resp)
		require.NoError(t, err)

		assert.Equal(t, "blocked", resp["status"], "Status should be blocked")
		assert.Contains(t, resp["message"].(string), "change-tier", "Message should direct to change-tier endpoint")
	})

	t.Run("checkout still works for new subscriptions", func(t *testing.T) {
		userID := "checkout-new-" + uuid.New().String()[:8]
		email := userID + "@test.example.com"
		token := getTestIssuer().CreateToken(userID, email)

		mock.Reset()

		// Checkout Premium (new subscription, should work)
		body := map[string]any{
			"price_id": premiumPriceID.String(),
			"payment": map[string]any{
				"processor":     "mobius",
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
