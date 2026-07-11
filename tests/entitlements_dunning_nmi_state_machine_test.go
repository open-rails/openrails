//go:build integration

package tests

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/modules/entitlements"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	riverjobs "github.com/open-rails/openrails/internal/river"
	"github.com/riverqueue/river"
	"github.com/stretchr/testify/require"
)

func TestEntitlementsDunningStateMachine_NMI_SucceedsAfterRetries(t *testing.T) {
	suite, mock := SetupSuiteWithMockNMI(t)
	rt := suite.App.Runtime
	require.NotNil(t, rt)
	require.NotNil(t, rt.DB)
	require.NotNil(t, rt.IdempotencyService)

	ctx := dbtest.WithTestMerchant(context.Background())

	baseNow := time.Now().UTC().Truncate(time.Second)
	t0 := baseNow.Add(-120 * 24 * time.Hour)
	clock := suite.SetMockClock(t0)
	require.IsType(t, &clockwork.FakeClock{}, clock)

	// Seed a product with two entitlements.
	productID := uuid.New()
	priceID := uuid.New()
	billingDays := 720

	suite.InsertProduct(ctx, &models.Product{
		ID:          productID,
		Key:         "test_nmi_multi_" + uuid.New().String()[:8],
		DisplayName: "Test Product",
		Description: "Test",
		EntitlementsSpec: map[string]*int{
			"premium": nil,
			"extra":   nil,
		},
		Archived:  false,
		CreatedAt: clock.Now().UTC(),
		UpdatedAt: clock.Now().UTC(),
	})

	suite.InsertPrice(ctx, &models.Price{
		ID:                  priceID,
		ProductID:           productID,
		Archived:            false,
		Amount:              999,
		Currency:            "usd",
		AccessDurationHours: &billingDays, AutoRenew: true,
		Rails: map[string]map[string]string{
			string(models.RailNMI): {
				models.RailKeyRail:   string(models.RailNMI),
				models.RailKeyPlanID: "plan_test_999",
			},
		},
		CreatedAt: clock.Now().UTC(),
		UpdatedAt: clock.Now().UTC(),
	})

	userID := uuid.New().String()
	pm := suite.CreateTestPaymentMethod(userID)

	periodStart := t0
	paidEnd := t0.Add(30 * 24 * time.Hour)

	sub := suite.CreateTestSubscriptionWithOptions(SubscriptionOptions{
		UserID:              userID,
		PriceID:             priceID,
		Status:              models.StatusActive,
		Rail:                models.RailNMI,
		PeriodStart:         periodStart,
		CurrentPeriodEndsAt: &paidEnd,
		PaymentMethodID:     &pm.ID,
		RailSubID:           "sub_" + uuid.New().String()[:8],
	})

	// Initial paid windows for both entitlements.
	for _, entName := range []string{"premium", "extra"} {
		notBefore := periodStart.UTC()
		endAt := paidEnd.UTC()
		_, err := rt.EntitlementService.PushNewEntitlement(ctx, entitlements.PushNewEntitlementParams{
			UserID:      userID,
			Entitlement: entName,
			NotBefore:   &notBefore,
			EndAt:       &endAt,
			SourceType:  models.EntitlementSourceSubscription,
			SourceID:    sub.ID,
		})
		require.NoError(t, err)
	}

	t.Cleanup(func() {
		_, _ = suite.Pool.Exec(ctx, "DELETE FROM openrails.entitlements WHERE customer_id = $1", suite.ensureCustomer(ctx, userID))
		_, _ = suite.Pool.Exec(ctx, "DELETE FROM openrails.subscriptions WHERE id = $1", sub.ID)
		_, _ = suite.Pool.Exec(ctx, "DELETE FROM openrails.payment_methods WHERE id = $1", pm.ID)
		_, _ = suite.Pool.Exec(ctx, "DELETE FROM openrails.prices WHERE id = $1", priceID)
		_, _ = suite.Pool.Exec(ctx, "DELETE FROM openrails.products WHERE id = $1", productID)
	})

	// Move time to paid end and mark a failure (puts subscription into past_due and schedules next_retry_at).
	clock.Advance(paidEnd.Sub(clock.Now().UTC()))
	failReason := "declined"
	require.NoError(t, rt.SubscriptionLifecycleService.FailMembership(ctx, &subscriptions.FailMembershipParams{
		Rail:           models.RailNMI,
		SubscriptionID: &sub.ID,
		FailureReason:  &failReason,
	}))

	// Monthly billing cycle -> progressive retry gaps (#359): +2d after the
	// initial failure, then +3d after the second.
	// First retry attempt: fail via mock. #691: no grace machinery exists —
	// access rides the untouched standing window through the failed retry.
	mock.ShouldFail = true
	clock.Advance(subscriptions.DunningNextRetryIn(30*24, 1))

	worker := &riverjobs.DunningWorker{
		DB:                 rt.DB,
		Config:             suite.Config,
		Clock:              clock,
		NMIResolver:        rt.CollectionResolver,
		IdempotencyService: rt.IdempotencyService,
	}
	require.NoError(t, worker.Work(ctx, &river.Job[riverjobs.DunningArgs]{}))

	// Fail-open mid-dunning (#691): the failed retry must not have touched
	// access — entitled well past the missed paid end, with zero grace rows.
	for _, entName := range []string{"premium", "extra"} {
		ok, err := rt.EntitlementService.IsEntitled(ctx, userID, entName, clock.Now().UTC().Add(time.Second))
		require.NoError(t, err)
		require.True(t, ok, "access never lapses mid-dunning (%s)", entName)
	}
	graceRows := suite.Count(ctx, `
		SELECT COUNT(*) FROM openrails.entitlements
		WHERE source_type = $1 AND source_id = $2 AND deleted_at IS NULL`,
		string(models.EntitlementSourceGrace), sub.ID)
	require.Zero(t, graceRows, "#691: no grace windows are ever appended during NMI dunning")

	// Second retry attempt: succeed via mock — recovery records the renewal;
	// the standing window needs no extension.
	mock.ShouldFail = false
	clock.Advance(subscriptions.DunningNextRetryIn(30*24, 2))
	require.NoError(t, worker.Work(ctx, &river.Job[riverjobs.DunningArgs]{}))

	for _, entName := range []string{"premium", "extra"} {
		ok, err := rt.EntitlementService.IsEntitled(ctx, userID, entName, clock.Now().UTC().Add(time.Second))
		require.NoError(t, err)
		require.True(t, ok)
	}
}

