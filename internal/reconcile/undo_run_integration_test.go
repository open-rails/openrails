//go:build integration

package reconcile

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/pkg/merchant"
)

// or#859 tier 1, the scope + gate slice: `openrails undo-run` over the whole
// destructive-run ledger.
//
// The converge reversibility itself is proven in
// converge_rollback_integration_test.go. What is proven here is the part the
// owner actually asked for — "per-merchant + per-payment-provider rollback" —
// plus the two gates that stand between an operator and a second incident: the
// dry run, and the refusal to reverse a kind whose damage no local undo reaches.

// runEnforcingPass drives one enforcing pull bound to `f`'s PSP against the bad
// roster and returns the destructive run it opened.
func runEnforcingPass(t *testing.T, appDB *db.DB, baseCtx context.Context, f convergeRunFixture, snap *RemoteSnapshot, now time.Time) uuid.UUID {
	t.Helper()
	eng := &Engine{
		Fetchers:  map[Provider]RailFetcher{ProviderNMI: &fakeFetcher{provider: ProviderNMI, snap: snap}},
		Store:     &PGStore{DB: appDB},
		Local:     &PGLocalStateLoader{DB: appDB},
		Writer:    &PGLocalWriter{DB: appDB},
		Decisions: NewDecisionApplier(appDB, intents.NewNMIDeleteScheduler(appDB, nil, intents.OriginSystem, "or859 undo-run test")),
		Runs:      &PGDestructiveRunRecorder{DB: appDB},
		Now:       func() time.Time { return now },
		Actor:     "or859-test",
	}
	var runID uuid.UUID
	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		res, err := eng.Run(ctx, RunParams{
			Mode:        ModeEnforce,
			Mutations:   &LocalMutationPolicy{Overwrite: true},
			Providers:   []Provider{ProviderNMI},
			PSPs:        map[Provider]PSPBinding{ProviderNMI: f.binding},
			PSPCoverage: map[Provider]PSPCoverage{ProviderNMI: {Declared: 1, Pulled: 1, Binding: f.binding}},
		})
		if err != nil {
			return err
		}
		runID = uuid.MustParse(res.Summary.Providers[string(ProviderNMI)].DestructiveRunID)
		return nil
	}))
	return runID
}

func planFor(t *testing.T, appDB *db.DB, baseCtx context.Context, runID uuid.UUID) UndoPlan {
	t.Helper()
	var plan UndoPlan
	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		var e error
		plan, e = PlanUndoRun(ctx, appDB, runID)
		return e
	}))
	return plan
}

