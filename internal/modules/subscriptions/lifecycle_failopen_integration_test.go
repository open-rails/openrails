//go:build integration

package subscriptions

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/modules/catalog"
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
	productID uuid.UUID
	priceID   uuid.UUID
	userID    string
	ent       string
}

// newFailopenFixture provisions a product with one entitlement and a price.
// autoRenew=false models a bounded (rental/one-off duration) price.
func newFailopenFixture(t *testing.T, billingHours int32, autoRenew bool) *failopenFixture {
	t.Helper()
	dsn := dbtest.SharedPostgresDSN(t)
	ctx := dbtest.WithTestMerchant(context.Background())
	dbi := dbtest.OpenAppDB(t, dsn)
	pool := dbi.Pool()
	q := gen.New(pool)
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
		MerchantID: dbtest.TestMerchantID.UUID(), ID: priceID, ProductID: productID, Amount: 9990000, Currency: "usd",
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

	f := &failopenFixture{dbi: dbi, pool: pool, q: q, lifecycle: lifecycle, entSvc: entitlementSvc, productID: productID, priceID: priceID, userID: userID, ent: entName}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.entitlements WHERE source_id IN (SELECT id FROM openrails.subscriptions WHERE product_id = $1)", productID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.grants WHERE customer_id::text = $1", userID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.payments WHERE price_id = $1", priceID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.notification_queue WHERE customer_id IN (SELECT id FROM openrails.customers WHERE id::text = $1)", userID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.subscriptions WHERE product_id = $1", productID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.prices WHERE id = $1", priceID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.products WHERE id = $1", productID)
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

func (f *failopenFixture) windows(t *testing.T, subID uuid.UUID, sourceType string) []failopenWindow {
	t.Helper()
	rows, err := f.pool.Query(context.Background(),
		`SELECT id, start_at, end_at, source_type, revoked_at, deleted_at
		 FROM openrails.entitlements WHERE source_id = $1 AND source_type = $2 ORDER BY start_at`, subID, sourceType)
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
	ctx := dbtest.WithTestMerchant(context.Background())
	ok, err := f.entSvc.IsEntitled(ctx, f.userID, f.ent, at)
	require.NoError(t, err)
	return ok
}

func (f *failopenFixture) loadSub(t *testing.T, id uuid.UUID) *models.Subscription {
	t.Helper()
	ctx := dbtest.WithTestMerchant(context.Background())
	subSvc := NewSubscriptionService(f.dbi, catalog.NewPriceService(f.dbi), catalog.NewProductService(f.dbi), nil, nil, nil, nil)
	sub, err := subSvc.GetByID(ctx, id)
	require.NoError(t, err)
	return sub
}

func (f *failopenFixture) create(t *testing.T, rail models.Rail) (*models.Subscription, string) {
	t.Helper()
	ctx := dbtest.WithTestMerchant(context.Background())
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

// TestFailOpen_WebhookSilence: an active auto-renew sub gets ONE standing
// window (end_at NULL); the period end passing with total webhook silence
// changes nothing; parking `unknown` (what the converge sweep does) changes
// nothing; a provider-pull renewal resolution advances paid-through with access
// continuous throughout. Also covers "converge never runs at all": before any
// sweep the window is already open-ended, so access holds trivially.
func TestFailOpen_WebhookSilence(t *testing.T) {
	f := newFailopenFixture(t, 30*24, true)
	ctx := dbtest.WithTestMerchant(context.Background())
	sub, _ := f.create(t, models.RailNMI)

	paid := f.windows(t, sub.ID, "subscription")
	require.Len(t, paid, 1)
	assert.Nil(t, paid[0].EndAt, "auto-renew activation must project a STANDING window (end_at NULL)")
	assert.Empty(t, f.windows(t, sub.ID, "grace"), "no #368 grace window is pre-appended anymore")

	// The grant ledger stays bounded: the activation grant carries the paid period.
	var grantEnds *time.Time
	require.NoError(t, f.pool.QueryRow(ctx,
		`SELECT ends_at FROM openrails.grants WHERE source_type='subscription' AND source_id=$1 AND event='grant'`,
		sub.ID.String()).Scan(&grantEnds))
	require.NotNil(t, grantEnds, "the activation grant is bounded to the paid period")

	// Webhook silence: no renewal arrives, the period end passes (queried via
	// the at-parameter — no machinery ran). Access is STILL live.
	farFuture := time.Now().UTC().Add(90 * 24 * time.Hour)
	assert.True(t, f.entitledAt(t, farFuture), "access must survive total webhook silence past period end")

	// The converge sweep parks the sub `unknown` (no revoke on a guess) —
	// access still live.
	require.NoError(t, f.lifecycle.ApplyLocalUnknown(ctx, f.dbi, f.loadSub(t, sub.ID)))
	require.Equal(t, models.StatusUnknown, f.loadSub(t, sub.ID).Status)
	assert.True(t, f.entitledAt(t, farFuture), "parking unknown must not touch access")

	// Provider pull resolves: verified renewal. Paid-through advances; access continuous.
	newEnd := time.Now().UTC().Add(60 * 24 * time.Hour).Truncate(time.Second)
	require.NoError(t, f.lifecycle.ResolveUnknownSubscription(ctx, f.dbi, f.loadSub(t, sub.ID), ResolveRenewed, &newEnd, time.Now().UTC()))
	resolved := f.loadSub(t, sub.ID)
	require.Equal(t, models.StatusActive, resolved.Status)
	require.NotNil(t, resolved.CurrentPeriodEndsAt)
	assert.WithinDuration(t, newEnd, *resolved.CurrentPeriodEndsAt, time.Second, "paid-through fact advanced")
	assert.True(t, f.entitledAt(t, farFuture), "access continuous across silence -> unknown -> renewed")

	// Still exactly one live subscription window — the standing one.
	live := 0
	for _, w := range f.windows(t, sub.ID, "subscription") {
		if w.RevokedAt == nil && w.DeletedAt == nil {
			live++
			assert.Nil(t, w.EndAt)
		}
	}
	assert.Equal(t, 1, live, "one standing window, no per-period appends")
}

// TestFailOpen_RenewalRecordsPeriodGrantNotWindow: a renewal extends the
// paid-through FACT and appends a bounded per-period grant, but the projection
// stays one standing window (no per-period window appends, GIST intact).
// Renewal replay appends nothing (idempotent). The derive-1 missing-effects
// detection agrees the per-period grants are satisfied by the standing window.
func TestFailOpen_RenewalRecordsPeriodGrantNotWindow(t *testing.T) {
	f := newFailopenFixture(t, 30*24, true)
	ctx := dbtest.WithTestMerchant(context.Background())
	sub, procSubID := f.create(t, models.RailNMI)

	require.NoError(t, f.lifecycle.RenewMembership(ctx, &RenewMembershipParams{
		Rail:               models.RailNMI,
		RailSubscriptionID: procSubID,
		TransactionID:      "txn_renew_" + uuid.New().String(),
	}))

	var liveWindows []failopenWindow
	for _, w := range f.windows(t, sub.ID, "subscription") {
		if w.RevokedAt == nil && w.DeletedAt == nil {
			liveWindows = append(liveWindows, w)
		}
	}
	require.Len(t, liveWindows, 1, "renewal must NOT append a second window")
	assert.Nil(t, liveWindows[0].EndAt, "the standing window stays open")

	var grantCount int
	require.NoError(t, f.pool.QueryRow(ctx,
		`SELECT count(*) FROM openrails.grants WHERE source_type='subscription' AND source_id=$1 AND event='grant' AND ends_at IS NOT NULL`,
		sub.ID.String()).Scan(&grantCount))
	assert.Equal(t, 2, grantCount, "activation + renewal each record a bounded per-period grant")

	// Replay the same renewal period: covered branch + grant idempotency.
	renewed := f.loadSub(t, sub.ID)
	require.NoError(t, f.lifecycle.RenewMembership(ctx, &RenewMembershipParams{
		Rail:                  models.RailNMI,
		RailSubscriptionID:    procSubID,
		CurrentPeriodStartsAt: renewed.CurrentPeriodStartsAt,
		CurrentPeriodEndsAt:   renewed.CurrentPeriodEndsAt,
		TransactionID:         "txn_renew_replay_" + uuid.New().String(),
	}))
	require.NoError(t, f.pool.QueryRow(ctx,
		`SELECT count(*) FROM openrails.grants WHERE source_type='subscription' AND source_id=$1 AND event='grant' AND ends_at IS NOT NULL`,
		sub.ID.String()).Scan(&grantCount))
	assert.Equal(t, 2, grantCount, "a replayed period appends no grant")

	// DERIVE parity: no per-period grant is reported as missing its effect —
	// the standing window satisfies them (detection mirrors MaterializeGrant).
	customerID := f.loadSub(t, sub.ID).CustomerID
	missing, err := f.q.ListLiveGrantsMissingEffects(dbtest.WithTestMerchant(context.Background()), gen.ListLiveGrantsMissingEffectsParams{
		MerchantID: dbtest.TestMerchantID.UUID(), CustomerID: &customerID,
	})
	require.NoError(t, err)
	assert.Empty(t, missing, "per-period grants are satisfied by the standing window")
}

// TestFailOpen_UserCancelClosesAtPeriodEnd: a user cancel writes the closure IN
// ADVANCE at the known period end — the end is already on disk, so a dead
// system cannot extend a cancelled sub. Access ends exactly at period end.
func TestFailOpen_UserCancelClosesAtPeriodEnd(t *testing.T) {
	f := newFailopenFixture(t, 30*24, true)
	ctx := dbtest.WithTestMerchant(context.Background())
	sub, _ := f.create(t, models.RailNMI)
	created := f.loadSub(t, sub.ID)
	require.NotNil(t, created.CurrentPeriodEndsAt)
	periodEnd := created.CurrentPeriodEndsAt.UTC()

	require.NoError(t, f.lifecycle.CancelMembership(ctx, &CancelMembershipParams{
		SubscriptionID: &sub.ID,
		CancelType:     models.CancelTypeUser,
		RevokeAccess:   false,
	}))

	paid := f.windows(t, sub.ID, "subscription")
	require.Len(t, paid, 1)
	require.Nil(t, paid[0].RevokedAt, "period-end cancel keeps the paid runway")
	require.NotNil(t, paid[0].EndAt, "closure must be written on disk at cancel time")
	assert.WithinDuration(t, periodEnd, paid[0].EndAt.UTC(), time.Second, "closure = the known period end")

	assert.True(t, f.entitledAt(t, periodEnd.Add(-time.Minute)), "runway access until period end")
	assert.False(t, f.entitledAt(t, periodEnd.Add(time.Minute)), "access ends exactly at the closure")
}

// TestFailOpen_ImmediateCancelRevokesNow: an immediate revoke closes access now.
func TestFailOpen_ImmediateCancelRevokesNow(t *testing.T) {
	f := newFailopenFixture(t, 30*24, true)
	ctx := dbtest.WithTestMerchant(context.Background())
	sub, _ := f.create(t, models.RailNMI)

	require.NoError(t, f.lifecycle.CancelMembership(ctx, &CancelMembershipParams{
		SubscriptionID: &sub.ID,
		CancelType:     models.CancelTypeUser,
		RevokeAccess:   true,
	}))
	for _, w := range f.windows(t, sub.ID, "subscription") {
		assert.True(t, w.RevokedAt != nil || w.DeletedAt != nil, "immediate cancel revokes the standing window")
	}
	assert.False(t, f.entitledAt(t, time.Now().UTC().Add(time.Minute)))
}

// TestFailOpen_ReactivateRestoresStanding: resuming a period-end cancel re-opens
// the standing window (the advance-written closure is undone).
func TestFailOpen_ReactivateRestoresStanding(t *testing.T) {
	f := newFailopenFixture(t, 30*24, true)
	ctx := dbtest.WithTestMerchant(context.Background())
	sub, procSubID := f.create(t, models.RailNMI)
	created := f.loadSub(t, sub.ID)

	require.NoError(t, f.lifecycle.CancelMembership(ctx, &CancelMembershipParams{
		SubscriptionID: &sub.ID,
		CancelType:     models.CancelTypeUser,
		RevokeAccess:   false,
	}))
	paid := f.windows(t, sub.ID, "subscription")
	require.Len(t, paid, 1)
	require.NotNil(t, paid[0].EndAt)

	_, err := f.lifecycle.ReactivateMembership(ctx, &ReactivateMembershipParams{
		Rail:                      models.RailNMI,
		RailSubscriptionID:        procSubID,
		CurrentPeriodEndsAt:       created.CurrentPeriodEndsAt,
		AllowTerminalReactivation: true,
	})
	require.NoError(t, err)

	var live []failopenWindow
	for _, w := range f.windows(t, sub.ID, "subscription") {
		if w.RevokedAt == nil && w.DeletedAt == nil {
			live = append(live, w)
		}
	}
	require.Len(t, live, 1)
	assert.Nil(t, live[0].EndAt, "resume must restore the STANDING window")
	assert.True(t, f.entitledAt(t, time.Now().UTC().Add(90*24*time.Hour)))
}

// TestFailOpen_DunningExhaustionClosesAccess: real recorded attempts through
// FailMembership; the schedule's terminal failure cancels and revokes as-of the
// policy instant. Mid-dunning (past_due) access stays live with NO grace
// windows minted.
func TestFailOpen_DunningExhaustionClosesAccess(t *testing.T) {
	f := newFailopenFixture(t, 30*24, true)
	ctx := dbtest.WithTestMerchant(context.Background())
	sub, _ := f.create(t, models.RailNMI)
	farFuture := time.Now().UTC().Add(90 * 24 * time.Hour)

	maxFailures := DunningMaxFailures(30 * 24) // monthly schedule: 5 recorded failures
	for i := 1; i < maxFailures; i++ {
		require.NoError(t, f.lifecycle.FailMembership(ctx, &FailMembershipParams{
			Rail:           models.RailNMI,
			SubscriptionID: &sub.ID,
		}))
		mid := f.loadSub(t, sub.ID)
		require.Equal(t, models.StatusPastDue, mid.Status, "attempt %d keeps dunning alive", i)
		assert.True(t, f.entitledAt(t, farFuture), "access intact mid-dunning (attempt %d)", i)
		assert.Empty(t, f.windows(t, sub.ID, "grace"), "no grace windows are minted during dunning")
	}

	// The final recorded failure is terminal: cancel + revoke as-of now.
	require.NoError(t, f.lifecycle.FailMembership(ctx, &FailMembershipParams{
		Rail:           models.RailNMI,
		SubscriptionID: &sub.ID,
	}))
	terminal := f.loadSub(t, sub.ID)
	require.Equal(t, models.StatusCancelled, terminal.Status)
	for _, w := range f.windows(t, sub.ID, "subscription") {
		assert.True(t, w.RevokedAt != nil || w.DeletedAt != nil || (w.EndAt != nil && !w.EndAt.After(time.Now().UTC())),
			"exhausted dunning must close the standing window")
	}
	assert.False(t, f.entitledAt(t, farFuture))
}

// TestFailOpen_DailyCycleFirstFailureTerminal pins the 0-retry-tier PLUMBING
// (#359): the price's billing cycle (24h here) actually reaches the failure
// policy — the FIRST recorded failure is terminal (no retry ever scheduled, no
// silent monthly fallback) and closes the standing window. The tier tables
// themselves are unit-pinned in dunning_test.go; this proves cycleHours flows
// from the price into FailMembership. Ports the essence of the deleted tests/
// dunning_worker FailMembership cadence tests (#694).
func TestFailOpen_DailyCycleFirstFailureTerminal(t *testing.T) {
	f := newFailopenFixture(t, 24, true)
	ctx := dbtest.WithTestMerchant(context.Background())
	sub, _ := f.create(t, models.RailNMI)

	reason := "rebill declined"
	require.NoError(t, f.lifecycle.FailMembership(ctx, &FailMembershipParams{
		Rail:           models.RailNMI,
		SubscriptionID: &sub.ID,
		FailureReason:  &reason,
	}))

	terminal := f.loadSub(t, sub.ID)
	require.Equal(t, models.StatusCancelled, terminal.Status, "first failure on a 1d cycle is terminal (0-retry tier)")
	require.NotNil(t, terminal.CancelType)
	assert.Equal(t, models.CancelTypeExpired, *terminal.CancelType)
	assert.Nil(t, terminal.NextRetryAt, "the 0-retry tier never schedules a retry")
	assert.False(t, f.entitledAt(t, time.Now().UTC().Add(time.Minute)), "terminal dunning closes the standing window")
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
	ctx := dbtest.WithTestMerchant(context.Background())
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

// TestFailOpen_MaterializeReplayIsIdempotent: re-deriving every recorded grant
// must not mint overlapping bounded windows next to the standing one.
func TestFailOpen_MaterializeReplayIsIdempotent(t *testing.T) {
	f := newFailopenFixture(t, 30*24, true)
	ctx := dbtest.WithTestMerchant(context.Background())
	sub, procSubID := f.create(t, models.RailNMI)
	require.NoError(t, f.lifecycle.RenewMembership(ctx, &RenewMembershipParams{
		Rail:               models.RailNMI,
		RailSubscriptionID: procSubID,
		TransactionID:      "txn_renew_" + uuid.New().String(),
	}))

	countWindows := func() int {
		n := 0
		for _, w := range f.windows(t, sub.ID, "subscription") {
			if w.DeletedAt == nil {
				n++
			}
		}
		return n
	}
	before := countWindows()
	customerID := f.loadSub(t, sub.ID).CustomerID

	// Replay derive-2 over the full grant log (what a converge repair does).
	require.NoError(t, f.dbi.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		q := gen.New(tx)
		gl := grants.New(q, dbtest.TestMerchantID.UUID())
		all, err := q.ListGrantsByCustomer(ctx, gen.ListGrantsByCustomerParams{
			MerchantID: dbtest.TestMerchantID.UUID(), CustomerID: customerID,
		})
		if err != nil {
			return err
		}
		for i := range all {
			if err := gl.MaterializeGrant(ctx, all[i]); err != nil {
				return err
			}
		}
		return nil
	}))

	assert.Equal(t, before, countWindows(), "derive replay must not create windows next to the standing one")
}
