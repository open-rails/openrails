//go:build integration

package reconcile

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/pkg/tenant"
)

func TestMain(m *testing.M) { dbtest.RunMain(m) }

// startReconcilePostgres boots the shared fully-migrated Postgres (incl.
// migration 008's reconciliation tables) and returns a *db.DB connected AS
// the unprivileged openrails_app role, so the engine's reads and writes run
// under the same RLS chokepoint as production.
func startReconcilePostgres(t *testing.T) *db.DB {
	t.Helper()
	_, appDSN := dbtest.SharedRLSPostgres(t)
	pool, err := pgxpool.New(context.Background(), appDSN)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	appDB, err := db.NewWithPGXPool(pool)
	require.NoError(t, err)
	return appDB
}

type seededState struct {
	subjectID  uuid.UUID
	productID  uuid.UUID
	priceID    uuid.UUID
	subAlive   uuid.UUID // active locally, active remotely
	subDead    uuid.UUID // active locally, ABSENT from the remote roster (PS-2)
	entDeadID  uuid.UUID // live entitlement sourced by subDead
}

// seedReconcileFixtures inserts a product, price, tenant subject, and two NMI
// subscriptions (one that the fake snapshot will keep alive, one that it
// drops), plus a live subscription-sourced entitlement on each.
func seedReconcileFixtures(t *testing.T, ctx context.Context, appDB *db.DB) seededState {
	t.Helper()
	s := seededState{
		productID: uuid.New(),
		priceID:   uuid.New(),
		subAlive:  uuid.New(),
		subDead:   uuid.New(),
		entDeadID: uuid.New(),
	}
	s.subjectID = dbtest.EnsureTenantSubjectIDPgx(ctx, t, appDB.Qx(ctx), uuid.NewString())

	suffix := uuid.NewString()[:8]
	now := time.Now().UTC()
	periodStart := now.Add(-10 * 24 * time.Hour)
	periodEnd := now.Add(20 * 24 * time.Hour)

	exec := func(sql string, args ...any) {
		t.Helper()
		_, err := appDB.Qx(ctx).Exec(ctx, sql, args...)
		require.NoError(t, err)
	}

	// One product per subscription: uq_subscriptions_tenant_subject_product_lifecycle
	// allows only one live subscription per (subject, product).
	for i, subID := range []uuid.UUID{s.subAlive, s.subDead} {
		productID := s.productID
		priceID := s.priceID
		if i > 0 {
			productID = uuid.New()
			priceID = uuid.New()
		}
		// Distinct entitlement names per subscription: the
		// entitlements_tenant_subject_no_overlap exclusion constraint forbids
		// overlapping windows of one entitlement for one subject.
		entName := fmt.Sprintf("premium-%d", i)
		exec(`INSERT INTO billing.products (id, slug, display_name, tier_group, entitlements_spec)
		      VALUES ($1, $2, $2, $3, jsonb_build_object($4::text, null))`,
			productID, fmt.Sprintf("reconcile-prod-%d-%s", i, suffix), fmt.Sprintf("reconcile-tier-%d-%s", i, suffix), entName)
		exec(`INSERT INTO billing.prices (id, product_id, amount, currency, billing_cycle_days)
		      VALUES ($1, $2, 999, 'usd', 30)`, priceID, productID)
		exec(`INSERT INTO billing.subscriptions
		        (id, price_id, product_id, status, processor, processor_subscription_id,
		         current_period_starts_at, current_period_ends_at, started_at,
		         entitlements_spec_snapshot, tenant_subject_id)
		      VALUES ($1, $2, $3, 'active', 'nmi', $4, $5, $6, $5, jsonb_build_object($8::text, null), $7)`,
			subID, priceID, productID, fmt.Sprintf("it-%d-%s", i, suffix),
			periodStart, periodEnd, s.subjectID, entName)
		entID := uuid.New()
		if subID == s.subDead {
			entID = s.entDeadID
		}
		exec(`INSERT INTO billing.entitlements (id, tenant_subject_id, entitlement, start_at, end_at, source_id, source_type)
		      VALUES ($1, $2, $3, $4, $5, $6, 'subscription')`,
			entID, s.subjectID, entName, periodStart, periodEnd, subID)
	}
	return s
}