// TestUndoRun_PerPSPScope_RestoresOneAccountAndLeavesTheSiblingAlone is the
// owner's ask, end to end: a merchant running two PSPs, one of which is wrecked
// by a bad roster. The undo must put THAT account's book back and be incapable
// of touching the other's — not by filtering carefully, but because the scope is
// a property of the run and every predicate is keyed on the run id.
func TestUndoRun_PerPSPScope_RestoresOneAccountAndLeavesTheSiblingAlone(t *testing.T) {
	appDB := startReconcilePostgres(t)
	baseCtx := merchant.WithID(context.Background(), dbtest.TestMerchantID)
	now := time.Now().UTC().Truncate(time.Second)
	periodEnd := now.Add(-60 * 24 * time.Hour)
	periodStart := periodEnd.Add(-30 * 24 * time.Hour)
	entEnd := now.Add(30 * 24 * time.Hour)

	// Two PSPs on the same rail, same merchant, each with its own book.
	wrecked := seedConvergeCohort(t, appDB, baseCtx, 5, periodStart, periodEnd, entEnd)
	sibling := seedConvergeCohort(t, appDB, baseCtx, 5, periodStart, periodEnd, entEnd)

	wreckedBefore := readSubStates(t, appDB, baseCtx, wrecked.subs)
	siblingBefore := readSubStates(t, appDB, baseCtx, sibling.subs)
	require.Len(t, siblingBefore, 5)

	// The bad roster covers only the wrecked PSP's account: 2 kept, 1 stalled
	// (queues the deferred vault delete), 2 silently absent. Three cancellations
	// is the most the cancellation cap allows against a five-row book, and that
	// cap is a guard, not an obstacle — the incident this reverses is the same
	// shape at any size.
	snap := badRosterSnapshot(wrecked, []int{0, 1}, []int{2}, now, entEnd)
	runID := runEnforcingPass(t, appDB, baseCtx, wrecked, snap, now)
	require.NotEqual(t, uuid.Nil, runID)

	// The damage is confined to the pulled account even before any undo — the
	// account-bound pull is what makes the run PSP-attributable in the first
	// place. If this ever regressed, the undo below would be reversing damage it
	// did not record.
	require.Equal(t, siblingBefore, readSubStates(t, appDB, baseCtx, sibling.subs),
		"an account-bound enforcing pull must not touch a sibling PSP's book")
	damaged := readSubStates(t, appDB, baseCtx, wrecked.subs)
	cancelled := 0
	for _, id := range wrecked.subs {
		if damaged[id].status == "cancelled" {
			cancelled++
		}
	}
	require.Equal(t, 3, cancelled, "the bad roster should have cancelled the 2 absent + 1 stalled subscriptions")

	// --- the plan: dry run, and it says which PSP it is confined to ------------
	plan := planFor(t, appDB, baseCtx, runID)
	require.Equal(t, DestructiveRunKindConvergeEnforce, plan.Kind)
	require.True(t, plan.Scope.PspScoped, "an account-bound run must carry its PSP")
	require.NotNil(t, plan.Scope.PspID)
	require.Equal(t, wrecked.pspID, *plan.Scope.PspID)
	require.Equal(t, dbtest.TestMerchantID.UUID(), plan.Scope.MerchantID)
	require.GreaterOrEqual(t, plan.Restorable["subscriptions"], int64(cancelled),
		"every subscription the pass overwrote must be restorable")
	require.Equal(t, plan.Restorable["subscriptions"], plan.ExpectedRows())
	require.Equal(t, 1, plan.IntentsUnfired, "the queued vault delete would be superseded")
	require.True(t, plan.Complete(), "nothing fired yet, so the plan must promise a complete reversal")

	// A plan mutates nothing. This is the whole point of dry-run-by-default.
	require.Equal(t, damaged, readSubStates(t, appDB, baseCtx, wrecked.subs), "planning an undo must change nothing")

	// --- the typed confirmation ------------------------------------------------
	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		_, e := UndoRun(ctx, appDB, runID, "operator", plan.ExpectedRows()+1, nil)
		require.ErrorAs(t, e, &ErrExpectedRowsMismatch{}, "a wrong row count must refuse the apply")
		return nil
	}))
	require.Equal(t, damaged, readSubStates(t, appDB, baseCtx, wrecked.subs), "a refused apply must change nothing")

	// --- the apply -------------------------------------------------------------
	var res UndoResult
	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		var e error
		res, e = UndoRun(ctx, appDB, runID, "operator", plan.ExpectedRows(), nil)
		return e
	}))
	require.True(t, res.Complete())
	require.Equal(t, plan.Restorable["subscriptions"], res.Restored["subscriptions"],
		"the apply must restore exactly what the plan promised")
	require.Equal(t, 1, res.IntentsSuperseded)

	// (a) the wrecked account's book is back, field by field.
	require.Equal(t, wreckedBefore, readSubStates(t, appDB, baseCtx, wrecked.subs))
	// (b) the sibling account never moved — not during the damage, not during the
	// undo. Per-PSP recovery means exactly this.
	require.Equal(t, siblingBefore, readSubStates(t, appDB, baseCtx, sibling.subs),
		"a PSP-scoped undo must be incapable of touching a sibling PSP's book")

	// (c) the sibling's rows carry no stamp from this run, and no before-image of
	// theirs was ever captured: the scope held at the recording end too.
	var siblingImages int
	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		return appDB.Qx(ctx).QueryRow(ctx,
			`SELECT count(*) FROM openrails.destructive_run_before_images WHERE destructive_run_id=$1 AND row_id = ANY($2)`,
			runID, sibling.subs).Scan(&siblingImages)
	}))
	require.Zero(t, siblingImages, "a run bound to one PSP must never capture another PSP's rows")
}

