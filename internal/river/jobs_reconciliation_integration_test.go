//go:build integration

package riverjobs_test

import (
	"context"
	"database/sql"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/credits"
	riverjobs "github.com/open-rails/openrails/internal/river"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
	"github.com/riverqueue/river"
	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"
)

// recordingReconSink captures every ReconciliationEvent the worker emits.
type recordingReconSink struct {
	mu     sync.Mutex
	events []credits.ReconciliationEvent
}

func (r *recordingReconSink) Handle(_ context.Context, ev credits.ReconciliationEvent) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, ev)
	return nil
}

func (r *recordingReconSink) byKind(k credits.ReconciliationEventKind) []credits.ReconciliationEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []credits.ReconciliationEvent
	for _, e := range r.events {
		if e.Kind == k {
			out = append(out, e)
		}
	}
	return out
}

func reconTestDB(t *testing.T) (*bun.DB, *db.DB) {
	t.Helper()
	dsn := os.Getenv("OPENRAILS_TEST_DB_URL")
	if dsn == "" {
		t.Skip("set OPENRAILS_TEST_DB_URL to run integration tests")
	}
	sqlDB := sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dsn)))
	t.Cleanup(func() { _ = sqlDB.Close() })
	bunDB := bun.NewDB(sqlDB, pgdialect.New())
	models.RegisterModels(bunDB)

	ctx := context.Background()
	require.NoError(t, bunDB.PingContext(ctx))

	var hasTable bool
	require.NoError(t, bunDB.NewSelect().
		ColumnExpr("EXISTS (SELECT 1 FROM information_schema.tables WHERE table_schema='billing' AND table_name='reconciliation_events')").
		Scan(ctx, &hasTable))
	if !hasTable {
		t.Skip("migration 053 not applied; run migrations before integration tests")
	}

	dbi, err := db.NewWithBun(bunDB)
	require.NoError(t, err)
	return bunDB, dbi
}

// seedCreditType + balance helpers scope a unique credit type per test so runs
// never collide and cleanup is trivial.
func seedReconCreditType(t *testing.T, ctx context.Context, bunDB *bun.DB, now time.Time) uuid.UUID {
	t.Helper()
	id := uuid.New()
	_, err := bunDB.NewInsert().Model(&models.CreditType{
		ID:            id,
		Name:          "recon_" + uuid.NewString(),
		DisplayName:   "Recon Test",
		Unit:          "units",
		DecimalPlaces: 0,
		IsActive:      true,
		CreatedAt:     now,
	}).Exec(ctx)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = bunDB.NewDelete().Model((*models.ReconciliationEvent)(nil)).Where("credit_type_id = ?", id).Exec(ctx)
		_, _ = bunDB.NewDelete().Model((*models.CreditTransaction)(nil)).Where("credit_type_id = ?", id).Exec(ctx)
		_, _ = bunDB.NewDelete().Model((*models.UserCreditBalance)(nil)).Where("credit_type_id = ?", id).Exec(ctx)
		_, _ = bunDB.NewDelete().Model((*models.CreditType)(nil)).Where("id = ?", id).Exec(ctx)
	})
	return id
}

func openReconCount(t *testing.T, ctx context.Context, bunDB *bun.DB, ctID uuid.UUID, kind models.ReconciliationKind) int {
	t.Helper()
	n, err := bunDB.NewSelect().Model((*models.ReconciliationEvent)(nil)).
		Where("credit_type_id = ? AND kind = ? AND resolved_at IS NULL", ctID, kind).
		Count(ctx)
	require.NoError(t, err)
	return n
}

