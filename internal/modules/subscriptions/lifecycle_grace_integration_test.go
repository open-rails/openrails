//go:build integration

package subscriptions

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/internal/modules/entitlements"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// graceTestFixture provisions a product (+entitlements spec) and price.
type graceTestFixture struct {
	dbi       *db.DB
	pool      *pgxpool.Pool
	q         *gen.Queries
	lifecycle *SubscriptionLifecycleService
	productID uuid.UUID
	priceID   uuid.UUID
	userID    string
}

func newGraceTestFixture(t *testing.T, billingDays int32) *graceTestFixture {
	t.Helper()
	dsn := dbtest.SharedPostgresDSN(t)
	ctx := context.Background()
	dbi := dbtest.OpenAppDB(t, dsn)
	pool := dbi.Pool()
	q := gen.New(pool)
	now := time.Now().UTC().Truncate(time.Second)

	productID, priceID := uuid.New(), uuid.New()
	userID := uuid.New().String()

	description := "Grace test"
	_, err := q.CreateProduct(ctx, gen.CreateProductParams{
		ID: productID, Slug: "grace_product_" + uuid.New().String(), DisplayName: "Grace Product",
		Description: &description, Status: string(models.CatalogStatusActive),
		EntitlementsSpec: []byte(`{"premium": null}`),
		CreatedAt:        now, UpdatedAt: now,
	})
	require.NoError(t, err)
	_, err = q.CreatePrice(ctx, gen.CreatePriceParams{
		ID: priceID, ProductID: productID, Amount: 999, Currency: "usd",
		Status: string(models.CatalogStatusActive), BillingCycleDays: &billingDays, CreatedAt: now, UpdatedAt: now,
	})
	require.NoError(t, err)
	dbtest.EnsureTenantSubjectIDPgx(ctx, t, pool, userID)

	priceSvc := catalog.NewPriceService(dbi)
	productSvc := catalog.NewProductService(dbi)
	entitlementSvc := entitlements.NewEntitlementService(dbi, nil)
	notifSvc := NewNotificationService(dbi, nil)
	paymentSvc := payments.NewPaymentService(dbi, nil)
	lifecycle := NewSubscriptionLifecycleService(dbi, productSvc, priceSvc, entitlementSvc, notifSvc, paymentSvc, nil, nil)

	f := &graceTestFixture{dbi: dbi, pool: pool, q: q, lifecycle: lifecycle, productID: productID, priceID: priceID, userID: userID}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM billing.entitlements WHERE source_id IN (SELECT id FROM billing.subscriptions WHERE product_id = $1)", productID)
		_, _ = pool.Exec(ctx, "DELETE FROM billing.payments WHERE price_id = $1", priceID)
		_, _ = pool.Exec(ctx, "DELETE FROM billing.notification_queue WHERE tenant_subject_id IN (SELECT id FROM billing.tenant_subjects WHERE id::text = $1)", userID)
		_, _ = pool.Exec(ctx, "DELETE FROM billing.subscriptions WHERE product_id = $1", productID)
		_, _ = pool.Exec(ctx, "DELETE FROM billing.prices WHERE id = $1", priceID)
		_, _ = pool.Exec(ctx, "DELETE FROM billing.products WHERE id = $1", productID)
	})
	return f
}

type entWindow struct {
	StartAt    time.Time
	EndAt      *time.Time
	SourceType string
	RevokedAt  *time.Time
	DeletedAt  *time.Time
}

func (f *graceTestFixture) windows(t *testing.T, subID uuid.UUID, sourceType string) []entWindow {
	t.Helper()
	rows, err := f.pool.Query(context.Background(),
		`SELECT start_at, end_at, source_type, revoked_at, deleted_at
		 FROM billing.entitlements WHERE source_id = $1 AND source_type = $2 ORDER BY start_at`, subID, sourceType)
	require.NoError(t, err)
	defer rows.Close()
	var out []entWindow
	for rows.Next() {
		var w entWindow
		require.NoError(t, rows.Scan(&w.StartAt, &w.EndAt, &w.SourceType, &w.RevokedAt, &w.DeletedAt))
		out = append(out, w)
	}
	return out
}