// TestUndoRun_IsInvisibleToAnotherMerchant is the per-merchant half. Scope is not
// a flag the caller passes and could get wrong: the run is loaded through the
// merchant-scoped connection, so a different merchant's connection cannot see it
// at all — the undo refuses before it can compute a plan, let alone write.
func TestUndoRun_IsInvisibleToAnotherMerchant(t *testing.T) {
	appDB := startReconcilePostgres(t)
	baseCtx := merchant.WithID(context.Background(), dbtest.TestMerchantID)
	now := time.Now().UTC().Truncate(time.Second)
	periodEnd := now.Add(-60 * 24 * time.Hour)
	f := seedConvergeCohort(t, appDB, baseCtx, 3, periodEnd.Add(-30*24*time.Hour), periodEnd, now.Add(30*24*time.Hour))

	runID := runEnforcingPass(t, appDB, baseCtx, f, badRosterSnapshot(f, []int{0, 1}, nil, now, now.Add(30*24*time.Hour)), now)
	require.NotEqual(t, uuid.Nil, runID)

	otherCtx := merchant.WithID(context.Background(), merchant.ID(uuid.New()))
	require.NoError(t, appDB.RunInMerchantConn(otherCtx, func(ctx context.Context) error {
		_, e := PlanUndoRun(ctx, appDB, runID)
		require.ErrorContains(t, e, "not found for this merchant")
		_, e = UndoRun(ctx, appDB, runID, "operator", 0, nil)
		require.ErrorContains(t, e, "not found for this merchant")
		return nil
	}))

	// And the rows are still damaged: nothing leaked across the boundary in
	// either direction.
	var stillCancelled int
	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		return appDB.Qx(ctx).QueryRow(ctx,
			`SELECT count(*) FROM openrails.subscriptions WHERE id = ANY($1) AND status='cancelled'`, f.subs).Scan(&stillCancelled)
	}))
	require.Positive(t, stillCancelled)
}