// TestReconciliation_ExpiredOrphanHoldDetectedAndCleaned: an active hold past
// its expiry is detected, an orphan_hold event is recorded + emitted, the hold
// is expired, and held_balance is restored. Re-running finds nothing new
// (idempotent).
func TestReconciliation_ExpiredOrphanHoldDetectedAndCleaned(t *testing.T) {
	bunDB, dbi := reconTestDB(t)
	ctx := context.Background()
	clock := clockwork.NewFakeClockAt(time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC))
	now := clock.Now().UTC()
	ctID := seedReconCreditType(t, ctx, bunDB, now)

	tenant := uuid.New()
	owner := uuid.New()
	auth := int64(70)

	// Owner has a balance with held_balance reflecting the active hold.
	_, err := bunDB.NewInsert().Model(&models.UserCreditBalance{
		ID: uuidutil.NewV7(), TenantID: tenant, OwnerID: owner, UserID: owner.String(),
		CreditTypeID: ctID, Balance: 1000, HeldBalance: auth, CreatedAt: now, UpdatedAt: now,
	}).Exec(ctx)
	require.NoError(t, err)

	// An ACTIVE hold whose expires_at is already in the past = orphan.
	expired := now.Add(-1 * time.Hour)
	hold := &models.CreditTransaction{
		ID: uuidutil.NewV7(), TenantID: tenant, OwnerID: owner, UserID: owner.String(),
		CreditTypeID: ctID, Amount: 0, TransactionType: "hold", Status: "active",
		Authorized: &auth, Source: "usage", ExpiresAt: &expired, CreatedAt: now, UpdatedAt: now,
	}
	_, err = bunDB.NewInsert().Model(hold).Exec(ctx)
	require.NoError(t, err)

	sink := &recordingReconSink{}
	w := &riverjobs.BillingReconciliationWorker{DB: dbi, Clock: clock, Sink: sink, AutoRemediate: true}

	require.NoError(t, w.Work(ctx, &river.Job[riverjobs.BillingReconciliationArgs]{Args: riverjobs.BillingReconciliationArgs{}}))

	// Orphan detected + emitted + persisted.
	require.Len(t, sink.byKind(credits.ReconOrphanHold), 1)
	require.Equal(t, 1, openReconCount(t, ctx, bunDB, ctID, models.ReconciliationOrphanHold))

	// Hold expired.
	var status string
	require.NoError(t, bunDB.NewSelect().Model((*models.CreditTransaction)(nil)).
		Column("status").Where("id = ?", hold.ID).Scan(ctx, &status))
	require.Equal(t, "expired", status)

	// held_balance restored to 0.
	var held int64
	require.NoError(t, bunDB.NewSelect().Model((*models.UserCreditBalance)(nil)).
		Column("held_balance").Where("owner_id = ? AND credit_type_id = ?", owner, ctID).Scan(ctx, &held))
	require.Equal(t, int64(0), held)

	// Idempotent re-run: no new orphan emitted (the hold is no longer active).
	sink2 := &recordingReconSink{}
	w2 := &riverjobs.BillingReconciliationWorker{DB: dbi, Clock: clock, Sink: sink2, AutoRemediate: true}
	require.NoError(t, w2.Work(ctx, &river.Job[riverjobs.BillingReconciliationArgs]{Args: riverjobs.BillingReconciliationArgs{}}))
	require.Len(t, sink2.byKind(credits.ReconOrphanHold), 0)
}

