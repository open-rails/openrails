//go:build integration

package subscriptions

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/internal/modules/collection"
	"github.com/open-rails/openrails/internal/modules/entitlements"
	"github.com/open-rails/openrails/internal/modules/grants"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// #691 fail-open entitlements: an auto-renew subscription's entitlement window
// is STANDING (end_at NULL) from creation and is closed ONLY by proven events —
// user cancel (advance-written closure at period end), terminal dunning,
// provider-confirmed death. Silence (lost webhooks, converge down, provider
// outage) can never remove a paying user's access.

type failopenFixture struct {
	dbi       *db.DB
	pool      *pgxpool.Pool
	q         *gen.Queries
	lifecycle *SubscriptionLifecycleService
	entSvc    *entitlements.EntitlementService
	pspID     uuid.UUID
	productID uuid.UUID
	priceID   uuid.UUID
	userID    string
	ent       string
}

// newFailopenFixture provisions a product with one entitlement and a price.
// autoRenew=false models a bounded (rental/one-off duration) price.
func newFailopenFixture(t *testing.T, billingHours int32, autoRenew bool) *failopenFixture {
	t.Helper()
	dbi := dbtest.OpenMerchantDB(t, dbtest.TestMerchantID.UUID())
	pool := dbi.Pool()
	// or#893: these fixtures drive the lifecycle service directly, so they must
	// arrive in the shape every production caller does — checkout's stampPSP,
	// the intent runner and the webhook plane all pin the routed PSP on ctx
	// before any provider-bound row is written.
	pspID := dbtest.EnsureTestPSP(context.Background(), t, pool, dbtest.TestMerchantID.UUID(), string(models.RailNMI))
	ctx := db.WithPSPID(dbtest.WithTestMerchant(context.Background()), pspID)
	q := dbtest.Queries(pool)
	now := time.Now().UTC().Truncate(time.Second)

	productID, priceID := uuid.New(), uuid.New()
	userID := uuid.New().String()
	entName := "premium_failopen_" + uuid.New().String()[:8]

	description := "Failopen test"
	_, err := q.CreateProduct(ctx, gen.CreateProductParams{
		MerchantID: dbtest.TestMerchantID.UUID(), ID: productID, Key: "failopen_product_" + uuid.New().String(), DisplayName: "Failopen Product",
		Description: &description, Archived: false,
		EntitlementsSpec: []byte(`{"` + entName + `": null}`),
		CreatedAt:        now, UpdatedAt: now,
	})
	require.NoError(t, err)
	_, err = q.CreatePrice(ctx, gen.CreatePriceParams{
		MerchantID: dbtest.TestMerchantID.UUID(), ID: priceID, ProductID: productID, Amount: 9990000, Currency: "USD",
		Archived: false, AccessDurationHours: &billingHours, AutoRenew: autoRenew, CreatedAt: now, UpdatedAt: now,
	})
	require.NoError(t, err)
	dbtest.EnsureCustomerIDPgx(ctx, t, pool, userID)

	priceSvc := catalog.NewPriceService(dbi)
	productSvc := catalog.NewProductService(dbi)
	entitlementSvc := entitlements.NewEntitlementService(dbi, nil)
	notifSvc := NewNotificationService(dbi, nil)
	paymentSvc := payments.NewPaymentService(dbi, nil)
	lifecycle := NewSubscriptionLifecycleService(dbi, productSvc, priceSvc, entitlementSvc, notifSvc, paymentSvc, nil)

	f := &failopenFixture{dbi: dbi, pool: pool, q: q, lifecycle: lifecycle, entSvc: entitlementSvc, pspID: pspID, productID: productID, priceID: priceID, userID: userID, ent: entName}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM billing.entitlements WHERE source_id IN (SELECT id FROM billing.subscriptions WHERE product_id = $1)", productID)
		_, _ = pool.Exec(ctx, "DELETE FROM billing.grants WHERE customer_id::text = $1", userID)
		_, _ = pool.Exec(ctx, "DELETE FROM billing.payments WHERE price_id = $1", priceID)
		_, _ = pool.Exec(ctx, "DELETE FROM billing.notifications WHERE customer_id IN (SELECT id FROM billing.customers WHERE id::text = $1)", userID)
		_, _ = pool.Exec(ctx, "DELETE FROM billing.subscriptions WHERE product_id = $1", productID)
		_, _ = pool.Exec(ctx, "DELETE FROM billing.prices WHERE id = $1", priceID)
		_, _ = pool.Exec(ctx, "DELETE FROM billing.products WHERE id = $1", productID)
	})
	return f
}