// TestUndoRun_NeverResurrectsAnIndependentlyDeletedRow: between the bad pass and
// the undo, a prune tombstones one of the overwritten subscriptions. That row now
// belongs to the PRUNE's reversal, not this one. Rewriting its values here would
// edit a row nobody can see, and clearing its tombstone would resurrect a row a
// different, deliberate operation removed. The undo must skip it — and say so,
// because a silent skip is how a partial recovery gets reported as a complete one.
func TestUndoRun_NeverResurrectsAnIndependentlyDeletedRow(t *testing.T) {
	appDB := startReconcilePostgres(t)
	baseCtx := merchant.WithID(context.Background(), dbtest.TestMerchantID)
	now := time.Now().UTC().Truncate(time.Second)
	periodEnd := now.Add(-60 * 24 * time.Hour)
	entEnd := now.Add(30 * 24 * time.Hour)
	f := seedConvergeCohort(t, appDB, baseCtx, 5, periodEnd.Add(-30*24*time.Hour), periodEnd, entEnd)

	before := readSubStates(t, appDB, baseCtx, f.subs)
	runID := runEnforcingPass(t, appDB, baseCtx, f, badRosterSnapshot(f, []int{0, 1}, []int{2}, now, entEnd), now)

	damaged := readSubStates(t, appDB, baseCtx, f.subs)
	var overwritten []uuid.UUID
	for _, id := range f.subs {
		if damaged[id].status == "cancelled" {
			overwritten = append(overwritten, id)
		}
	}
	require.GreaterOrEqual(t, len(overwritten), 2)
	tombstoned := overwritten[0]

	// How many rows the undo would have restored had nobody else touched them.
	var imagesBefore int64
	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		return appDB.Qx(ctx).QueryRow(ctx,
			`SELECT count(*) FROM openrails.destructive_run_before_images
			  WHERE destructive_run_id=$1 AND table_name='subscriptions' AND restored_at IS NULL`, runID).Scan(&imagesBefore)
	}))
	require.Positive(t, imagesBefore)

	// A LATER prune takes that row: soft-deleted, stamped with its own run.
	pruneRunID := uuid.New()
	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		if _, err := appDB.Qx(ctx).Exec(ctx,
			`INSERT INTO openrails.destructive_runs (id, merchant_id, psp_id, kind, actor, dry_run, status)
			 VALUES ($1,$2,$3,'prune','operator',false,'completed')`,
			pruneRunID, dbtest.TestMerchantID.UUID(), f.pspID); err != nil {
			return err
		}
		_, err := appDB.Qx(ctx).Exec(ctx,
			`UPDATE openrails.subscriptions SET deleted_at = now(), destructive_run_id = $2 WHERE id = $1`,
			tombstoned, pruneRunID)
		return err
	}))
	t.Cleanup(func() {
		_ = appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
			_, _ = appDB.Qx(ctx).Exec(ctx, `UPDATE openrails.subscriptions SET deleted_at=NULL, destructive_run_id=NULL WHERE id=$1`, tombstoned)
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.destructive_runs WHERE id=$1`, pruneRunID)
			return nil
		})
	})

	// The plan excludes the tombstoned row from what it promises, and reports it.
	plan := planFor(t, appDB, baseCtx, runID)
	require.Equal(t, int64(1), plan.SubscriptionsTombstoned)
	require.Equal(t, imagesBefore-1, plan.Restorable["subscriptions"],
		"the tombstoned row must drop out of what the plan promises")

	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		res, e := UndoRun(ctx, appDB, runID, "operator", plan.ExpectedRows(), nil)
		if e == nil {
			require.Equal(t, imagesBefore-1, res.Restored["subscriptions"])
		}
		return e
	}))

	// The tombstoned row is untouched: still deleted, still stamped with the
	// PRUNE's run, and its VALUES are still the ones the bad pass wrote — the
	// converge undo did not reach in and edit an invisible row.
	var (
		gotDeleted *time.Time
		gotRun     *uuid.UUID
		gotStatus  string
	)
	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		return appDB.Qx(ctx).QueryRow(ctx,
			`SELECT deleted_at, destructive_run_id, status::text FROM openrails.subscriptions WHERE id=$1`, tombstoned).
			Scan(&gotDeleted, &gotRun, &gotStatus)
	}))
	require.NotNil(t, gotDeleted, "a converge undo must never clear a tombstone another run set")
	require.NotNil(t, gotRun)
	require.Equal(t, pruneRunID, *gotRun, "the row still belongs to the prune's reversal")
	require.Equal(t, damaged[tombstoned].status, gotStatus)

	// Everything else came back.
	restored := readSubStates(t, appDB, baseCtx, f.subs)
	for _, id := range f.subs {
		if id == tombstoned {
			continue
		}
		require.Equal(t, before[id], restored[id], "subscription %s was not restored", id)
	}
}

// TestUndoRun_RefusesTheNeverRollbackableClasses: the register, enforced at the
// verb. A merchant purge hard-DELETEs append-only Class A rows, so no local undo
// reverses it; a kind that was declared in the schema but never converted has no
// undo at all. Both must be refused BY NAME with what to reach for instead,
// because the alternative — a reversal that restores nothing and still marks the
// run reversed — is a lie told at the worst possible moment.
func TestUndoRun_RefusesTheNeverRollbackableClasses(t *testing.T) {
	appDB := startReconcilePostgres(t)
	baseCtx := merchant.WithID(context.Background(), dbtest.TestMerchantID)

	seed := func(kind string) uuid.UUID {
		id := uuid.New()
		require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
			_, err := appDB.Qx(ctx).Exec(ctx,
				`INSERT INTO openrails.destructive_runs (id, merchant_id, kind, actor, dry_run, status)
				 VALUES ($1,$2,$3,'operator',false,'completed')`, id, dbtest.TestMerchantID.UUID(), kind)
			return err
		}))
		t.Cleanup(func() {
			_ = appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
				_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.destructive_runs WHERE id=$1`, id)
				return nil
			})
		})
		return id
	}

	purge := seed("merchant_delete")
	unconverted := seed("catalog_push")

	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		_, e := PlanUndoRun(ctx, appDB, purge)
		require.ErrorContains(t, e, "NOT reversible")
		require.ErrorContains(t, e, "PITR", "the refusal must name what the operator reaches for instead")
		_, e = UndoRun(ctx, appDB, purge, "operator", 0, nil)
		require.ErrorContains(t, e, "NOT reversible")

		_, e = PlanUndoRun(ctx, appDB, unconverted)
		require.ErrorContains(t, e, "not yet converted")
		_, e = UndoRun(ctx, appDB, unconverted, "operator", 0, nil)
		require.ErrorContains(t, e, "not yet converted")

		// Neither run was marked reversed by the refusal.
		var purgeStatus, unconvertedStatus string
		if err := appDB.Qx(ctx).QueryRow(ctx, `SELECT status FROM openrails.destructive_runs WHERE id=$1`, purge).Scan(&purgeStatus); err != nil {
			return err
		}
		if err := appDB.Qx(ctx).QueryRow(ctx, `SELECT status FROM openrails.destructive_runs WHERE id=$1`, unconverted).Scan(&unconvertedStatus); err != nil {
			return err
		}
		require.Equal(t, "completed", purgeStatus)
		require.Equal(t, "completed", unconvertedStatus)
		return nil
	}))
}