// TestGraceWindow_CreateAndRenew_NoGap (#368): activation pre-appends a
// trailing grace window starting EXACTLY at the paid period end; renewal
// revokes the old grace, pushes the next paid window, and pre-appends the
// next grace — no access gap at any boundary.
func TestGraceWindow_CreateAndRenew_NoGap(t *testing.T) {
	f := newGraceTestFixture(t, 30)
	ctx := context.Background()

	procSubID := "sub_grace_" + uuid.New().String()
	sub, err := f.lifecycle.CreateMembership(ctx, &CreateMembershipParams{
		UserID:                  f.userID,
		PriceID:                 f.priceID,
		Processor:               models.ProcessorMobius,
		ProcessorSubscriptionID: &procSubID,
		TransactionID:           "txn_create_" + uuid.New().String(),
	})
	require.NoError(t, err)

	paid := f.windows(t, sub.ID, "subscription")
	require.Len(t, paid, 1)
	require.NotNil(t, paid[0].EndAt)

	grace := f.windows(t, sub.ID, "grace")
	require.Len(t, grace, 1, "activation must pre-append one trailing grace window")
	assert.True(t, grace[0].StartAt.Equal(*paid[0].EndAt), "no-gap property: grace start must equal paid end (got paid end %s, grace start %s)", paid[0].EndAt, grace[0].StartAt)
	require.NotNil(t, grace[0].EndAt)
	assert.Equal(t, GraceSlack(30), grace[0].EndAt.Sub(grace[0].StartAt), "monthly cycle grace is the 48h cap")
	assert.Nil(t, grace[0].RevokedAt)
	assert.Nil(t, grace[0].DeletedAt)

	// Idempotence: re-pushing the same grace (e.g. a replayed webhook) lands
	// in the covered branch — still exactly one live grace window.
	require.NoError(t, f.lifecycle.RenewMembership(ctx, &RenewMembershipParams{
		Processor:               models.ProcessorMobius,
		ProcessorSubscriptionID: procSubID,
		TransactionID:           "txn_renew_" + uuid.New().String(),
	}))

	paid = f.windows(t, sub.ID, "subscription")
	require.Len(t, paid, 2, "renewal appends the next paid window")
	grace = f.windows(t, sub.ID, "grace")
	var live []entWindow
	for _, g := range grace {
		if g.RevokedAt == nil && g.DeletedAt == nil {
			live = append(live, g)
		}
	}
	require.Len(t, live, 1, "renewal must revoke/delete the old grace and pre-append exactly one new one")
	require.NotNil(t, paid[1].EndAt)
	assert.True(t, live[0].StartAt.Equal(*paid[1].EndAt), "new grace must trail the NEW paid window")
}

// TestGraceWindow_UserCancelDeletesFutureGrace (#368): a deliberate
// period-end cancellation forfeits silence generosity — the scheduled grace
// window is deleted at cancel time; paid access until period end is kept.
func TestGraceWindow_UserCancelDeletesFutureGrace(t *testing.T) {
	f := newGraceTestFixture(t, 30)
	ctx := context.Background()

	procSubID := "sub_cancel_" + uuid.New().String()
	sub, err := f.lifecycle.CreateMembership(ctx, &CreateMembershipParams{
		UserID:                  f.userID,
		PriceID:                 f.priceID,
		Processor:               models.ProcessorMobius,
		ProcessorSubscriptionID: &procSubID,
		TransactionID:           "txn_create_" + uuid.New().String(),
	})
	require.NoError(t, err)
	require.Len(t, f.windows(t, sub.ID, "grace"), 1)

	require.NoError(t, f.lifecycle.CancelMembership(ctx, &CancelMembershipParams{
		SubscriptionID: &sub.ID,
		CancelType:     models.CancelTypeUser,
		RevokeAccess:   false,
	}))

	// Paid window survives (access until period end as the user expects)...
	paid := f.windows(t, sub.ID, "subscription")
	require.Len(t, paid, 1)
	assert.Nil(t, paid[0].RevokedAt, "period-end cancel must not revoke the paid window")
	assert.Nil(t, paid[0].DeletedAt)

	// ...but the scheduled grace is gone: no generosity for explicit cancel.
	for _, g := range f.windows(t, sub.ID, "grace") {
		assert.True(t, g.RevokedAt != nil || g.DeletedAt != nil, "scheduled grace must be deleted on user cancel")
	}
}

