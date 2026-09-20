//go:build integration

package tests

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/modules/collection"
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

	ctx := suite.MerchantCtx()

	baseNow := time.Now().UTC().Truncate(time.Second)
	// Keep the recovered period current for the production inline convergence
	// pass, which observes wall time after the worker commits.
	t0 := baseNow.Add(-35 * 24 * time.Hour)
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
		Amount:              9990000,
		Currency:            "USD",
		AccessDurationHours: &billingDays, AutoRenew: true,
		PSPLinks: map[string]map[string]string{
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

	sub.EntitlementsSpecSnapshot = map[string]*int{"premium": nil, "extra": nil}
	require.NoError(t, subscriptions.NewSubscriptionRepo(rt.DB).Update(ctx, sub))
	seedDunningProviderObligation(mock, sub, pm, paidEnd, 9990000)

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
		_, _ = suite.Pool.Exec(ctx, "DELETE FROM billing.entitlements WHERE customer_id = $1", suite.ensureCustomer(ctx, userID))
		_, _ = suite.Pool.Exec(ctx, "DELETE FROM billing.subscriptions WHERE id = $1", sub.ID)
		_, _ = suite.Pool.Exec(ctx, "DELETE FROM billing.payment_methods WHERE id = $1", pm.ID)
		_, _ = suite.Pool.Exec(ctx, "DELETE FROM billing.prices WHERE id = $1", priceID)
		_, _ = suite.Pool.Exec(ctx, "DELETE FROM billing.products WHERE id = $1", productID)
	})

	// Move time to paid end and mark a failure (puts subscription into past_due and schedules next_retry_at).
	clock.Advance(paidEnd.Sub(clock.Now().UTC()))
	failReason := "declined"
	require.NoError(t, rt.SubscriptionLifecycleService.FailMembership(ctx, &subscriptions.FailMembershipParams{
		Rail:           models.RailNMI,
		SubscriptionID: &sub.ID,
		FailureReason:  &failReason,
		// A real declined charge attempt underlies this failure (#840): that is
		// what lets the schedule's exhaustion count as a certainty leg.
		AttemptRecorded: true,
	}))

	// Monthly billing cycle -> progressive retry gaps (#359): +2d after the
	// initial failure, then +3d after the second.
	// First retry attempt: fail via mock. #691: no grace machinery exists —
	// access rides the untouched standing window through the failed retry.
	mock.ShouldFail = true
	clock.Advance(collection.NextRetryIn(30*24, 1))

	worker := &riverjobs.DunningWorker{
		DB:                 rt.DB,
		Config:             suite.Config,
		Clock:              clock,
		NMIResolver:        rt.CollectionResolver,
		IdempotencyService: rt.IdempotencyService,
		DeferDelete:        rt.DeferredDeletes,
	}
	require.NoError(t, worker.Work(suite.WorkerCtx(), &river.Job[riverjobs.DunningArgs]{}))

	// Fail-open mid-dunning (#691): the failed retry must not have touched
	// access — entitled well past the missed paid end, with zero grace rows.
	for _, entName := range []string{"premium", "extra"} {
		ok, err := rt.EntitlementService.IsEntitled(ctx, userID, entName, clock.Now().UTC().Add(time.Second))
		require.NoError(t, err)
		require.True(t, ok, "access never lapses mid-dunning (%s)", entName)
	}
	graceRows := suite.Count(ctx, `
		SELECT COUNT(*) FROM billing.entitlements
		WHERE source_type = $1 AND source_id = $2 AND deleted_at IS NULL`,
		string(models.EntitlementSourceGrace), sub.ID)
	require.Zero(t, graceRows, "#691: no grace windows are ever appended during NMI dunning")

	// Second retry attempt: succeed via mock — recovery records the renewal;
	// the standing window needs no extension.
	mock.ShouldFail = false
	next := suite.GetSubscription(sub.ID).NextRetryAt
	require.NotNil(t, next)
	clock.Advance(next.Sub(clock.Now().UTC()) + time.Second)
	require.NoError(t, worker.Work(suite.WorkerCtx(), &river.Job[riverjobs.DunningArgs]{}))

	refreshed := suite.GetSubscription(sub.ID)
	require.Equal(t, models.StatusActive, refreshed.Status)
	require.True(t, refreshed.CurrentPeriodEndsAt.After(paidEnd), "confirmed retry advances this paid period")
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

	ctx := suite.MerchantCtx()
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

	seedDunningProviderObligation(mock, sub, pm, paidEnd, products[0].Prices[0].Amount)

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
		_, _ = suite.Pool.Exec(ctx, "DELETE FROM billing.entitlements WHERE customer_id = $1", suite.ensureCustomer(ctx, userID))
		_, _ = suite.Pool.Exec(ctx, "DELETE FROM billing.subscriptions WHERE id = $1", sub.ID)
		_, _ = suite.Pool.Exec(ctx, "DELETE FROM billing.payment_methods WHERE id = $1", pm.ID)
	})

	clock.Advance(paidEnd.Sub(clock.Now().UTC()))
	failReason := "declined"
	require.NoError(t, rt.SubscriptionLifecycleService.FailMembership(ctx, &subscriptions.FailMembershipParams{
		Rail:           models.RailNMI,
		SubscriptionID: &sub.ID,
		FailureReason:  &failReason,
		// A real declined charge attempt underlies this failure (#840): that is
		// what lets the schedule's exhaustion count as a certainty leg.
		AttemptRecorded: true,
	}))

	// or#836: the terminal outcome this test is about is a DESTRUCTIVE action,
	// and destructive actions ship OFF — a fresh deployment cancels nothing and
	// revokes nothing until an operator arms them. This test asserts what a
	// live, REVIEWED deployment does, so it puts itself in that state; the
	// safe default has its own tests.
	dbtest.ArmDestructiveActions(ctx, t, dbtest.TestMerchantID.UUID())
	t.Cleanup(func() {
		_, _ = suite.MerchantPool().Exec(context.Background(),
			`UPDATE billing.destructive_action_switch SET enabled = false`)
		_, _ = suite.MerchantPool().Exec(context.Background(),
			`DELETE FROM billing.merchant_destructive_policy WHERE merchant_id = $1`,
			dbtest.TestMerchantID.UUID())
	})

	// or#870: only a NAMED certainty terminates. 261 "Stop All Recurring
	// Payments" is the issuer withdrawing the recurring mandate — bucket 3, the
	// one decline class that may end a subscription. A generic 300 is bucket 1
	// and retries forever, and exhausting the ladder on its own is no longer a
	// death certificate (or#839/or#840).
	mock.ShouldFail = true
	mock.FailCode = "261"
	worker := &riverjobs.DunningWorker{
		DB:                 rt.DB,
		Config:             suite.Config,
		Clock:              clock,
		NMIResolver:        rt.CollectionResolver,
		IdempotencyService: rt.IdempotencyService,
		DeferDelete:        rt.DeferredDeletes,
	}

	// Drive retries until the subscription is cancelled (monthly schedule,
	// #359: 5 failures total, progressive gaps of at most 4d). Advancing by
	// the largest gap each pass guarantees the next retry is due.
	maxDunningFailures := collection.MaxFailures(30 * 24)
	for i := 0; i < maxDunningFailures+1; i++ {
		clock.Advance(4 * 24 * time.Hour)
		require.NoError(t, worker.Work(suite.WorkerCtx(), &river.Job[riverjobs.DunningArgs]{}))
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

func seedDunningProviderObligation(mock *MockNMIServer, sub *models.Subscription, pm *models.PaymentMethod, paidEnd time.Time, amount int64) {
	mock.rebillMu.Lock()
	defer mock.rebillMu.Unlock()
	if mock.rebillObligations == nil {
		mock.rebillObligations = map[string]map[string]any{}
	}
	decimal := fmt.Sprintf("%d.%02d", amount/1000000, (amount%1000000)/10000)
	mock.rebillObligations[sub.RailSubscriptionID] = map[string]any{"id": sub.RailSubscriptionID, "amount": decimal, "customer_vault_id": pm.RailCustomerRef, "delayed_condition": "active", "paused_subscription": "0", "next_billing_date": paidEnd.Add(30 * 24 * time.Hour).UTC().Format("2006-01-02"), "plan": map[string]any{"id": "plan-dunning", "plan_amount": decimal, "plan_payments": "0", "day_frequency": "30"}}
}
