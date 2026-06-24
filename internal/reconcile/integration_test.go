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
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/pkg/merchant"
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
	appDB, err := db.NewWithPGXPool(pool, "") // default schema (shared harness)
	require.NoError(t, err)
	return appDB
}

type seededState struct {
	subjectID uuid.UUID
	productID uuid.UUID
	priceID   uuid.UUID
	subAlive  uuid.UUID // active locally, active remotely
	subDead   uuid.UUID // active locally, ABSENT from the remote roster (PS-2)
	entDeadID uuid.UUID // live entitlement sourced by subDead
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
	s.subjectID = dbtest.EnsureCustomerIDPgx(ctx, t, appDB.Qx(ctx), uuid.NewString())

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
		exec(`INSERT INTO openrails.products (id, slug, display_name, tier_group, entitlements_spec, merchant_id)
		      VALUES ($1, $2, $2, $3, jsonb_build_object($4::text, null), $5)`,
			productID, fmt.Sprintf("reconcile-prod-%d-%s", i, suffix), fmt.Sprintf("reconcile-tier-%d-%s", i, suffix), entName, dbtest.TestMerchantID.UUID())
		exec(`INSERT INTO openrails.prices (id, product_id, amount, currency, billing_cycle_days, merchant_id)
		      VALUES ($1, $2, 999, 'usd', 30, $3)`, priceID, productID, dbtest.TestMerchantID.UUID())
		exec(`INSERT INTO openrails.subscriptions
		        (id, price_id, product_id, status, processor, processor_subscription_id,
		         current_period_starts_at, current_period_ends_at, started_at,
		         entitlements_spec_snapshot, customer_id, merchant_id)
		      VALUES ($1, $2, $3, 'active', 'nmi', $4, $5, $6, $5, jsonb_build_object($8::text, null), $7, $9)`,
			subID, priceID, productID, fmt.Sprintf("it-%d-%s", i, suffix),
			periodStart, periodEnd, s.subjectID, entName, dbtest.TestMerchantID.UUID())
		entID := uuid.New()
		if subID == s.subDead {
			entID = s.entDeadID
		}
		exec(`INSERT INTO openrails.entitlements (id, customer_id, entitlement, start_at, end_at, source_id, source_type, merchant_id)
		      VALUES ($1, $2, $3, $4, $5, $6, 'subscription', $7)`,
			entID, s.subjectID, entName, periodStart, periodEnd, subID, dbtest.TestMerchantID.UUID())
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
		`SELECT processor_subscription_id FROM openrails.subscriptions WHERE id = $1`, seeded.subAlive).Scan(&alivePSID))

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
	baseCtx := merchant.WithID(context.Background(), dbtest.TestMerchantID)

	var seeded seededState
	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
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
	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		snap = reconcileSnapshot(t, ctx, appDB, seeded)
		return nil
	}))

	// ---- advisory: findings persisted, zero local writes -------------------
	var advisory *RunResult
	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
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
	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
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
	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		var status string
		require.NoError(t, appDB.Qx(ctx).QueryRow(ctx, `SELECT status::text FROM openrails.subscriptions WHERE id = $1`, seeded.subDead).Scan(&status))
		assert.Equal(t, "active", status, "advisory must not cancel anything")
		var n int
		require.NoError(t, appDB.Qx(ctx).QueryRow(ctx, `SELECT count(*) FROM openrails.payments WHERE transaction_id LIKE 'itxn-%'`).Scan(&n))
		assert.Zero(t, n, "advisory must not backfill payments")
		return nil
	}))

	// ---- enforce: local state converges, findings auto_fixed ---------------
	var enforce *RunResult
	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		res, err := newEngine(snap).Run(ctx, RunParams{Mode: ModeEnforce, Providers: []Provider{ProviderNMI}})
		enforce = res
		return err
	}))
	assert.Equal(t, "completed", enforce.Status)
	assert.GreaterOrEqual(t, enforce.Summary.Providers["nmi"].AutoFixed, 2, "PS-2 + PS-4 applied")

	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		// PS-2: subDead cancelled locally with cancel_type=expired, retry
		// schedule cleared, entitlement revoked.
		var status, cancelType string
		require.NoError(t, appDB.Qx(ctx).QueryRow(ctx,
			`SELECT status::text, COALESCE(cancel_type, '') FROM openrails.subscriptions WHERE id = $1`, seeded.subDead).
			Scan(&status, &cancelType))
		assert.Equal(t, "cancelled", status)
		assert.Equal(t, "expired", cancelType)

		var revokedAt *time.Time
		require.NoError(t, appDB.Qx(ctx).QueryRow(ctx,
			`SELECT revoked_at FROM openrails.entitlements WHERE id = $1`, seeded.entDeadID).Scan(&revokedAt))
		assert.NotNil(t, revokedAt, "subDead's entitlement must be revoked")

		// PS-4: the missing charge is backfilled, deduped identity.
		var amount int64
		var subjectID uuid.UUID
		require.NoError(t, appDB.Qx(ctx).QueryRow(ctx,
			`SELECT amount, customer_id FROM openrails.payments WHERE processor = 'nmi' AND transaction_id LIKE 'itxn-%'`).
			Scan(&amount, &subjectID))
		assert.Equal(t, int64(999), amount)
		assert.Equal(t, seeded.subjectID, subjectID)

		// PS-1 + PS-8 hold in the admin queue, never auto-applied.
		queue, err := store.ListFindings(ctx, FindingFilter{Provider: "nmi", OnlyAdminQueue: true})
		require.NoError(t, err)
		queueTypes := map[FindingType]bool{}
		for _, f := range queue {
			queueTypes[f.Type] = true
			assert.Equal(t, FindingStatusAdminRequired, f.Status)
		}
		assert.True(t, queueTypes[FindingRemoteSubMissingLocal], "PS-1 in admin queue")
		assert.True(t, queueTypes[FindingDuplicateSubscriptions], "PS-8 in admin queue")
		return nil
	}))

	// ---- rerun enforce: stable -----------------------------------------------
	var rerun *RunResult
	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
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

	// Identity is stable: the PS-1 records update in place instead of
	// duplicating rows.
	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		all, err := store.ListFindings(ctx, FindingFilter{Provider: "nmi", Type: string(FindingRemoteSubMissingLocal)})
		require.NoError(t, err)
		require.Len(t, all, 3, "three PS-1 identities (ghost + two unmatched duplicates), one row each")
		var ghost *FindingRecord
		for i := range all {
			assert.Equal(t, advisory.RunID, all[i].FirstSeenRun, "three runs, one row per identity")
			assert.Equal(t, rerun.RunID, all[i].LastSeenRun, "three runs, one row per identity")
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
		assert.Equal(t, FindingStatusFixed, acked.Status)
		assert.Equal(t, "admin_fixed", acked.Resolution)
		assert.Equal(t, "imported by hand", acked.Notes)
		return nil
	}))

	// ---- drift vanishes: auto-resolve ---------------------------------------
	fixedSnap := *snap
	fixedSnap.Subscriptions = snap.Subscriptions[:1] // ghosts + dups disappear
	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		res, err := newEngine(&fixedSnap).Run(ctx, RunParams{Mode: ModeAdvisory, Providers: []Provider{ProviderNMI}})
		require.NoError(t, err)
		assert.GreaterOrEqual(t, res.Summary.Providers["nmi"].AutoResolved, int64(1), "PS-8 vanished")

		dups, err := store.ListFindings(ctx, FindingFilter{Provider: "nmi", Type: string(FindingDuplicateSubscriptions)})
		require.NoError(t, err)
		require.Len(t, dups, 1)
		assert.Equal(t, FindingStatusFixed, dups[0].Status)
		assert.Equal(t, "auto_vanished", dups[0].Resolution)
		return nil
	}))
}