type failopenWindow struct {
	ID         uuid.UUID
	StartAt    time.Time
	EndAt      *time.Time
	SourceType string
	RevokedAt  *time.Time
	DeletedAt  *time.Time
}

type failingLifecycleEntitlements struct {
	lifecycleEntitlementService
	listErr          error
	revokeErr        error
	revokeSourcesErr error
	boundErr         error
	resumeErr        error
}

func (f *failingLifecycleEntitlements) ListDistinctEntitlementNamesBySource(ctx context.Context, sourceType models.EntitlementSourceType, sourceID uuid.UUID) ([]string, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.lifecycleEntitlementService.ListDistinctEntitlementNamesBySource(ctx, sourceType, sourceID)
}

func (f *failingLifecycleEntitlements) RevokeExistingEntitlement(ctx context.Context, params entitlements.RevokeExistingEntitlementParams) error {
	if f.revokeErr != nil {
		return f.revokeErr
	}
	return f.lifecycleEntitlementService.RevokeExistingEntitlement(ctx, params)
}

func (f *failingLifecycleEntitlements) RevokeSourcesForSubscriptionAsOf(ctx context.Context, userID string, subscriptionID uuid.UUID, asOf time.Time, reason models.EntitlementRevokeReason, sourceTypes ...models.EntitlementSourceType) error {
	if f.revokeSourcesErr != nil {
		return f.revokeSourcesErr
	}
	return f.lifecycleEntitlementService.RevokeSourcesForSubscriptionAsOf(ctx, userID, subscriptionID, asOf, reason, sourceTypes...)
}

func (f *failingLifecycleEntitlements) BoundSubscriptionAccess(ctx context.Context, subscriptionID uuid.UUID, endAt time.Time) error {
	if f.boundErr != nil {
		return f.boundErr
	}
	return f.lifecycleEntitlementService.BoundSubscriptionAccess(ctx, subscriptionID, endAt)
}

func (f *failingLifecycleEntitlements) ResumeSubscriptionAccess(ctx context.Context, subscriptionID uuid.UUID) error {
	if f.resumeErr != nil {
		return f.resumeErr
	}
	return f.lifecycleEntitlementService.ResumeSubscriptionAccess(ctx, subscriptionID)
}

func (f *failopenFixture) failLifecycleEntitlements(failure failingLifecycleEntitlements) {
	f.lifecycle.entitlementServiceFactory = func(dbb *db.DB, c clockwork.Clock) lifecycleEntitlementService {
		failure.lifecycleEntitlementService = entitlements.NewEntitlementService(dbb, c)
		return &failure
	}
}

func (f *failopenFixture) windows(t *testing.T, subID uuid.UUID, sourceType string) []failopenWindow {
	t.Helper()
	rows, err := f.pool.Query(context.Background(),
		`SELECT id, start_at, end_at, source_type, revoked_at, deleted_at
		 FROM billing.entitlements WHERE source_id = $1 AND source_type = $2 ORDER BY start_at`, subID, sourceType)
	require.NoError(t, err)
	defer rows.Close()
	var out []failopenWindow
	for rows.Next() {
		var w failopenWindow
		require.NoError(t, rows.Scan(&w.ID, &w.StartAt, &w.EndAt, &w.SourceType, &w.RevokedAt, &w.DeletedAt))
		out = append(out, w)
	}
	return out
}

func (f *failopenFixture) entitledAt(t *testing.T, at time.Time) bool {
	t.Helper()
	ctx := f.ctx()
	ok, err := f.entSvc.IsEntitled(ctx, f.userID, f.ent, at)
	require.NoError(t, err)
	return ok
}

func (f *failopenFixture) loadSub(t *testing.T, id uuid.UUID) *models.Subscription {
	t.Helper()
	ctx := f.ctx()
	subSvc := NewSubscriptionService(f.dbi, catalog.NewPriceService(f.dbi), catalog.NewProductService(f.dbi), nil, nil, nil, nil)
	sub, err := subSvc.GetByID(ctx, id)
	require.NoError(t, err)
	return sub
}