// TestNeverRollbackableTablesAreNotWritableByTheAppRole is migration 0036: the
// register held at the privilege layer, where it survives a query that never met
// the Go file. Checked against the LIVE database as the unprivileged role the
// application actually runs as.
func TestNeverRollbackableTablesAreNotWritableByTheAppRole(t *testing.T) {
	appDB := startReconcilePostgres(t)
	ctx := context.Background()

	// table -> privileges openrails_app must NOT hold.
	forbidden := map[string][]string{
		"ledger_transfers":                {"UPDATE", "DELETE"},
		"ledger_accounts":                 {"UPDATE", "DELETE"},
		"grants":                          {"UPDATE", "DELETE"},
		"subscription_status_transitions": {"UPDATE", "DELETE"},
		// or#859/0036: the record of what we did to the outside world is never
		// edited, and the forensic run ledger is never erased.
		"rail_mutation_logs":  {"UPDATE"},
		"reconciliation_runs": {"DELETE"},
	}
	for table, privs := range forbidden {
		for _, priv := range privs {
			var has bool
			require.NoError(t, appDB.Pool().QueryRow(ctx,
				`SELECT has_table_privilege('openrails_app', 'openrails.'||$1, $2)`, table, priv).Scan(&has))
			require.False(t, has, "openrails_app must not hold %s on openrails.%s: %s",
				priv, table, NeverRollbackableTables[table])
		}
	}

	// webhook_events keeps DELETE on purpose — the retention sweep needs it — so
	// the register records the consequence rather than pretending otherwise: a
	// run stops being safely reversible once it reaches past dedup retention.
	var retentionDelete bool
	require.NoError(t, appDB.Pool().QueryRow(ctx,
		`SELECT has_table_privilege('openrails_app', 'openrails.webhook_events', 'DELETE')`).Scan(&retentionDelete))
	require.True(t, retentionDelete, "the webhook_events retention sweep needs DELETE; see migration 0036")
	var webhookUpdate bool
	require.NoError(t, appDB.Pool().QueryRow(ctx,
		`SELECT has_table_privilege('openrails_app', 'openrails.webhook_events', 'UPDATE')`).Scan(&webhookUpdate))
	require.False(t, webhookUpdate, "dedup truth is never edited in place")
}