// TestGraceWindow_DailyCycleSlackCap (#368): daily cycles get half a day of
// grace, not the 48h cap.
func TestGraceWindow_DailyCycleSlackCap(t *testing.T) {
	f := newGraceTestFixture(t, 1)
	ctx := context.Background()

	procSubID := "sub_daily_" + uuid.New().String()
	sub, err := f.lifecycle.CreateMembership(ctx, &CreateMembershipParams{
		UserID:                  f.userID,
		PriceID:                 f.priceID,
		Processor:               models.ProcessorMobius,
		ProcessorSubscriptionID: &procSubID,
		TransactionID:           "txn_create_" + uuid.New().String(),
	})
	require.NoError(t, err)

	grace := f.windows(t, sub.ID, "grace")
	require.Len(t, grace, 1)
	require.NotNil(t, grace[0].EndAt)
	assert.Equal(t, 12*time.Hour, grace[0].EndAt.Sub(grace[0].StartAt), "daily cycle gets 12h grace")
}

// TestGraceWindow_IneligibleProcessorGetsNone (#368): CCBill keeps its own
// retry-driven grace; activation must not pre-append a renewal grace window.
func TestGraceWindow_IneligibleProcessorGetsNone(t *testing.T) {
	f := newGraceTestFixture(t, 30)
	ctx := context.Background()

	procSubID := "sub_ccbill_" + uuid.New().String()
	sub, err := f.lifecycle.CreateMembership(ctx, &CreateMembershipParams{
		UserID:                  f.userID,
		PriceID:                 f.priceID,
		Processor:               models.ProcessorCCBill,
		ProcessorSubscriptionID: &procSubID,
		TransactionID:           "txn_create_" + uuid.New().String(),
	})
	require.NoError(t, err)

	assert.Len(t, f.windows(t, sub.ID, "subscription"), 1)
	assert.Empty(t, f.windows(t, sub.ID, "grace"), "ccbill must not get a pre-appended renewal grace window")
}

// TestGraceWindow_StalePeriodGetsNoResurrectionGrace (#368): a renewal whose
// period end + slack is already in the past (months-stale import shapes)
// must not mint a grace window.
func TestGraceWindow_StalePeriodGetsNoResurrectionGrace(t *testing.T) {
	f := newGraceTestFixture(t, 30)
	ctx := context.Background()

	procSubID := "sub_stale_" + uuid.New().String()
	staleStart := time.Now().UTC().Add(-120 * 24 * time.Hour)
	staleEnd := staleStart.Add(30 * 24 * time.Hour) // ended ~90 days ago
	sub, err := f.lifecycle.CreateMembership(ctx, &CreateMembershipParams{
		UserID:                  f.userID,
		PriceID:                 f.priceID,
		Processor:               models.ProcessorMobius,
		ProcessorSubscriptionID: &procSubID,
		CurrentPeriodStartsAt:   &staleStart,
		CurrentPeriodEndsAt:     &staleEnd,
		TransactionID:           "txn_create_" + uuid.New().String(),
	})
	require.NoError(t, err)

	assert.Empty(t, f.windows(t, sub.ID, "subscription"), "an already-elapsed period grants no paid window")
	assert.Empty(t, f.windows(t, sub.ID, "grace"), "a months-stale period end must not get resurrection grace")

	// A stale renewal (months-stale backfill repair walking one cycle
	// forward: -90d -> -60d) advances the lifecycle + records the payment but
	// still mints no windows — no resurrection access, no resurrection grace.
	require.NoError(t, f.lifecycle.RenewMembership(ctx, &RenewMembershipParams{
		Processor:               models.ProcessorMobius,
		ProcessorSubscriptionID: procSubID,
		TransactionID:           "txn_stale_renew_" + uuid.New().String(),
	}))
	assert.Empty(t, f.windows(t, sub.ID, "subscription"), "a stale renewal grants no paid window")
	assert.Empty(t, f.windows(t, sub.ID, "grace"), "a stale renewal mints no grace")

	var periodEnd time.Time
	require.NoError(t, f.pool.QueryRow(ctx, `SELECT current_period_ends_at FROM billing.subscriptions WHERE id = $1`, sub.ID).Scan(&periodEnd))
	assert.WithinDuration(t, staleEnd.Add(30*24*time.Hour), periodEnd, 2*time.Second, "stale renewal still advances the period one cycle")
}
