//go:build integration

package tests

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/entitlements"
	"github.com/stretchr/testify/require"
)

func TestEntitlements_MixedSources_MultipleEntitlements(t *testing.T) {
	suite := setupTestSuite(t)
	rt := suite.App.Runtime
	require.NotNil(t, rt)
	require.NotNil(t, rt.DB)
	require.NotNil(t, rt.EntitlementService)

	ctx := context.Background()
	baseNow := time.Now().UTC().Truncate(time.Second)
	t0 := baseNow.Add(-60 * 24 * time.Hour)
	clock := suite.SetMockClock(t0)
	require.IsType(t, &clockwork.FakeClock{}, clock)

	// Product + price with multiple entitlements.
	productID := uuid.New()
	priceID := uuid.New()
	billingDays := 30
	_, err := suite.BunDB.NewInsert().Model(&models.Product{
		ID:          productID,
		Slug:        "test_mixed_sources_" + uuid.New().String()[:8],
		DisplayName: "Test Product",
		Description: "Test",
		EntitlementsSpec: map[string]*int{
			"premium": nil,
			"extra":   nil,
		},
		IsActive:  true,
		CreatedAt: clock.Now().UTC(),
		UpdatedAt: clock.Now().UTC(),
	}).Exec(ctx)
	require.NoError(t, err)

	_, err = suite.BunDB.NewInsert().Model(&models.Price{
		ID:               priceID,
		ProductID:        productID,
		DisplayName:      "Test Monthly",
		IsActive:         true,
		Amount:           999,
		Currency:         "usd",
		BillingCycleDays: &billingDays,
		CreatedAt:        clock.Now().UTC(),
		UpdatedAt:        clock.Now().UTC(),
	}).Exec(ctx)
	require.NoError(t, err)

	userID := uuid.New().String()
	subID := uuid.New()
	periodStart := t0
	paidEnd := t0.Add(30 * 24 * time.Hour)

	_, err = suite.BunDB.NewInsert().Model(&models.Subscription{
		ID:                      subID,
		UserID:                  userID,
		ProductID:               productID,
		PriceID:                 priceID,
		Status:                  models.StatusActive,
		Processor:               models.ProcessorMobius,
		ProcessorSubscriptionID: "sub_" + uuid.New().String()[:8],
		CurrentPeriodStartsAt:   &periodStart,
		CurrentPeriodEndsAt:     &paidEnd,
		StartedAt:               clock.Now().UTC(),
		CreatedAt:               clock.Now().UTC(),
		UpdatedAt:               clock.Now().UTC(),
	}).Exec(ctx)
	require.NoError(t, err)

	// Subscription grants both entitlements for 30 days.
	for _, entName := range []string{"premium", "extra"} {
		notBefore := periodStart.UTC()
		endAt := paidEnd.UTC()
		_, err := rt.EntitlementService.PushNewEntitlement(ctx, entitlements.PushNewEntitlementParams{
			UserID:      userID,
			Entitlement: entName,
			NotBefore:   &notBefore,
			EndAt:       &endAt,
			SourceType:  models.EntitlementSourceSubscription,
			SourceID:    subID,
		})
		require.NoError(t, err)
	}

	// One-off purchase: premium +10 days, should schedule after subscription end.
	payment := suite.CreateTestPaymentWithOptions(PaymentOptions{
		UserID:      userID,
		PriceID:     priceID,
		Processor:   models.ProcessorMobius,
		Amount:      111,
		Currency:    "usd",
		PurchasedAt: clock.Now().UTC(),
	})
	oneOffEnd := paidEnd.Add(10 * 24 * time.Hour).UTC()
	notBefore := clock.Now().UTC()
	_, err = rt.EntitlementService.PushNewEntitlement(ctx, entitlements.PushNewEntitlementParams{
		UserID:      userID,
		Entitlement: "premium",
		NotBefore:   &notBefore,
		EndAt:       &oneOffEnd,
		SourceType:  models.EntitlementSourceOneOff,
		SourceID:    payment.ID,
	})
	require.NoError(t, err)

	// Admin manual grant: extra +5 days, should schedule after subscription end too.
	adminGrant := &models.AdminGrant{
		ID:        uuid.New(),
		UserID:    userID,
		GrantedBy: "admin",
		Reason:    "test",
		CreatedAt: clock.Now().UTC(),
	}
	_, err = suite.BunDB.NewInsert().Model(adminGrant).Exec(ctx)
	require.NoError(t, err)

	d := 5 * 24 * time.Hour
	_, err = rt.EntitlementService.PushNewEntitlement(ctx, entitlements.PushNewEntitlementParams{
		UserID:      userID,
		Entitlement: "extra",
		NotBefore:   &notBefore,
		Duration:    &d,
		SourceType:  models.EntitlementSourceAdmin,
		SourceID:    adminGrant.ID,
	})
	require.NoError(t, err)

	// Grace: premium +2 days (simulating dunning), should schedule after one-off tail.
	graceUntil := oneOffEnd.Add(2 * 24 * time.Hour).UTC()
	_, err = rt.EntitlementService.PushNewEntitlement(ctx, entitlements.PushNewEntitlementParams{
		UserID:      userID,
		Entitlement: "premium",
		NotBefore:   &notBefore,
		EndAt:       &graceUntil,
		SourceType:  models.EntitlementSourceGrace,
		SourceID:    subID,
	})
	require.NoError(t, err)

	t.Cleanup(func() {
		_, _ = suite.BunDB.NewDelete().Model((*models.Entitlement)(nil)).Where("user_id = ?", userID).Exec(ctx)
		_, _ = suite.BunDB.NewDelete().Model((*models.Payment)(nil)).Where("id = ?", payment.ID).Exec(ctx)
		_, _ = suite.BunDB.NewDelete().Model((*models.AdminGrant)(nil)).Where("id = ?", adminGrant.ID).Exec(ctx)
		_, _ = suite.BunDB.NewDelete().Model((*models.Subscription)(nil)).Where("id = ?", subID).Exec(ctx)
		_, _ = suite.BunDB.NewDelete().Model((*models.Price)(nil)).Where("id = ?", priceID).Exec(ctx)
		_, _ = suite.BunDB.NewDelete().Model((*models.Product)(nil)).Where("id = ?", productID).Exec(ctx)
	})

	// Immediately after purchase (still inside base paid window), both entitlements are active.
	ok, err := rt.EntitlementService.IsEntitled(ctx, userID, "premium", clock.Now().UTC().Add(1*time.Second))
	require.NoError(t, err)
	require.True(t, ok)
	ok, err = rt.EntitlementService.IsEntitled(ctx, userID, "extra", clock.Now().UTC().Add(1*time.Second))
	require.NoError(t, err)
	require.True(t, ok)

	// After subscription ends but before stacked windows end, premium and extra remain active.
	clock.Advance(paidEnd.Sub(clock.Now().UTC()) + time.Second)
	ok, err = rt.EntitlementService.IsEntitled(ctx, userID, "premium", clock.Now().UTC())
	require.NoError(t, err)
	require.True(t, ok)
	ok, err = rt.EntitlementService.IsEntitled(ctx, userID, "extra", clock.Now().UTC())
	require.NoError(t, err)
	require.True(t, ok)

	// After everything, they expire.
	clock.Advance(graceUntil.Sub(clock.Now().UTC()) + time.Second)
	ok, err = rt.EntitlementService.IsEntitled(ctx, userID, "premium", clock.Now().UTC())
	require.NoError(t, err)
	require.False(t, ok)
	ok, err = rt.EntitlementService.IsEntitled(ctx, userID, "extra", clock.Now().UTC())
	require.NoError(t, err)
	require.False(t, ok)
}