func (f *failopenFixture) create(t *testing.T, rail models.Rail) (*models.Subscription, string) {
	t.Helper()
	ctx := f.ctx()
	procSubID := "sub_failopen_" + uuid.New().String()
	sub, err := f.lifecycle.CreateMembership(ctx, &CreateMembershipParams{
		UserID:             f.userID,
		PriceID:            f.priceID,
		Rail:               rail,
		RailSubscriptionID: &procSubID,
		TransactionID:      "txn_create_" + uuid.New().String(),
	})
	require.NoError(t, err)
	return sub, procSubID
}

func TestRenewMembership_DowngradeRevokeFailureRollsBack(t *testing.T) {
	f := newFailopenFixture(t, 30*24, true)
	ctx := f.ctx()
	sub, railSubID := f.create(t, models.RailNMI)
	original := f.loadSub(t, sub.ID)
	require.NotNil(t, original.CurrentPeriodEndsAt)

	now := time.Now().UTC().Truncate(time.Second)
	targetProductID, targetPriceID := uuid.New(), uuid.New()
	description := "Downgrade target"
	_, err := f.q.CreateProduct(ctx, gen.CreateProductParams{
		MerchantID:       dbtest.TestMerchantID.UUID(),
		ID:               targetProductID,
		Key:              "downgrade_target_" + uuid.NewString(),
		DisplayName:      "Downgrade Target",
		Description:      &description,
		EntitlementsSpec: []byte(`{}`),
		Archived:         false,
		CreatedAt:        now,
		UpdatedAt:        now,
	})
	require.NoError(t, err)
	cycleHours := int32(30 * 24)
	_, err = f.q.CreatePrice(ctx, gen.CreatePriceParams{
		MerchantID:          dbtest.TestMerchantID.UUID(),
		ID:                  targetPriceID,
		ProductID:           targetProductID,
		Amount:              4990000,
		Currency:            "USD",
		AccessDurationHours: &cycleHours,
		AutoRenew:           true,
		CreatedAt:           now,
		UpdatedAt:           now,
	})
	require.NoError(t, err)
	_, err = f.pool.Exec(ctx, `UPDATE billing.subscriptions SET scheduled_price_id=$2 WHERE id=$1`, sub.ID, targetPriceID)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = f.pool.Exec(ctx, `DELETE FROM billing.entitlements WHERE source_id=$1`, sub.ID)
		_, _ = f.pool.Exec(ctx, `DELETE FROM billing.payments WHERE subscription_id=$1`, sub.ID)
		_, _ = f.pool.Exec(ctx, `DELETE FROM billing.notifications WHERE customer_id=$1`, sub.CustomerID)
		_, _ = f.pool.Exec(ctx, `DELETE FROM billing.subscriptions WHERE id=$1`, sub.ID)
		_, _ = f.pool.Exec(ctx, `DELETE FROM billing.prices WHERE id=$1`, targetPriceID)
		_, _ = f.pool.Exec(ctx, `DELETE FROM billing.products WHERE id=$1`, targetProductID)
	})

	periodStart := original.CurrentPeriodEndsAt.UTC()
	periodEnd := periodStart.Add(30 * 24 * time.Hour)
	txnID := "downgrade_renewal_" + uuid.NewString()
	params := &RenewMembershipParams{
		Rail:                  models.RailNMI,
		RailSubscriptionID:    railSubID,
		TransactionID:         txnID,
		Amount:                4990000,
		AmountProvided:        true,
		Currency:              "USD",
		CurrentPeriodStartsAt: &periodStart,
		CurrentPeriodEndsAt:   &periodEnd,
	}

	injected := errors.New("injected downgrade revoke failure")
	f.failLifecycleEntitlements(failingLifecycleEntitlements{revokeErr: injected})
	err = f.lifecycle.RenewMembership(ctx, params)
	require.ErrorIs(t, err, injected)

	afterFailure := f.loadSub(t, sub.ID)
	require.Equal(t, original.PriceID, afterFailure.PriceID)
	require.Equal(t, original.ProductID, afterFailure.ProductID)
	require.Equal(t, &targetPriceID, afterFailure.ScheduledPriceID)
	require.Equal(t, original.CurrentPeriodEndsAt.UTC(), afterFailure.CurrentPeriodEndsAt.UTC())
	var paymentCount int
	require.NoError(t, f.pool.QueryRow(ctx, `SELECT count(*) FROM billing.payments WHERE transaction_id=$1`, txnID).Scan(&paymentCount))
	require.Zero(t, paymentCount, "the renewal payment marker must roll back with the failed downgrade effect")
	windows := f.windows(t, sub.ID, string(models.EntitlementSourceSubscription))
	require.Len(t, windows, 1)
	require.Nil(t, windows[0].RevokedAt, "the old-tier access must remain intact when the renewal rolls back")

	f.failLifecycleEntitlements(failingLifecycleEntitlements{})
	require.NoError(t, f.lifecycle.RenewMembership(ctx, params))
	afterRetry := f.loadSub(t, sub.ID)
	require.Equal(t, targetPriceID, afterRetry.PriceID)
	require.Equal(t, targetProductID, afterRetry.ProductID)
	require.Nil(t, afterRetry.ScheduledPriceID)
	require.Equal(t, periodEnd, afterRetry.CurrentPeriodEndsAt.UTC())
	require.NoError(t, f.pool.QueryRow(ctx, `SELECT count(*) FROM billing.payments WHERE transaction_id=$1`, txnID).Scan(&paymentCount))
	require.Equal(t, 1, paymentCount)
	windows = f.windows(t, sub.ID, string(models.EntitlementSourceSubscription))
	require.Len(t, windows, 1)
	require.NotNil(t, windows[0].RevokedAt, "the successful retry must remove the old-tier entitlement")
}