// TestReconciliation_HeldBalanceDriftDetectedAndCorrected: a balance whose
// denormalized held_balance disagrees with the sum of active holds is detected
// and corrected.
func TestReconciliation_HeldBalanceDriftDetectedAndCorrected(t *testing.T) {
	bunDB, dbi := reconTestDB(t)
	ctx := context.Background()
	clock := clockwork.NewFakeClockAt(time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC))
	now := clock.Now().UTC()
	ctID := seedReconCreditType(t, ctx, bunDB, now)

	tenant := uuid.New()
	owner := uuid.New()
	auth := int64(30)

	// Active hold reserves 30, but denormalized held_balance says 90 (drift of 60).
	future := now.Add(2 * time.Hour)
	_, err := bunDB.NewInsert().Model(&models.CreditTransaction{
		ID: uuidutil.NewV7(), TenantID: tenant, OwnerID: owner, UserID: owner.String(),
		CreditTypeID: ctID, Amount: 0, TransactionType: "hold", Status: "active",
		Authorized: &auth, Source: "usage", ExpiresAt: &future, CreatedAt: now, UpdatedAt: now,
	}).Exec(ctx)
	require.NoError(t, err)
	_, err = bunDB.NewInsert().Model(&models.UserCreditBalance{
		ID: uuidutil.NewV7(), TenantID: tenant, OwnerID: owner, UserID: owner.String(),
		CreditTypeID: ctID, Balance: 1000, HeldBalance: 90, CreatedAt: now, UpdatedAt: now,
	}).Exec(ctx)
	require.NoError(t, err)

	sink := &recordingReconSink{}
	w := &riverjobs.BillingReconciliationWorker{DB: dbi, Clock: clock, Sink: sink, AutoRemediate: true}
	require.NoError(t, w.Work(ctx, &river.Job[riverjobs.BillingReconciliationArgs]{Args: riverjobs.BillingReconciliationArgs{}}))

	drift := sink.byKind(credits.ReconHeldBalanceDrift)
	require.Len(t, drift, 1)
	require.Equal(t, int64(30), drift[0].Expected)
	require.Equal(t, int64(90), drift[0].Observed)
	require.Equal(t, 1, openReconCount(t, ctx, bunDB, ctID, models.ReconciliationHeldBalanceDrift))

	// Corrected to the ledger-derived 30.
	var held int64
	require.NoError(t, bunDB.NewSelect().Model((*models.UserCreditBalance)(nil)).
		Column("held_balance").Where("owner_id = ? AND credit_type_id = ?", owner, ctID).Scan(ctx, &held))
	require.Equal(t, int64(30), held)

	// Idempotent: re-run finds no new drift.
	sink2 := &recordingReconSink{}
	w2 := &riverjobs.BillingReconciliationWorker{DB: dbi, Clock: clock, Sink: sink2, AutoRemediate: true}
	require.NoError(t, w2.Work(ctx, &river.Job[riverjobs.BillingReconciliationArgs]{Args: riverjobs.BillingReconciliationArgs{}}))
	require.Len(t, sink2.byKind(credits.ReconHeldBalanceDrift), 0)
}

// TestReconciliation_BalanceDriftReportedNotCorrected: a balance whose
// denormalized available balance disagrees with the ledger is REPORTED but the
// denormalized value is left untouched (alert-only).
func TestReconciliation_BalanceDriftReportedNotCorrected(t *testing.T) {
	bunDB, dbi := reconTestDB(t)
	ctx := context.Background()
	clock := clockwork.NewFakeClockAt(time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC))
	now := clock.Now().UTC()
	ctID := seedReconCreditType(t, ctx, bunDB, now)

	tenant := uuid.New()
	owner := uuid.New()

	// Ledger: a +100 deposit and a -40 withdrawal => ledger_sum 60.
	dep := int64(100)
	_, err := bunDB.NewInsert().Model(&models.CreditTransaction{
		ID: uuidutil.NewV7(), TenantID: tenant, OwnerID: owner, UserID: owner.String(),
		CreditTypeID: ctID, Amount: 100, BalanceAfter: &dep, TransactionType: "deposit", Status: "posted",
		Source: "purchase", CreatedAt: now, UpdatedAt: now,
	}).Exec(ctx)
	require.NoError(t, err)
	src := "req-withdraw-1"
	_, err = bunDB.NewInsert().Model(&models.CreditTransaction{
		ID: uuidutil.NewV7(), TenantID: tenant, OwnerID: owner, UserID: owner.String(),
		CreditTypeID: ctID, Amount: -40, TransactionType: "withdrawal", Status: "posted",
		Source: "usage", SourceID: &src, CreatedAt: now, UpdatedAt: now,
	}).Exec(ctx)
	require.NoError(t, err)

	// Denormalized balance says 200 (wrong; ledger says 60).
	_, err = bunDB.NewInsert().Model(&models.UserCreditBalance{
		ID: uuidutil.NewV7(), TenantID: tenant, OwnerID: owner, UserID: owner.String(),
		CreditTypeID: ctID, Balance: 200, HeldBalance: 0, CreatedAt: now, UpdatedAt: now,
	}).Exec(ctx)
	require.NoError(t, err)

	sink := &recordingReconSink{}
	w := &riverjobs.BillingReconciliationWorker{DB: dbi, Clock: clock, Sink: sink, AutoRemediate: true}
	require.NoError(t, w.Work(ctx, &river.Job[riverjobs.BillingReconciliationArgs]{Args: riverjobs.BillingReconciliationArgs{}}))

	bd := sink.byKind(credits.ReconBalanceDrift)
	require.Len(t, bd, 1)
	require.Equal(t, int64(60), bd[0].Expected)
	require.Equal(t, int64(200), bd[0].Observed)
	require.False(t, bd[0].Remediated, "balance drift is alert-only")
	require.Equal(t, 1, openReconCount(t, ctx, bunDB, ctID, models.ReconciliationBalanceDrift))

	// Denormalized balance left untouched.
	var bal int64
	require.NoError(t, bunDB.NewSelect().Model((*models.UserCreditBalance)(nil)).
		Column("balance").Where("owner_id = ? AND credit_type_id = ?", owner, ctID).Scan(ctx, &bal))
	require.Equal(t, int64(200), bal)
}

