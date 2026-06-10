//go:build integration

package tests

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/db/repo"
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
	suite.InsertProduct(ctx, &models.Product{
		ID:          productID,
		Slug:        "test_mixed_sources_" + uuid.New().String()[:8],
		DisplayName: "Test Product",
		Description: "Test",
		EntitlementsSpec: map[string]*int{
			"premium": nil,
			"extra":   nil,
		},
		Status:    models.CatalogStatusActive,
		CreatedAt: clock.Now().UTC(),
		UpdatedAt: clock.Now().UTC(),
	})

	suite.InsertPrice(ctx, &models.Price{
		ID:               priceID,
		ProductID:        productID,
		Status:           models.CatalogStatusActive,
		Amount:           999,
		Currency:         "usd",
		BillingCycleDays: &billingDays,
		CreatedAt:        clock.Now().UTC(),
		UpdatedAt:        clock.Now().UTC(),
	})

	userID := uuid.New().String()
	subID := uuid.New()
	periodStart := t0
	paidEnd := t0.Add(30 * 24 * time.Hour)

	suite.InsertSubscription(ctx, &models.Subscription{
		ID:                      subID,
		TenantSubjectID:         suite.ensureTenantSubject(ctx, userID),
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
	})

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
	_, err := rt.EntitlementService.PushNewEntitlement(ctx, entitlements.PushNewEntitlementParams{
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
		ID:              uuid.New(),
		TenantSubjectID: suite.ensureTenantSubject(ctx, userID),
		GrantedBy:       "admin",
		Reason:          "test",
		CreatedAt:       clock.Now().UTC(),
	}
	suite.InsertAdminGrant(ctx, adminGrant)

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
		_, _ = suite.Pool.Exec(ctx, "DELETE FROM billing.entitlements WHERE tenant_subject_id = $1", suite.ensureTenantSubject(ctx, userID))
		_, _ = suite.Pool.Exec(ctx, "DELETE FROM billing.payments WHERE id = $1", payment.ID)
		_, _ = suite.Pool.Exec(ctx, "DELETE FROM billing.admin_grants WHERE id = $1", adminGrant.ID)
		_, _ = suite.Pool.Exec(ctx, "DELETE FROM billing.subscriptions WHERE id = $1", subID)
		_, _ = suite.Pool.Exec(ctx, "DELETE FROM billing.prices WHERE id = $1", priceID)
		_, _ = suite.Pool.Exec(ctx, "DELETE FROM billing.products WHERE id = $1", productID)
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

func TestEntitlementSoftDeleteExcludedFromIsEntitled(t *testing.T) {
	suite := setupTestSuite(t)
	ctx := context.Background()

	userID := uuid.New().String()
	entName := "soft_delete_test_entitlement"
	now := time.Now().UTC()

	// Insert an entitlement window that is currently active.
	ent := &models.Entitlement{
		ID:              uuid.New(),
		TenantSubjectID: suite.ensureTenantSubject(ctx, userID),
		Entitlement:     entName,
		StartAt:         now.Add(-1 * time.Hour),
		EndAt:           nil, // active indefinitely
		SourceType:      models.EntitlementSourceAdmin,
		SourceID:        ptrUUID(uuid.New()),
		CreatedAt:       now,
		UpdatedAt:       now,
	}
	suite.InsertEntitlement(ctx, ent)

	require.NotNil(t, suite.App)
	require.NotNil(t, suite.App.Runtime)
	require.NotNil(t, suite.App.Runtime.DB)
	r := repo.NewEntitlementRepo(suite.App.Runtime.DB)

	ok, err := r.IsEntitled(ctx, userID, entName, now)
	require.NoError(t, err)
	require.True(t, ok, "entitlement should be active before soft delete")

	// Soft-delete the row (the bun-era model used a soft_delete tag; emulate it
	// by setting deleted_at directly).
	_, err = suite.Pool.Exec(ctx, "UPDATE billing.entitlements SET deleted_at = now() WHERE id = $1", ent.ID)
	require.NoError(t, err)

	ok, err = r.IsEntitled(ctx, userID, entName, now)
	require.NoError(t, err)
	require.False(t, ok, "soft-deleted entitlement should not be considered active")
}

func ptrUUID(v uuid.UUID) *uuid.UUID { return &v }

func TestEntitlements_RevokeExistingEntitlement_DropsAccessImmediately(t *testing.T) {
	suite := setupTestSuite(t)
	rt := suite.App.Runtime
	require.NotNil(t, rt)
	require.NotNil(t, rt.EntitlementService)

	ctx := context.Background()
	baseNow := time.Now().UTC().Truncate(time.Second)
	t0 := baseNow.Add(-30 * 24 * time.Hour)
	clock := suite.SetMockClock(t0)
	require.IsType(t, &clockwork.FakeClock{}, clock)

	userID := uuid.New().String()
	subID := uuid.New()
	notBefore := clock.Now().UTC()
	endAt := clock.Now().UTC().Add(30 * 24 * time.Hour)

	_, err := rt.EntitlementService.PushNewEntitlement(ctx, entitlements.PushNewEntitlementParams{
		UserID:      userID,
		Entitlement: "premium",
		NotBefore:   &notBefore,
		EndAt:       &endAt,
		SourceType:  models.EntitlementSourceSubscription,
		SourceID:    subID,
	})
	require.NoError(t, err)

	ok, err := rt.EntitlementService.IsEntitled(ctx, userID, "premium", clock.Now().UTC().Add(time.Second))
	require.NoError(t, err)
	require.True(t, ok)

	st := models.EntitlementSourceSubscription
	sid := subID
	require.NoError(t, rt.EntitlementService.RevokeExistingEntitlement(ctx, entitlements.RevokeExistingEntitlementParams{
		UserID:      userID,
		Entitlement: "premium",
		SourceType:  &st,
		SourceID:    &sid,
		Reason:      models.EntitlementRevokeChargeback,
	}))

	ok, err = rt.EntitlementService.IsEntitled(ctx, userID, "premium", clock.Now().UTC().Add(time.Second))
	require.NoError(t, err)
	require.False(t, ok)
}