// One paid lifecycle covers silence, renewal/replay, grant projection and
// provider reconciliation without repeatedly rebuilding the same merchant.
func TestSubscriptionAccess_RenewalAndRecovery(t *testing.T) {
	f := newFailopenFixture(t, 30*24, true)
	ctx := f.ctx()
	sub, remoteID := f.create(t, models.RailNMI)
	farFuture := time.Now().UTC().Add(180 * 24 * time.Hour)
	standing := func() {
		t.Helper()
		windows := f.windows(t, sub.ID, "subscription")
		require.Len(t, windows, 1, "period grants must project to one standing window")
		require.Nil(t, windows[0].EndAt)
		require.Nil(t, windows[0].RevokedAt)
		require.Nil(t, windows[0].DeletedAt)
		assert.Empty(t, f.windows(t, sub.ID, "grace"))
		assert.True(t, f.entitledAt(t, farFuture), "webhook silence cannot terminate paid access")
	}
	grantCount := func(want int) {
		t.Helper()
		var count int
		require.NoError(t, f.pool.QueryRow(ctx, `SELECT count(*) FROM billing.grants
			WHERE source_type='subscription' AND source_id=$1 AND event='grant' AND ends_at IS NOT NULL`,
			sub.ID.String()).Scan(&count))
		assert.Equal(t, want, count, "each paid period records one bounded grant")
	}
	standing()
	grantCount(1)
	require.NoError(t, f.lifecycle.RenewMembership(ctx, &RenewMembershipParams{
		Rail: models.RailNMI, RailSubscriptionID: remoteID, TransactionID: "renew_" + uuid.NewString(),
	}))
	standing()
	grantCount(2)
	renewed := f.loadSub(t, sub.ID)
	require.NoError(t, f.lifecycle.RenewMembership(ctx, &RenewMembershipParams{
		Rail: models.RailNMI, RailSubscriptionID: remoteID, TransactionID: "replay_" + uuid.NewString(),
		CurrentPeriodStartsAt: renewed.CurrentPeriodStartsAt, CurrentPeriodEndsAt: renewed.CurrentPeriodEndsAt,
	}))
	grantCount(2)
	missing, err := f.q.ListLiveGrantsMissingEffects(ctx, gen.ListLiveGrantsMissingEffectsParams{
		MerchantID: dbtest.TestMerchantID.UUID(), CustomerID: &renewed.CustomerID,
	})
	require.NoError(t, err)
	assert.Empty(t, missing, "derive detection must agree with the standing projection")
	require.NoError(t, f.dbi.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		q := dbtest.Queries(tx)
		all, err := q.ListGrantsByCustomer(ctx, gen.ListGrantsByCustomerParams{
			MerchantID: dbtest.TestMerchantID.UUID(), CustomerID: renewed.CustomerID,
		})
		if err != nil {
			return err
		}
		ledger := grants.New(q, dbtest.TestMerchantID.UUID())
		for _, grant := range all {
			if err := ledger.MaterializeGrant(ctx, grant); err != nil {
				return err
			}
		}
		return nil
	}))
	standing()
	require.NoError(t, f.lifecycle.ApplyLocalUnknown(ctx, f.dbi, f.loadSub(t, sub.ID)))
	require.Equal(t, models.StatusUnknown, f.loadSub(t, sub.ID).Status)
	standing()
	newEnd := time.Now().UTC().Add(120 * 24 * time.Hour).Truncate(time.Second)
	require.NoError(t, f.lifecycle.ResolveUnknownSubscription(ctx, f.dbi, f.loadSub(t, sub.ID), ResolveRenewed, &newEnd, time.Now().UTC()))
	resolved := f.loadSub(t, sub.ID)
	require.Equal(t, models.StatusActive, resolved.Status)
	require.NotNil(t, resolved.CurrentPeriodEndsAt)
	assert.WithinDuration(t, newEnd, *resolved.CurrentPeriodEndsAt, time.Second)
	standing()
}