// reconcileSnapshot builds the fake NMI snapshot the integration engine
// diffs: subAlive present, subDead absent (PS-2), a ghost subscription
// (PS-1), two duplicate ghosts on one email (PS-8), and a successful charge
// missing locally (PS-4).
func reconcileSnapshot(t *testing.T, ctx context.Context, appDB *db.DB, seeded seededState) *RemoteSnapshot {
	t.Helper()
	var alivePSID string
	require.NoError(t, appDB.Qx(ctx).QueryRow(ctx,
		`SELECT processor_subscription_id FROM billing.subscriptions WHERE id = $1`, seeded.subAlive).Scan(&alivePSID))

	now := time.Now().UTC()
	end := now.Add(20 * 24 * time.Hour)
	return &RemoteSnapshot{
		Provider:     ProviderNMI,
		FetchedAt:    now,
		Capabilities: Capabilities{Subscriptions: true, Transactions: true, Refunds: true, Vault: true},
		Subscriptions: []RemoteSubscription{
			{ProcessorSubscriptionID: alivePSID, Status: SubscriptionStatusActive, NextBillingAt: &end},
			{ProcessorSubscriptionID: "ghost-" + seeded.subAlive.String()[:8], Status: SubscriptionStatusActive, Email: "ghost@example.com"},
			{ProcessorSubscriptionID: "dup1-" + seeded.subAlive.String()[:8], Status: SubscriptionStatusActive, Email: "dup@example.com", PlanID: "plan-1"},
			{ProcessorSubscriptionID: "dup2-" + seeded.subAlive.String()[:8], Status: SubscriptionStatusActive, Email: "dup@example.com", PlanID: "plan-1"},
		},
		Transactions: []RemoteTransaction{
			{
				TransactionID: "itxn-" + seeded.subAlive.String()[:8],
				Type:          TransactionTypeSale,
				Success:       true,
				AmountCents:   999,
				Currency:      "USD",
				OccurredAt:    now.Add(-24 * time.Hour),
				Raw:           rawJSON(map[string]any{"order_id": fmt.Sprintf("rebill-%s-%d", seeded.subAlive, now.Unix())}),
			},
		},
	}
}