// TestReconcileMaterializeIntegration proves the PS-1 materialization end to
// end against real Postgres under RLS: a fake snapshot carries one RESOLVABLE
// PS-1 (vault identity + provider_links plan) and one UNRESOLVABLE PS-1.
// `fix` (enforce) creates the local subscription with remote status/periods,
// backfills the snapshot's charge, grants entitlements via the
// subscription-sourced path, and resolves the finding as enforced — while the
// unresolvable one stays requires_review.
func TestReconcileMaterializeIntegration(t *testing.T) {
	appDB := startReconcilePostgres(t)
	baseCtx := merchant.WithID(context.Background(), dbtest.TestMerchantID)
	store := &PGStore{DB: appDB}

	suffix := uuid.NewString()[:8]
	subjectIDHolder := uuid.Nil
	productID := uuid.New()
	priceID := uuid.New()
	pmID := uuid.New()
	vaultID := "mat-vault-" + suffix
	planID := "mat-plan-" + suffix
	entName := "premium-mat-" + suffix

	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		subjectIDHolder = dbtest.EnsureCustomerIDPgx(ctx, t, appDB.Qx(ctx), uuid.NewString())
		exec := func(sql string, args ...any) {
			t.Helper()
			_, err := appDB.Qx(ctx).Exec(ctx, sql, args...)
			require.NoError(t, err)
		}
		// Catalog: product with an entitlements spec + a price whose
		// provider_links blob carries the NMI plan id.
		exec(`INSERT INTO openrails.products (id, slug, display_name, tier_group, entitlements_spec, merchant_id)
		      VALUES ($1, $2, $2, $3, jsonb_build_object($4::text, null), $5)`,
			productID, "mat-prod-"+suffix, "mat-tier-"+suffix, entName, dbtest.TestMerchantID.UUID())
		exec(`INSERT INTO openrails.prices (id, product_id, amount, currency, billing_cycle_days, processors, merchant_id)
		      VALUES ($1, $2, 1499, 'usd', 30, jsonb_build_object('nmi', jsonb_build_object('plan_id', $3::text)), $4)`,
			priceID, productID, planID, dbtest.TestMerchantID.UUID())
		// Identity anchor: a stored payment method holding the remote vault id.
		exec(`INSERT INTO openrails.payment_methods (id, customer_id, processor, vault_id, initial_transaction_id, last_four, expiry_date, merchant_id)
		      VALUES ($1, $2, 'nmi', $3, 'init-txn-'||$4::text, '1111', '1029', $5)`, pmID, subjectIDHolder, vaultID, suffix, dbtest.TestMerchantID.UUID())
		return nil
	}))
	subjectID := subjectIDHolder

	now := time.Now().UTC()
	periodEnd := now.Add(20 * 24 * time.Hour)
	lastBilled := now.Add(-10 * 24 * time.Hour)
	resolvablePSID := "mat-resolvable-" + suffix
	unresolvablePSID := "mat-unresolvable-" + suffix
	txnID := "mat-txn-" + suffix
	snap := &RemoteSnapshot{
		Provider:     ProviderNMI,
		FetchedAt:    now,
		Capabilities: Capabilities{Subscriptions: true, Transactions: true, Vault: true},
		Subscriptions: []RemoteSubscription{
			{
				ProcessorSubscriptionID: resolvablePSID,
				Status:                  SubscriptionStatusActive,
				CustomerID:              vaultID,
				Email:                   "mat-" + suffix + "@example.com",
				PlanID:                  planID,
				NextBillingAt:           &periodEnd,
				LastBilledAt:            &lastBilled,
				AmountCents:             1499,
				Currency:                "usd",
			},
			{
				// No vault/email match locally, no plan link: unresolvable.
				ProcessorSubscriptionID: unresolvablePSID,
				Status:                  SubscriptionStatusActive,
				Email:                   "stranger-" + suffix + "@example.com",
				PlanID:                  "unknown-plan-" + suffix,
			},
		},
		Transactions: []RemoteTransaction{
			{
				TransactionID: txnID, Type: TransactionTypeSale, Success: true,
				AmountCents: 1499, Currency: "USD", OccurredAt: lastBilled,
				Raw: rawJSON(map[string]any{"customer_vault_id": vaultID}),
			},
		},
	}

	newEngine := func() *Engine {
		return &Engine{
			Fetchers: map[Provider]ProcessorFetcher{ProviderNMI: &fakeFetcher{provider: ProviderNMI, snap: snap}},
			Store:    store,
			Local:    &PGLocalStateLoader{DB: appDB},
			Writer:   &PGLocalWriter{DB: appDB},
		}
	}

	// ---- advisory: both PS-1 stay requires_review, nothing is created ----------
	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		res, err := newEngine().Run(ctx, RunParams{Mode: ModeAdvisory, Providers: []Provider{ProviderNMI}})
		require.NoError(t, err)
		for _, f := range res.Findings {
			if f.Type == FindingRemoteSubMissingLocal {
				assert.Equal(t, FindingStatusAdminRequired, f.Status, "advisory PS-1 stays requires_review")
			}
		}
		var n int
		require.NoError(t, appDB.Qx(ctx).QueryRow(ctx,
			`SELECT count(*) FROM openrails.subscriptions WHERE processor_subscription_id = $1`, resolvablePSID).Scan(&n))
		assert.Zero(t, n, "no subscription created by an advisory run")
		return nil
	}))

	// ---- enforce: resolvable PS-1 materializes automatically ------------------
	var matRun *RunResult
	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		res, err := newEngine().Run(ctx, RunParams{Mode: ModeEnforce, Providers: []Provider{ProviderNMI}})
		matRun = res
		return err
	}))
	require.NotNil(t, matRun)
	assert.Equal(t, "completed", matRun.Status)

	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		// Resolvable: subscription created with remote status/periods.
		var subID uuid.UUID
		var status, processor string
		var gotSubject uuid.UUID
		var gotPeriodEnd time.Time
		require.NoError(t, appDB.Qx(ctx).QueryRow(ctx,
			`SELECT id, status::text, processor, customer_id, current_period_ends_at
			 FROM openrails.subscriptions WHERE processor_subscription_id = $1`, resolvablePSID).
			Scan(&subID, &status, &processor, &gotSubject, &gotPeriodEnd))
		assert.Equal(t, "active", status)
		assert.Equal(t, "nmi", processor)
		assert.Equal(t, subjectID, gotSubject)
		assert.WithinDuration(t, periodEnd, gotPeriodEnd, time.Second)

		// Snapshot charge backfilled against the new subscription.
		var amount int64
		var paySub uuid.UUID
		require.NoError(t, appDB.Qx(ctx).QueryRow(ctx,
			`SELECT amount, subscription_id FROM openrails.payments WHERE transaction_id = $1`, txnID).
			Scan(&amount, &paySub))
		assert.Equal(t, int64(1499), amount)
		assert.Equal(t, subID, paySub)

		// Entitlements granted through the normal subscription-sourced path.
		var entCount int
		require.NoError(t, appDB.Qx(ctx).QueryRow(ctx,
			`SELECT count(*) FROM openrails.entitlements
			 WHERE source_type = 'subscription' AND source_id = $1
			   AND entitlement = $2 AND revoked_at IS NULL`, subID, entName).Scan(&entCount))
		assert.Equal(t, 1, entCount)

		// Finding fixed as enforced with the resolution evidence.
		findings, err := store.ListFindings(ctx, FindingFilter{Provider: "nmi", Type: string(FindingRemoteSubMissingLocal), Limit: 50})
		require.NoError(t, err)
		var resolved, pending *FindingRecord
		for i := range findings {
			switch findings[i].SubjectKey {
			case resolvablePSID:
				resolved = &findings[i]
			case unresolvablePSID:
				pending = &findings[i]
			}
		}
		require.NotNil(t, resolved)
		assert.Equal(t, FindingStatusAutoFixed, resolved.Status)
		assert.Equal(t, "enforced", resolved.Resolution)
		require.NotNil(t, resolved.ResolutionEvid)
		assert.Equal(t, subID.String(), resolved.ResolutionEvid["materialized_subscription_id"])
		assert.Equal(t, "vault_id", resolved.ResolutionEvid["identity_via"])

		// Unresolvable: still requires_review, blocker documented.
		require.NotNil(t, pending)
		assert.Equal(t, FindingStatusAdminRequired, pending.Status)
		assert.True(t, pending.RequiresAdmin)
		blocked, _ := pending.RemoteEvidence["materialize_blocked"].(string)
		assert.Contains(t, blocked, "identity unresolved")
		assert.Contains(t, blocked, "plan unresolved")
		return nil
	}))

	// ---- re-run: idempotent, no duplicate subscription ------------------------
	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		res, err := newEngine().Run(ctx, RunParams{Mode: ModeEnforce, Providers: []Provider{ProviderNMI}})
		require.NoError(t, err)
		for _, f := range res.Findings {
			assert.NotEqual(t, resolvablePSID, f.SubjectKey, "materialized PS-1 must not re-diff")
		}
		var n int
		require.NoError(t, appDB.Qx(ctx).QueryRow(ctx,
			`SELECT count(*) FROM openrails.subscriptions WHERE processor_subscription_id = $1`, resolvablePSID).Scan(&n))
		assert.Equal(t, 1, n, "exactly one materialized subscription after a re-run")
		return nil
	}))
}