// Cancellation and either resume entrypoint must commit status and access
// together. Inject failure at the real transaction's entitlement boundary.
func TestSubscriptionAccess_CancelAndResume(t *testing.T) {
	for _, rail := range []models.Rail{models.RailNMI, models.RailStripe} {
		t.Run(string(rail), func(t *testing.T) {
			f := newFailopenFixture(t, 30*24, true)
			ctx := f.ctx()
			sub, remoteID := f.create(t, rail)
			created := f.loadSub(t, sub.ID)
			require.NotNil(t, created.CurrentPeriodEndsAt)
			periodEnd := created.CurrentPeriodEndsAt.UTC()
			cancel := &CancelMembershipParams{SubscriptionID: &sub.ID, CancelType: models.CancelTypeUser}
			injected := errors.New("access write failed")
			f.failLifecycleEntitlements(failingLifecycleEntitlements{boundErr: injected})
			require.ErrorIs(t, f.lifecycle.CancelMembership(ctx, cancel), injected)
			require.Equal(t, models.StatusActive, f.loadSub(t, sub.ID).Status)
			windows := f.windows(t, sub.ID, "subscription")
			require.Len(t, windows, 1)
			require.Nil(t, windows[0].EndAt, "failed cancel leaves standing access")
			f.lifecycle.entitlementServiceFactory = nil
			require.NoError(t, f.lifecycle.CancelMembership(ctx, cancel))
			windows = f.windows(t, sub.ID, "subscription")
			require.Len(t, windows, 1)
			require.Nil(t, windows[0].RevokedAt)
			require.NotNil(t, windows[0].EndAt)
			boundedAt := windows[0].EndAt.UTC()
			assert.WithinDuration(t, periodEnd, boundedAt, time.Second)
			assert.True(t, f.entitledAt(t, periodEnd.Add(-time.Minute)))
			assert.False(t, f.entitledAt(t, periodEnd.Add(time.Minute)))
			// Provider-owned NMI cancellation is not locally resumable; its
			// verified-provider reactivation uses the separate entrypoint below.
			if rail == models.RailStripe {
				f.failLifecycleEntitlements(failingLifecycleEntitlements{resumeErr: injected})
				_, err := f.lifecycle.ResumeMembership(ctx, &ResumeMembershipParams{SubscriptionID: sub.ID})
				require.ErrorIs(t, err, injected)
				cancelled := f.loadSub(t, sub.ID)
				require.Equal(t, models.StatusCancelled, cancelled.Status)
				require.NotNil(t, cancelled.CancelledAt)
				windows = f.windows(t, sub.ID, "subscription")
				require.Len(t, windows, 1)
				require.NotNil(t, windows[0].EndAt)
				require.Equal(t, boundedAt, windows[0].EndAt.UTC(), "resume failure rolls back the window")
				f.lifecycle.entitlementServiceFactory = nil
				resumed, err := f.lifecycle.ResumeMembership(ctx, &ResumeMembershipParams{SubscriptionID: sub.ID})
				require.NoError(t, err)
				require.Equal(t, models.StatusActive, resumed.Status)
				windows = f.windows(t, sub.ID, "subscription")
				require.Len(t, windows, 1)
				require.Nil(t, windows[0].EndAt)
				require.NoError(t, f.lifecycle.CancelMembership(ctx, cancel))
			}
			require.Equal(t, models.StatusCancelled, f.loadSub(t, sub.ID).Status)
			windows = f.windows(t, sub.ID, "subscription")
			require.Len(t, windows, 1)
			require.NotNil(t, windows[0].EndAt, "reactivation must reopen a bounded window")
			assert.Equal(t, boundedAt, windows[0].EndAt.UTC())
			_, err := f.lifecycle.ReactivateMembership(ctx, &ReactivateMembershipParams{
				Rail: rail, RailSubscriptionID: remoteID, CurrentPeriodEndsAt: created.CurrentPeriodEndsAt,
				AllowTerminalReactivation: true,
			})
			require.NoError(t, err)
			windows = f.windows(t, sub.ID, "subscription")
			require.Len(t, windows, 1)
			assert.Nil(t, windows[0].EndAt)
			assert.Nil(t, windows[0].RevokedAt)
			assert.Nil(t, windows[0].DeletedAt)
			assert.True(t, f.entitledAt(t, time.Now().UTC().Add(90*24*time.Hour)))
		})
	}
}

