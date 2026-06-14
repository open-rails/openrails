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

	ctx := dbtest.WithTestTenant(context.Background())

	baseNow := time.Now().UTC().Truncate(time.Second)
	t0 := baseNow.Add(-120 * 24 * time.Hour)
	clock := suite.SetMockClock(t0)
	require.IsType(t, &clockwork.FakeClock{}, clock)

	// Seed a product with two entitlements.
	productID := uuid.New()
	priceID := uuid.New()
	billingDays := 30

	suite.InsertProduct(ctx, &models.Product{
		ID:          productID,
		Slug:        "test_nmi_multi_" + uuid.New().String()[:8],
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
		Processors: map[string]map[string]string{
			string(models.ProcessorMobius): {
				models.ProcessorKeyPlanID: "plan_test_999",
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
		Processor:           models.ProcessorMobius,
		PeriodStart:         periodStart,
		CurrentPeriodEndsAt: &paidEnd,
		PaymentMethodID:     &pm.ID,
		ProcessorSubID:      "sub_" + uuid.New().String()[:8],
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
		Processor:      models.ProcessorMobius,
		SubscriptionID: &sub.ID,
		FailureReason:  &failReason,
	}))

	// Monthly billing cycle -> progressive retry gaps (#359): +2d after the
	// initial failure, then +3d after the second.
	// First retry attempt: fail via mock, should append grace up to next_retry_at.
	mock.ShouldFail = true
	clock.Advance(subscriptions.DunningNextRetryIn(30, 1))

	worker := &riverjobs.DunningWorker{
		DB:                 rt.DB,
		Config:             suite.Config,
		Clock:              clock,
		NMIClients:         rt.NMIClients,
		IdempotencyService: rt.IdempotencyService,
		EventLogService:    rt.EventLogService,
	}
	require.NoError(t, worker.Work(ctx, &river.Job[riverjobs.DunningArgs]{}))

	// Second retry attempt: succeed via mock, should clear grace and push the next paid window.
	mock.ShouldFail = false
	clock.Advance(subscriptions.DunningNextRetryIn(30, 2))
	require.NoError(t, worker.Work(ctx, &river.Job[riverjobs.DunningArgs]{}))

	for _, entName := range []string{"premium", "extra"} {
		ok, err := rt.EntitlementService.IsEntitled(ctx, userID, entName, clock.Now().UTC().Add(time.Second))
		require.NoError(t, err)
		require.True(t, ok)
	}
}

func TestEntitlementsDunningStateMachine_NMI_TerminalFailureRevokesGrace(t *testing.T) {
	suite, mock := SetupSuiteWithMockNMI(t)
	rt := suite.App.Runtime
	require.NotNil(t, rt)

	ctx := dbtest.WithTestTenant(context.Background())
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
		Processor:           models.ProcessorMobius,
		PeriodStart:         periodStart,
		CurrentPeriodEndsAt: &paidEnd,
		PaymentMethodID:     &pm.ID,
		ProcessorSubID:      "sub_" + uuid.New().String()[:8],
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
		Processor:      models.ProcessorMobius,
		SubscriptionID: &sub.ID,
		FailureReason:  &failReason,
	}))

	mock.ShouldFail = true
	worker := &riverjobs.DunningWorker{
		DB:                 rt.DB,
		Config:             suite.Config,
		Clock:              clock,
		NMIClients:         rt.NMIClients,
		IdempotencyService: rt.IdempotencyService,
		EventLogService:    rt.EventLogService,
	}

	// Drive retries until the subscription is cancelled (monthly schedule,
	// #359: 5 failures total, progressive gaps of at most 4d). Advancing by
	// the largest gap each pass guarantees the next retry is due.
	maxDunningFailures := subscriptions.DunningMaxFailures(30)
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