// TestReconcileStuckIntentIntegration proves the PS-10 stuck-intent finding
// type end to end against real Postgres under RLS: non-terminal
// openrails.provider_intents rows older than the hardcoded thresholds surface as
// findings on a run with ZERO providers configured (PS-10 is
// provider-independent and reads only the local ledger), mode/kill-switch
// parks stay informational, fresh intents stay invisible, enforce never
// touches the intent rows, and a recovered intent's finding auto-resolves.
func TestReconcileStuckIntentIntegration(t *testing.T) {
	appDB := startReconcilePostgres(t)
	baseCtx := merchant.WithID(context.Background(), dbtest.TestMerchantID)
	store := &PGStore{DB: appDB}

	suffix := uuid.NewString()[:8]
	oldPending := uuid.New()    // pending 26h, genuine failure -> requires-review
	parkedPending := uuid.New() // pending 26h, kill-switch park -> informational
	freshPending := uuid.New()  // pending 1h -> no finding
	oldUnknown := uuid.New()    // unknown_needs_verify 3h -> requires-review
	subscriptionID := uuid.New()

	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		seed := func(id uuid.UUID, status string, age time.Duration, reason *string) {
			t.Helper()
			_, err := appDB.Qx(ctx).Exec(ctx,
				`INSERT INTO openrails.provider_intents
				   (id, provider, intent_type, subscription_id, idempotency_key,
				    status, attempts, next_attempt_at, origin, last_failure_reason, created_at, merchant_id)
				 VALUES ($1, 'mobius', 'nmi_delete_subscription', $2, $3, $4, 2, now(), 'system', $5, now() - make_interval(mins => $6), $7)`,
				id, subscriptionID, fmt.Sprintf("stuck-%s-%s", id.String()[:8], suffix),
				status, reason, int(age.Minutes()), dbtest.TestMerchantID.UUID())
			require.NoError(t, err)
		}
		failure := "nmi error: connection refused"
		park := "nmi client is read-only (mode=readonly)"
		ambiguous := "ambiguous: timeout after send"
		seed(oldPending, "pending", 26*time.Hour, &failure)
		seed(parkedPending, "pending", 26*time.Hour, &park)
		seed(freshPending, "pending", time.Hour, nil)
		seed(oldUnknown, "unknown_needs_verify", 3*time.Hour, &ambiguous)
		return nil
	}))

	// Engine with NO fetchers: the PS-10 pass runs regardless of providers.
	newEngine := func() *Engine {
		return &Engine{
			Fetchers: map[Provider]ProcessorFetcher{},
			Store:    store,
			Local:    &PGLocalStateLoader{DB: appDB},
			Writer:   &PGLocalWriter{DB: appDB},
			Intents:  &PGStuckIntentSource{DB: appDB},
		}
	}

	// ---- check: stuck intents surface, fresh ones do not --------------------
	var check *RunResult
	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		res, err := newEngine().Run(ctx, RunParams{Mode: ModeAdvisory})
		check = res
		return err
	}))
	require.NotNil(t, check)
	assert.Equal(t, "completed", check.Status)

	bySubject := map[string]FindingRecord{}
	for _, f := range check.Findings {
		if f.Type == FindingStuckIntent {
			bySubject[f.SubjectKey] = f
		}
	}
	require.Len(t, bySubject, 3, "old pending + parked + old unknown")
	assert.NotContains(t, bySubject, freshPending.String(), "a fresh pending intent is not stuck")

	rec := bySubject[oldPending.String()]
	assert.Equal(t, FindingStatusAdminRequired, rec.Status)
	assert.True(t, rec.RequiresAdmin)
	assert.Equal(t, SeverityHigh, rec.Severity)
	assert.Equal(t, Provider("mobius"), rec.Provider)
	assert.Equal(t, "nmi_delete_subscription", rec.IntentEvidence["intent_type"])
	assert.Equal(t, subscriptionID.String(), rec.IntentEvidence["subscription_id"])
	assert.Equal(t, "nmi error: connection refused", rec.IntentEvidence["last_failure_reason"])

	parked := bySubject[parkedPending.String()]
	assert.Equal(t, FindingStatusReconcileRequired, parked.Status)
	assert.False(t, parked.RequiresAdmin, "kill-switch park is informational")
	assert.Equal(t, SeverityLow, parked.Severity)

	unknown := bySubject[oldUnknown.String()]
	assert.Equal(t, FindingStatusAdminRequired, unknown.Status)
	assert.True(t, unknown.RequiresAdmin)

	stuckRep := check.Summary.StuckIntents
	require.NotNil(t, stuckRep)
	assert.Equal(t, 3, stuckRep.Total)
	assert.Equal(t, 2, stuckRep.AdminRequired)
	assert.Equal(t, 1, stuckRep.ModeParked)
	assert.Equal(t, 3, stuckRep.ByIntentType["nmi_delete_subscription"])

	// Admin queue carries the genuine stuck intents, not the parked one.
	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		queue, err := store.ListFindings(ctx, FindingFilter{Type: string(FindingStuckIntent), OnlyAdminQueue: true})
		require.NoError(t, err)
		assert.Len(t, queue, 2)
		return nil
	}))

	// ---- fix: identical emission, intents untouched --------------------------
	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		res, err := newEngine().Run(ctx, RunParams{Mode: ModeEnforce})
		require.NoError(t, err)
		require.NotNil(t, res.Summary.StuckIntents)
		assert.Equal(t, 3, res.Summary.StuckIntents.Total, "fix emits/refreshes identically")

		// fix mode never touches the ledger: statuses and attempts unchanged.
		var status string
		var attempts int
		require.NoError(t, appDB.Qx(ctx).QueryRow(ctx,
			`SELECT status, attempts FROM openrails.provider_intents WHERE id = $1`, oldPending).Scan(&status, &attempts))
		assert.Equal(t, "pending", status)
		assert.Equal(t, 2, attempts)

		// Stable identity: one row per intent, seen again on the second run.
		all, err := store.ListFindings(ctx, FindingFilter{Type: string(FindingStuckIntent), Limit: 50})
		require.NoError(t, err)
		assert.Len(t, all, 3)
		for _, f := range all {
			assert.Equal(t, check.RunID, f.FirstSeenRun)
			assert.Equal(t, res.RunID, f.LastSeenRun)
		}
		return nil
	}))

	// ---- recovery: succeeded/superseded intents auto-resolve -----------------
	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		_, err := appDB.Qx(ctx).Exec(ctx,
			`UPDATE openrails.provider_intents SET status = 'succeeded', executed_at = now() WHERE id = $1`, oldPending)
		require.NoError(t, err)
		_, err = appDB.Qx(ctx).Exec(ctx,
			`UPDATE openrails.provider_intents SET status = 'superseded' WHERE id = $1`, oldUnknown)
		require.NoError(t, err)
		return nil
	}))
	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		res, err := newEngine().Run(ctx, RunParams{Mode: ModeAdvisory})
		require.NoError(t, err)
		require.NotNil(t, res.Summary.StuckIntents)
		assert.Equal(t, 1, res.Summary.StuckIntents.Total, "only the parked intent is still stuck")
		assert.Equal(t, int64(2), res.Summary.StuckIntents.AutoResolved)

		recovered, err := store.ListFindings(ctx, FindingFilter{Type: string(FindingStuckIntent), Status: string(FindingStatusFixed), Limit: 50})
		require.NoError(t, err)
		require.Len(t, recovered, 2)
		for _, f := range recovered {
			assert.Equal(t, "auto_vanished", f.Resolution)
		}
		return nil
	}))
}