func TestSubscriptionAccess_ImmediateClosureIsAtomic(t *testing.T) {
	injected := errors.New("access write failed")
	for _, tc := range []struct {
		name    string
		failure failingLifecycleEntitlements
		close   func(*failopenFixture, uuid.UUID) error
	}{
		{"user", failingLifecycleEntitlements{revokeSourcesErr: injected}, func(f *failopenFixture, id uuid.UUID) error {
			return f.lifecycle.CancelMembership(f.ctx(), &CancelMembershipParams{SubscriptionID: &id, CancelType: models.CancelTypeUser, RevokeAccess: true})
		}},
		{"chargeback", failingLifecycleEntitlements{revokeSourcesErr: injected}, func(f *failopenFixture, id uuid.UUID) error {
			return f.lifecycle.CancelMembership(f.ctx(), &CancelMembershipParams{SubscriptionID: &id, CancelType: models.CancelTypeChargeback, RevokeAccess: true})
		}},
		{"expired", failingLifecycleEntitlements{revokeErr: injected}, func(f *failopenFixture, id uuid.UUID) error {
			return f.lifecycle.ExpireMembership(f.ctx(), id)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFailopenFixture(t, 30*24, true)
			sub, _ := f.create(t, models.RailNMI)
			farFuture := time.Now().UTC().Add(90 * 24 * time.Hour)
			f.failLifecycleEntitlements(tc.failure)
			require.ErrorIs(t, tc.close(f, sub.ID), injected)
			require.Equal(t, models.StatusActive, f.loadSub(t, sub.ID).Status)
			assert.True(t, f.entitledAt(t, farFuture))
			f.lifecycle.entitlementServiceFactory = nil
			require.NoError(t, tc.close(f, sub.ID))
			require.Equal(t, models.StatusCancelled, f.loadSub(t, sub.ID).Status)
			windows := f.windows(t, sub.ID, "subscription")
			require.NotEmpty(t, windows)
			for _, w := range windows {
				assert.True(t, w.RevokedAt != nil || w.DeletedAt != nil)
			}
			assert.False(t, f.entitledAt(t, time.Now().UTC().Add(time.Minute)))
			assert.False(t, f.entitledAt(t, farFuture))
		})
	}
}

func TestSubscriptionAccess_DunningExhaustion(t *testing.T) {
	for _, hours := range []int32{24, 30 * 24} {
		t.Run((time.Duration(hours) * time.Hour).String(), func(t *testing.T) {
			f := newFailopenFixture(t, hours, true)
			ctx := f.ctx()
			sub, _ := f.create(t, models.RailNMI)
			farFuture := time.Now().UTC().Add(90 * 24 * time.Hour)
			failure := &FailMembershipParams{Rail: models.RailNMI, SubscriptionID: &sub.ID, AttemptRecorded: true}
			maxFailures := collection.MaxFailures(int(hours))
			if hours == 24 {
				require.Equal(t, 1, maxFailures, "daily cycle must not inherit monthly retries")
			}
			for i := 1; i < maxFailures; i++ {
				require.NoError(t, f.lifecycle.FailMembership(ctx, failure))
				require.Equal(t, models.StatusPastDue, f.loadSub(t, sub.ID).Status)
				assert.True(t, f.entitledAt(t, farFuture))
				assert.Empty(t, f.windows(t, sub.ID, "grace"))
			}
			before := f.loadSub(t, sub.ID)
			injected := errors.New("entitlement listing failed")
			f.failLifecycleEntitlements(failingLifecycleEntitlements{listErr: injected})
			require.ErrorIs(t, f.lifecycle.FailMembership(ctx, failure), injected)
			require.Equal(t, before.Status, f.loadSub(t, sub.ID).Status)
			assert.True(t, f.entitledAt(t, farFuture))
			f.lifecycle.entitlementServiceFactory = nil
			require.NoError(t, f.lifecycle.FailMembership(ctx, failure))
			terminal := f.loadSub(t, sub.ID)
			require.Equal(t, models.StatusCancelled, terminal.Status)
			require.NotNil(t, terminal.CancelType)
			assert.Equal(t, models.CancelTypeExpired, *terminal.CancelType)
			assert.Nil(t, terminal.NextRetryAt)
			for _, w := range f.windows(t, sub.ID, "subscription") {
				assert.True(t, w.RevokedAt != nil || w.DeletedAt != nil || (w.EndAt != nil && !w.EndAt.After(time.Now().UTC())))
			}
			assert.False(t, f.entitledAt(t, farFuture))
		})
	}
}