// TestEntitlementsDunningStateMachine_NMI_TerminalFailure: the NMI ladder's
// certainty-gated terminal outcome — real retries exhaust the cadence schedule
// through the worker, and ONLY that proof closes access (#691/#664).
func TestEntitlementsDunningStateMachine_NMI_TerminalFailure(t *testing.T) {
	suite, mock := SetupSuiteWithMockNMI(t)
	rt := suite.App.Runtime
	require.NotNil(t, rt)

	ctx := dbtest.WithTestMerchant(context.Background())
	baseNow := time.Now().UTC().Truncate(time.Second)
	t0 := baseNow.Add(-120 * 24 * time.Hour)
	clock := suite.SetMockClock(t0)

	products := suite.SeedTieredProducts()
	priceID := products[0].Prices[0].ID

	userID := uuid.New().String()
	pm := suite.CreateTestPaymentMethod(userID)

	periodStart := t0
	paidEnd := t0.Add(30 * 24 * time.Hour)
	sub := suite.CreateTestSubscriptionWithOptions(SubscriptionOptions{
		UserID:              userID,
		PriceID:             priceID,
		Status:              models.StatusActive,
		Rail:                models.RailNMI,
		PeriodStart:         periodStart,
		CurrentPeriodEndsAt: &paidEnd,
		PaymentMethodID:     &pm.ID,
		RailSubID:           "sub_" + uuid.New().String()[:8],
	})

	// Minimal entitlement for this subscription.
	notBefore := periodStart.UTC()
	endAt := paidEnd.UTC()
	_, err := rt.EntitlementService.PushNewEntitlement(ctx, entitlements.PushNewEntitlementParams{
		UserID:      userID,
		Entitlement: "premium",
		NotBefore:   &notBefore,
		EndAt:       &endAt,
		SourceType:  models.EntitlementSourceSubscription,
		SourceID:    sub.ID,
	})
	require.NoError(t, err)

	t.Cleanup(func() {
		_, _ = suite.Pool.Exec(ctx, "DELETE FROM openrails.entitlements WHERE customer_id = $1", suite.ensureCustomer(ctx, userID))
		_, _ = suite.Pool.Exec(ctx, "DELETE FROM openrails.subscriptions WHERE id = $1", sub.ID)
		_, _ = suite.Pool.Exec(ctx, "DELETE FROM openrails.payment_methods WHERE id = $1", pm.ID)
	})

	clock.Advance(paidEnd.Sub(clock.Now().UTC()))
	failReason := "declined"
	require.NoError(t, rt.SubscriptionLifecycleService.FailMembership(ctx, &subscriptions.FailMembershipParams{
		Rail:           models.RailNMI,
		SubscriptionID: &sub.ID,
		FailureReason:  &failReason,
	}))

	mock.ShouldFail = true
	worker := &riverjobs.DunningWorker{
		DB:                 rt.DB,
		Config:             suite.Config,
		Clock:              clock,
		NMIResolver:        rt.CollectionResolver,
		IdempotencyService: rt.IdempotencyService,
	}

	// Drive retries until the subscription is cancelled (monthly schedule,
	// #359: 5 failures total, progressive gaps of at most 4d). Advancing by
	// the largest gap each pass guarantees the next retry is due.
	maxDunningFailures := subscriptions.DunningMaxFailures(30 * 24)
	for i := 0; i < maxDunningFailures+1; i++ {
		clock.Advance(4 * 24 * time.Hour)
		require.NoError(t, worker.Work(ctx, &river.Job[riverjobs.DunningArgs]{}))
		refreshed := suite.GetSubscription(sub.ID)
		if refreshed.Status == models.StatusCancelled {
			break
		}
	}

	// After terminal failure, access must be removed immediately.
	ok, err := rt.EntitlementService.IsEntitled(ctx, userID, "premium", clock.Now().UTC().Add(time.Second))
	require.NoError(t, err)
	require.False(t, ok)
}