// TestReconcileRunRecordsFailure proves a fetch failure persists a failed run
// with its error instead of half-applying.
func TestReconcileRunRecordsFailure(t *testing.T) {
	appDB := startReconcilePostgres(t)
	baseCtx := merchant.WithID(context.Background(), dbtest.TestMerchantID)
	store := &PGStore{DB: appDB}
	eng := &Engine{
		Fetchers: map[Provider]ProcessorFetcher{ProviderNMI: &fakeFetcher{provider: ProviderNMI, err: fmt.Errorf("query.php unreachable")}},
		Store:    store,
		Local:    &PGLocalStateLoader{DB: appDB},
		Writer:   &PGLocalWriter{DB: appDB},
	}
	var res *RunResult
	err := appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		r, err := eng.Run(ctx, RunParams{Mode: ModeAdvisory, Providers: []Provider{ProviderNMI}})
		res = r
		return err
	})
	require.Error(t, err)
	require.NotNil(t, res)
	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		run, err := store.GetRun(ctx, res.RunID)
		require.NoError(t, err)
		assert.Equal(t, "failed", run.Status)
		assert.Contains(t, run.Error, "query.php unreachable")
		return nil
	}))
}

// TestReconcileAdoptPreservesScheduledProviderActions covers #511 Phase H
// schedule-awareness: a pull that observes a subscription still LIVE at the
// provider (because a cancel/downgrade is scheduled for the FUTURE, not yet
// effective — "Jun 15 cancel / Jun 28 delete / Jun 17 pull is consistent") adopts
// the provider-observed status/period but MUST NOT clobber the standing local
// provider-action intents. The adopt applier writes only status + period, so
// deletion_scheduled_at / scheduled_price_id survive by construction; this locks
// that in against a regression that widens the UPDATE.
func TestReconcileAdoptPreservesScheduledProviderActions(t *testing.T) {
	appDB := startReconcilePostgres(t)
	baseCtx := merchant.WithID(context.Background(), dbtest.TestMerchantID)
	merchantID := dbtest.TestMerchantID.UUID()
	suffix := uuid.NewString()[:8]
	productID, priceID, schedPriceID, subID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	deleteAt := time.Now().UTC().Add(11 * 24 * time.Hour).Truncate(time.Second)

	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		customer := dbtest.EnsureCustomerIDPgx(ctx, t, appDB.Qx(ctx), uuid.NewString())
		exec := func(sql string, args ...any) {
			_, err := appDB.Qx(ctx).Exec(ctx, sql, args...)
			require.NoError(t, err)
		}
		exec(`INSERT INTO openrails.products (id, slug, display_name, merchant_id) VALUES ($1,$2,$2,$3)`, productID, "sa-prod-"+suffix, merchantID)
		// Two prices for the SAME product (the scheduled downgrade target), so the
		// subscriptions_price_product_merchant composite FK is satisfied.
		exec(`INSERT INTO openrails.prices (id, product_id, amount, currency, merchant_id) VALUES ($1,$2,999,'usd',$3),($4,$2,499,'usd',$3)`,
			priceID, productID, merchantID, schedPriceID)
		exec(`INSERT INTO openrails.subscriptions
		        (id, price_id, product_id, status, processor, processor_subscription_id,
		         current_period_starts_at, current_period_ends_at, started_at,
		         deletion_scheduled_at, scheduled_price_id, entitlements_spec_snapshot, customer_id, merchant_id)
		      VALUES ($1,$2,$3,'active','nmi',$4,$5,$6,$5,$7,$8,'{}'::jsonb,$9,$10)`,
			subID, priceID, productID, "sa-sub-"+suffix,
			time.Now().Add(-20*24*time.Hour), time.Now().Add(10*24*time.Hour), deleteAt, schedPriceID, customer, merchantID)
		return nil
	}))

	t.Cleanup(func() {
		_ = appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.subscriptions WHERE id=$1`, subID)
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.prices WHERE id=ANY($1)`, []uuid.UUID{priceID, schedPriceID})
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.products WHERE id=$1`, productID)
			return nil
		})
	})

	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		newEnd := time.Now().UTC().Add(40 * 24 * time.Hour)
		_, err := appDB.Gen(ctx).ReconcileAdoptSubscriptionStatus(ctx, gen.ReconcileAdoptSubscriptionStatusParams{
			ID: subID, Status: gen.OpenrailsSubscriptionStatus("active"), PeriodEndsAt: &newEnd,
		})
		require.NoError(t, err)

		var delAt *time.Time
		var schedPrice *uuid.UUID
		var status string
		require.NoError(t, appDB.Qx(ctx).QueryRow(ctx,
			`SELECT deletion_scheduled_at, scheduled_price_id, status::text FROM openrails.subscriptions WHERE id=$1`, subID).
			Scan(&delAt, &schedPrice, &status))
		require.Equal(t, "active", status)
		require.NotNil(t, delAt, "scheduled delete must survive the adopt")
		require.WithinDuration(t, deleteAt, *delAt, time.Second)
		require.NotNil(t, schedPrice, "scheduled downgrade must survive the adopt")
		require.Equal(t, schedPriceID, *schedPrice)
		return nil
	}))
}