// failopenDeferredDelete records ScheduleNMIDelete calls (#679 regression leg).
type failopenDeferredDelete struct{ scheduled []uuid.UUID }

func (d *failopenDeferredDelete) ScheduleNMIDelete(_ context.Context, _ string, subscriptionID uuid.UUID, _ time.Time) error {
	d.scheduled = append(d.scheduled, subscriptionID)
	return nil
}
func (d *failopenDeferredDelete) CancelNMIDelete(context.Context, string, uuid.UUID) error {
	return nil
}
func (d *failopenDeferredDelete) WithTx(pgx.Tx) DeferredDeleteScheduler { return d }

// TestFailOpen_ResolveCancelledRemoteAlive: provider-confirmed death closes
// access as-of provider truth AND still queues the #679 deferred remote delete
// (no regression).
func TestFailOpen_ResolveCancelledRemoteAlive(t *testing.T) {
	f := newFailopenFixture(t, 30*24, true)
	ctx := f.ctx()
	deferDelete := &failopenDeferredDelete{}
	f.lifecycle.SetDeferredDeleteScheduler(deferDelete)

	sub, _ := f.create(t, models.RailNMI)
	require.NoError(t, f.lifecycle.ApplyLocalUnknown(ctx, f.dbi, f.loadSub(t, sub.ID)))

	require.NoError(t, f.lifecycle.ResolveUnknownSubscription(ctx, f.dbi, f.loadSub(t, sub.ID), ResolveCancelledRemoteAlive, nil, time.Now().UTC()))

	terminal := f.loadSub(t, sub.ID)
	require.Equal(t, models.StatusCancelled, terminal.Status)
	assert.NotNil(t, terminal.DeletionScheduledAt, "#679: the deferred delete marker is written")
	assert.Contains(t, deferDelete.scheduled, sub.ID, "#679: the remote delete intent is still queued")

	for _, w := range f.windows(t, sub.ID, "subscription") {
		assert.True(t, w.RevokedAt != nil || w.DeletedAt != nil || (w.EndAt != nil && !w.EndAt.After(time.Now().UTC().Add(31*24*time.Hour))),
			"provider-confirmed death closes the window as-of provider truth")
	}
	assert.False(t, f.entitledAt(t, time.Now().UTC().Add(60*24*time.Hour)))
}

// TestFailOpen_BoundedPurchaseKeepsInterval: a NON-auto-renew (bounded duration)
// price keeps a bounded interval window that expires on its own.
func TestFailOpen_BoundedPurchaseKeepsInterval(t *testing.T) {
	f := newFailopenFixture(t, 7*24, false) // one-off 7-day rental price
	sub, _ := f.create(t, models.RailNMI)

	paid := f.windows(t, sub.ID, "subscription")
	require.Len(t, paid, 1)
	require.NotNil(t, paid[0].EndAt, "bounded purchases keep interval windows")
	assert.True(t, f.entitledAt(t, time.Now().UTC().Add(time.Hour)))
	assert.False(t, f.entitledAt(t, paid[0].EndAt.Add(time.Minute)), "the interval window expires on its own")
}

// ctx is the production context shape for a provider-bound write: the
// merchant, plus the PSP the caller routed to (or#893). The id is resolved, not
// assumed: EnsureTestPSP reuses whatever account this database already has on
// the rail.
func (f *failopenFixture) ctx() context.Context {
	return db.WithPSPID(dbtest.WithTestMerchant(context.Background()), f.pspID)
}