func TestReconcileEngineIntegration(t *testing.T) {
	appDB := startReconcilePostgres(t)
	baseCtx := tenant.WithID(context.Background(), tenant.DefaultID)

	var seeded seededState
	require.NoError(t, appDB.RunInTenantConn(baseCtx, func(ctx context.Context) error {
		seeded = seedReconcileFixtures(t, ctx, appDB)
		return nil
	}))

	store := &PGStore{DB: appDB}
	newEngine := func(snap *RemoteSnapshot) *Engine {
		return &Engine{
			Fetchers: map[Provider]ProcessorFetcher{ProviderNMI: &fakeFetcher{provider: ProviderNMI, snap: snap}},
			Store:    store,
			Local:    &PGLocalStateLoader{DB: appDB},
			Writer:   &PGLocalWriter{DB: appDB},
		}
	}

	var snap *RemoteSnapshot
	require.NoError(t, appDB.RunInTenantConn(baseCtx, func(ctx context.Context) error {
		snap = reconcileSnapshot(t, ctx, appDB, seeded)
		return nil
	}))

	// ---- advisory: findings persisted, zero local writes -------------------
	var advisory *RunResult
	require.NoError(t, appDB.RunInTenantConn(baseCtx, func(ctx context.Context) error {
		res, err := newEngine(snap).Run(ctx, RunParams{Mode: ModeAdvisory, Providers: []Provider{ProviderNMI}})
		advisory = res
		return err
	}))
	require.NotNil(t, advisory)
	assert.Equal(t, "completed", advisory.Status)

	byType := map[FindingType]int{}
	for _, f := range advisory.Findings {
		byType[f.Type]++
	}
	assert.Equal(t, 3, byType[FindingRemoteSubMissingLocal], "PS-1: ghost + the two unmatched duplicates")
	assert.Equal(t, 1, byType[FindingLocalActiveRemoteDead], "PS-2 absent subscription")
	assert.Equal(t, 1, byType[FindingChargeMissingLocal], "PS-4 missing charge")
	assert.Equal(t, 1, byType[FindingDuplicateSubscriptions], "PS-8 remote duplicates")

	// Advisory persisted the run + findings...
	require.NoError(t, appDB.RunInTenantConn(baseCtx, func(ctx context.Context) error {
		run, err := store.GetRun(ctx, advisory.RunID)
		require.NoError(t, err)
		assert.Equal(t, "completed", run.Status)
		assert.NotEmpty(t, run.Summary)

		open, err := store.ListFindings(ctx, FindingFilter{Provider: "nmi"})
		require.NoError(t, err)
		assert.GreaterOrEqual(t, len(open), 4)
		return nil
	}))

	// ...but wrote nothing to billing state.
	require.NoError(t, appDB.RunInTenantConn(baseCtx, func(ctx context.Context) error {
		var status string
		require.NoError(t, appDB.Qx(ctx).QueryRow(ctx, `SELECT status::text FROM billing.subscriptions WHERE id = $1`, seeded.subDead).Scan(&status))
		assert.Equal(t, "active", status, "advisory must not cancel anything")
		var n int
		require.NoError(t, appDB.Qx(ctx).QueryRow(ctx, `SELECT count(*) FROM billing.payments WHERE transaction_id LIKE 'itxn-%'`).Scan(&n))
		assert.Zero(t, n, "advisory must not backfill payments")
		return nil
	}))

	// ---- enforce: local state converges, findings auto_fixed ---------------
	var enforce *RunResult
	require.NoError(t, appDB.RunInTenantConn(baseCtx, func(ctx context.Context) error {
		res, err := newEngine(snap).Run(ctx, RunParams{Mode: ModeEnforce, Providers: []Provider{ProviderNMI}})
		enforce = res
		return err
	}))
	assert.Equal(t, "completed", enforce.Status)
	assert.GreaterOrEqual(t, enforce.Summary.Providers["nmi"].AutoFixed, 2, "PS-2 + PS-4 applied")

	require.NoError(t, appDB.RunInTenantConn(baseCtx, func(ctx context.Context) error {
		// PS-2: subDead cancelled locally with cancel_type=expired, retry
		// schedule cleared, entitlement revoked.
		var status, cancelType string
		require.NoError(t, appDB.Qx(ctx).QueryRow(ctx,
			`SELECT status::text, COALESCE(cancel_type, '') FROM billing.subscriptions WHERE id = $1`, seeded.subDead).
			Scan(&status, &cancelType))
		assert.Equal(t, "cancelled", status)
		assert.Equal(t, "expired", cancelType)

		var revokedAt *time.Time
		require.NoError(t, appDB.Qx(ctx).QueryRow(ctx,
			`SELECT revoked_at FROM billing.entitlements WHERE id = $1`, seeded.entDeadID).Scan(&revokedAt))
		assert.NotNil(t, revokedAt, "subDead's entitlement must be revoked")

		// PS-4: the missing charge is backfilled, deduped identity.
		var amount int64
		var subjectID uuid.UUID
		require.NoError(t, appDB.Qx(ctx).QueryRow(ctx,
			`SELECT amount, tenant_subject_id FROM billing.payments WHERE processor = 'nmi' AND transaction_id LIKE 'itxn-%'`).
			Scan(&amount, &subjectID))
		assert.Equal(t, int64(999), amount)
		assert.Equal(t, seeded.subjectID, subjectID)

		// PS-1 + PS-8 hold in the admin queue, never auto-applied.
		queue, err := store.ListFindings(ctx, FindingFilter{Provider: "nmi", OnlyAdminQueue: true})
		require.NoError(t, err)
		queueTypes := map[FindingType]bool{}
		for _, f := range queue {
			queueTypes[f.Type] = true
			assert.Equal(t, FindingStatusAdminPending, f.Status)
		}
		assert.True(t, queueTypes[FindingRemoteSubMissingLocal], "PS-1 in admin queue")
		assert.True(t, queueTypes[FindingDuplicateSubscriptions], "PS-8 in admin queue")
		return nil
	}))

	// ---- rerun enforce: stable -----------------------------------------------
	var rerun *RunResult
	require.NoError(t, appDB.RunInTenantConn(baseCtx, func(ctx context.Context) error {
		res, err := newEngine(snap).Run(ctx, RunParams{Mode: ModeEnforce, Providers: []Provider{ProviderNMI}})
		rerun = res
		return err
	}))
	assert.Equal(t, "completed", rerun.Status)
	assert.Zero(t, rerun.Summary.Providers["nmi"].AutoFixed, "second enforce run must be a no-op")
	rerunTypes := map[FindingType]int{}
	for _, f := range rerun.Findings {
		rerunTypes[f.Type]++
	}
	assert.Zero(t, rerunTypes[FindingLocalActiveRemoteDead], "PS-2 converged")
	assert.Zero(t, rerunTypes[FindingChargeMissingLocal], "PS-4 converged")
	assert.Equal(t, 3, rerunTypes[FindingRemoteSubMissingLocal], "PS-1 persists for the admin")
	assert.Equal(t, 1, rerunTypes[FindingDuplicateSubscriptions], "PS-8 persists for the admin")

	// Identity is stable: the PS-1 record accumulated occurrences instead of
	// duplicating rows.
	require.NoError(t, appDB.RunInTenantConn(baseCtx, func(ctx context.Context) error {
		all, err := store.ListFindings(ctx, FindingFilter{Provider: "nmi", Type: string(FindingRemoteSubMissingLocal)})
		require.NoError(t, err)
		require.Len(t, all, 3, "three PS-1 identities (ghost + two unmatched duplicates), one row each")
		var ghost *FindingRecord
		for i := range all {
			assert.Equal(t, 3, all[i].OccurrenceCount, "three runs, one row per identity")
			if strings.HasPrefix(all[i].SubjectKey, "ghost-") {
				ghost = &all[i]
			}
		}
		require.NotNil(t, ghost)
		assert.Equal(t, advisory.RunID, ghost.FirstSeenRun)
		assert.Equal(t, rerun.RunID, ghost.LastSeenRun)

		// Ack/dismiss lifecycle.
		ok, err := store.AckFinding(ctx, ghost.ID, "imported by hand")
		require.NoError(t, err)
		assert.True(t, ok)
		acked, err := store.GetFinding(ctx, ghost.ID)
		require.NoError(t, err)
		assert.Equal(t, FindingStatusResolved, acked.Status)
		assert.Equal(t, "admin_ack", acked.Resolution)
		assert.Equal(t, "imported by hand", acked.Notes)
		return nil
	}))

	// ---- drift vanishes: auto-resolve ---------------------------------------
	fixedSnap := *snap
	fixedSnap.Subscriptions = snap.Subscriptions[:1] // ghosts + dups disappear
	require.NoError(t, appDB.RunInTenantConn(baseCtx, func(ctx context.Context) error {
		res, err := newEngine(&fixedSnap).Run(ctx, RunParams{Mode: ModeAdvisory, Providers: []Provider{ProviderNMI}})
		require.NoError(t, err)
		assert.GreaterOrEqual(t, res.Summary.Providers["nmi"].AutoResolved, int64(1), "PS-8 vanished")

		dups, err := store.ListFindings(ctx, FindingFilter{Provider: "nmi", Type: string(FindingDuplicateSubscriptions)})
		require.NoError(t, err)
		require.Len(t, dups, 1)
		assert.Equal(t, FindingStatusResolved, dups[0].Status)
		assert.Equal(t, "auto_vanished", dups[0].Resolution)
		return nil
	}))
}