// TestReconciliation_OwnerScopedAndIdempotent: two owners each with their own
// orphan hold are reconciled independently; re-running produces no new events.
func TestReconciliation_OwnerScopedAndIdempotent(t *testing.T) {
	bunDB, dbi := reconTestDB(t)
	ctx := context.Background()
	clock := clockwork.NewFakeClockAt(time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC))
	now := clock.Now().UTC()
	ctID := seedReconCreditType(t, ctx, bunDB, now)

	tenant := uuid.New()
	ownerA := uuid.New()
	ownerB := uuid.New()
	auth := int64(25)
	expired := now.Add(-30 * time.Minute)

	for _, owner := range []uuid.UUID{ownerA, ownerB} {
		_, err := bunDB.NewInsert().Model(&models.UserCreditBalance{
			ID: uuidutil.NewV7(), TenantID: tenant, OwnerID: owner, UserID: owner.String(),
			CreditTypeID: ctID, Balance: 500, HeldBalance: auth, CreatedAt: now, UpdatedAt: now,
		}).Exec(ctx)
		require.NoError(t, err)
		_, err = bunDB.NewInsert().Model(&models.CreditTransaction{
			ID: uuidutil.NewV7(), TenantID: tenant, OwnerID: owner, UserID: owner.String(),
			CreditTypeID: ctID, Amount: 0, TransactionType: "hold", Status: "active",
			Authorized: &auth, Source: "usage", ExpiresAt: &expired, CreatedAt: now, UpdatedAt: now,
		}).Exec(ctx)
		require.NoError(t, err)
	}

	sink := &recordingReconSink{}
	w := &riverjobs.BillingReconciliationWorker{DB: dbi, Clock: clock, Sink: sink, AutoRemediate: true}
	require.NoError(t, w.Work(ctx, &river.Job[riverjobs.BillingReconciliationArgs]{Args: riverjobs.BillingReconciliationArgs{}}))

	orphans := sink.byKind(credits.ReconOrphanHold)
	require.Len(t, orphans, 2, "each owner's orphan hold reconciled independently")
	owners := map[uuid.UUID]bool{}
	for _, o := range orphans {
		require.NotNil(t, o.OwnerID)
		owners[*o.OwnerID] = true
	}
	require.True(t, owners[ownerA] && owners[ownerB])

	// Both owners' held_balance restored to 0.
	for _, owner := range []uuid.UUID{ownerA, ownerB} {
		var held int64
		require.NoError(t, bunDB.NewSelect().Model((*models.UserCreditBalance)(nil)).
			Column("held_balance").Where("owner_id = ? AND credit_type_id = ?", owner, ctID).Scan(ctx, &held))
		require.Equal(t, int64(0), held)
	}

	// Idempotent re-run.
	sink2 := &recordingReconSink{}
	w2 := &riverjobs.BillingReconciliationWorker{DB: dbi, Clock: clock, Sink: sink2, AutoRemediate: true}
	require.NoError(t, w2.Work(ctx, &river.Job[riverjobs.BillingReconciliationArgs]{Args: riverjobs.BillingReconciliationArgs{}}))
	require.Empty(t, sink2.byKind(credits.ReconOrphanHold))
}

// TestReconciliation_CrossSystemSettlementDiff: the ReconcileSettlements
// interface flags an expected settlement with no ledger capture (missing) and a
// ledger capture with no expected settlement (double-charge candidate),
// owner-scoped and idempotent.
func TestReconciliation_CrossSystemSettlementDiff(t *testing.T) {
	bunDB, dbi := reconTestDB(t)
	ctx := context.Background()
	clock := clockwork.NewFakeClockAt(time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC))
	now := clock.Now().UTC()
	ctID := seedReconCreditType(t, ctx, bunDB, now)

	tenant := uuid.New()
	owner := uuid.New()

	// Ledger has a withdrawal for req-A (settled) and a captured hold for req-X
	// that the host did NOT expect (double-charge candidate).
	srcA := "req-A"
	_, err := bunDB.NewInsert().Model(&models.CreditTransaction{
		ID: uuidutil.NewV7(), TenantID: tenant, OwnerID: owner, UserID: owner.String(),
		CreditTypeID: ctID, Amount: -10, TransactionType: "withdrawal", Status: "posted",
		Source: "usage", SourceID: &srcA, CreatedAt: now, UpdatedAt: now,
	}).Exec(ctx)
	require.NoError(t, err)
	srcX := "req-X"
	capAmt := int64(15)
	_, err = bunDB.NewInsert().Model(&models.CreditTransaction{
		ID: uuidutil.NewV7(), TenantID: tenant, OwnerID: owner, UserID: owner.String(),
		CreditTypeID: ctID, Amount: -15, TransactionType: "hold", Status: "captured",
		Authorized: &capAmt, Captured: &capAmt, Source: "usage", SourceID: &srcX, CreatedAt: now, UpdatedAt: now,
	}).Exec(ctx)
	require.NoError(t, err)

	// Host expects req-A (matches) and req-B (NO ledger row => missing).
	expected := []credits.ExpectedSettlement{
		{TenantID: tenant, OwnerID: owner, UserID: owner.String(), CreditTypeID: ctID, Source: "usage", SourceID: "req-A", Amount: 10, EmittedAt: now},
		{TenantID: tenant, OwnerID: owner, UserID: owner.String(), CreditTypeID: ctID, Source: "usage", SourceID: "req-B", Amount: 20, EmittedAt: now},
	}

	sink := &recordingReconSink{}
	w := &riverjobs.BillingReconciliationWorker{DB: dbi, Clock: clock, Sink: sink}
	missing, unexpected, err := w.ReconcileSettlements(ctx, tenant, owner, ctID, expected, now)
	require.NoError(t, err)
	require.Equal(t, 1, missing, "req-B expected but not settled")
	require.Equal(t, 1, unexpected, "req-X settled but not expected")

	require.Len(t, sink.byKind(credits.ReconMissingSettlement), 1)
	require.Equal(t, "req-B", sink.byKind(credits.ReconMissingSettlement)[0].SubjectID)
	require.Len(t, sink.byKind(credits.ReconUnexpectedCapture), 1)
	require.Equal(t, "req-X", sink.byKind(credits.ReconUnexpectedCapture)[0].SubjectID)

	require.Equal(t, 1, openReconCount(t, ctx, bunDB, ctID, models.ReconciliationMissingSettlement))
	require.Equal(t, 1, openReconCount(t, ctx, bunDB, ctID, models.ReconciliationUnexpectedCapture))

	// Idempotent: re-running with the same inputs inserts no duplicate open rows.
	missing2, unexpected2, err := w.ReconcileSettlements(ctx, tenant, owner, ctID, expected, now)
	require.NoError(t, err)
	require.Equal(t, 1, missing2)
	require.Equal(t, 1, unexpected2)
	require.Equal(t, 1, openReconCount(t, ctx, bunDB, ctID, models.ReconciliationMissingSettlement))
	require.Equal(t, 1, openReconCount(t, ctx, bunDB, ctID, models.ReconciliationUnexpectedCapture))
}