// TestReconcileRunRecordsFailure proves a fetch failure persists a failed run
// with its error instead of half-applying.
func TestReconcileRunRecordsFailure(t *testing.T) {
	appDB := startReconcilePostgres(t)
	baseCtx := tenant.WithID(context.Background(), tenant.DefaultID)
	store := &PGStore{DB: appDB}
	eng := &Engine{
		Fetchers: map[Provider]ProcessorFetcher{ProviderNMI: &fakeFetcher{provider: ProviderNMI, err: fmt.Errorf("query.php unreachable")}},
		Store:    store,
		Local:    &PGLocalStateLoader{DB: appDB},
		Writer:   &PGLocalWriter{DB: appDB},
	}
	var res *RunResult
	err := appDB.RunInTenantConn(baseCtx, func(ctx context.Context) error {
		r, err := eng.Run(ctx, RunParams{Mode: ModeAdvisory, Providers: []Provider{ProviderNMI}})
		res = r
		return err
	})
	require.Error(t, err)
	require.NotNil(t, res)
	require.NoError(t, appDB.RunInTenantConn(baseCtx, func(ctx context.Context) error {
		run, err := store.GetRun(ctx, res.RunID)
		require.NoError(t, err)
		assert.Equal(t, "failed", run.Status)
		assert.Contains(t, run.Error, "query.php unreachable")
		return nil
	}))
}
