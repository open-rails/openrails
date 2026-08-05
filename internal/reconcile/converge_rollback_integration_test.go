//go:build integration

package reconcile

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/pkg/merchant"
)

// or#859 tier 1, converge-enforce slice. The measured incident: a bad NMI
// roster drove an ENFORCING pull to cancel a merchant's book and queue deferred
// vault deletes behind it. Before this change the pass opened no destructive
// run and captured nothing — the damage was UPDATEs, and or#858's soft-delete
// tombstones cannot undo an UPDATE — so there was nothing to reverse by.
//
// These tests assert the whole contract, including the parts that are
// deliberately NOT undone.

type convergeRunFixture struct {
	pspID    uuid.UUID
	binding  PSPBinding
	subs     []uuid.UUID
	railSubs []string
	customer []uuid.UUID
	suffix   string
}

// seedConvergeCohort creates n account-bound NMI subscriptions in `active`,
// each with a live entitlement window, all attributed to one PSP so the pass —
// and therefore the run it opens — is account-bound.
// entEnd is the access window's end, deliberately in the FUTURE while the
// billing period has lapsed: the paid runway a customer still holds when a bad
// pass cancels them. That runway is what the incident took away.
func seedConvergeCohort(t *testing.T, appDB *db.DB, baseCtx context.Context, n int, periodStart, periodEnd, entEnd time.Time) convergeRunFixture {
	t.Helper()
	merchantID := dbtest.TestMerchantID.UUID()
	f := convergeRunFixture{pspID: uuid.New(), suffix: uuid.NewString()[:8]}

	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		exec := func(sql string, args ...any) {
			_, err := appDB.Qx(ctx).Exec(ctx, sql, args...)
			require.NoError(t, err)
		}
		exec(`INSERT INTO openrails.psps (id, merchant_id, rail, account_id, archived) VALUES ($1,$2,'nmi',$3,false)`,
			f.pspID, merchantID, "acct-cr-"+f.suffix)
		for i := 0; i < n; i++ {
			cust := dbtest.EnsureCustomerIDPgx(ctx, t, appDB.Qx(ctx), uuid.NewString())
			prod, price, sub := uuid.New(), uuid.New(), uuid.New()
			rs := fmt.Sprintf("rs-cr-%s-%d", f.suffix, i)
			key := fmt.Sprintf("cr-%s-%d", f.suffix, i)
			exec(`INSERT INTO openrails.products (id,key,display_name,entitlements_spec,merchant_id)
			      VALUES ($1,$2,$2,jsonb_build_object('premium',null),$3)`, prod, key, merchantID)
			exec(`INSERT INTO openrails.prices (id,product_id,amount,currency,access_duration_hours,auto_renew,merchant_id)
			      VALUES ($1,$2,5000000,'USD',720,true,$3)`, price, prod, merchantID)
			exec(`INSERT INTO openrails.subscriptions
			        (id,merchant_id,customer_id,product_id,price_id,status,rail,rail_subscription_id,psp_id,
			         started_at,current_period_starts_at,current_period_ends_at,entitlements_spec_snapshot)
			      VALUES ($1,$2,$3,$4,$5,'active','nmi',$6,$7,$8,$8,$9,jsonb_build_object('premium',null))`,
				sub, merchantID, cust, prod, price, rs, f.pspID, periodStart, periodEnd)
			exec(`INSERT INTO openrails.entitlements (merchant_id,customer_id,entitlement,start_at,end_at,source_id,source_type)
			      VALUES ($1,$2,'premium',$3,$4,$5,'subscription')`,
				merchantID, cust, periodStart, entEnd, sub)
			f.subs = append(f.subs, sub)
			f.railSubs = append(f.railSubs, rs)
			f.customer = append(f.customer, cust)
		}
		return nil
	}))

	t.Cleanup(func() {
		_ = appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
			for _, sub := range f.subs {
				_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.rail_intents WHERE subscription_id=$1`, sub)
				_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.entitlements WHERE source_id=$1`, sub)
				_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.payments WHERE subscription_id=$1`, sub)
				_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.subscription_status_transitions WHERE subscription_id=$1`, sub)
				_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.destructive_run_before_images WHERE row_id=$1`, sub)
			}
			_, _ = appDB.Qx(ctx).Exec(ctx,
				`DELETE FROM openrails.destructive_run_before_images
				  WHERE destructive_run_id IN (SELECT id FROM openrails.destructive_runs WHERE psp_id=$1)`, f.pspID)
			for _, sub := range f.subs {
				_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.subscriptions WHERE id=$1`, sub)
			}
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.destructive_runs WHERE psp_id=$1`, f.pspID)
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.prices WHERE product_id IN (SELECT id FROM openrails.products WHERE key LIKE 'cr-'||$1||'-%')`, f.suffix)
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.products WHERE key LIKE 'cr-'||$1||'-%'`, f.suffix)
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.psps WHERE id=$1`, f.pspID)
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.merchant_destructive_policy WHERE merchant_id=$1`, dbtest.TestMerchantID.UUID())
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.reconciliation_state WHERE merchant_id=$1`, dbtest.TestMerchantID.UUID())
			return nil
		})
	})
	f.binding = PSPBinding{ID: f.pspID, Rail: "nmi", AccountID: "acct-cr-" + f.suffix}
	return f
}

// badRosterSnapshot is the shape a bad snapshot takes once or#842's empty-roster
// refusal is in place: not literally empty, but MISSING most of the book. Same
// class of failure, same damage, and it still gets through every guard.
//
//   - kept[]     listed alive with a future boundary  → survives untouched
//   - stalled[]  listed past_due with a hard-declined renewal beyond the dunning
//     window → terminal cancel with the remote sub still ALIVE, which
//     is the case that queues the deferred NMI vault delete
//   - everything else is simply absent from a roster claiming to be exhaustive
//     → cancelled by the coverage-absence proof
func badRosterSnapshot(f convergeRunFixture, kept, stalled []int, now, futureEnd time.Time) *RemoteSnapshot {
	snap := &RemoteSnapshot{
		Provider:     ProviderNMI,
		FetchedAt:    now,
		Capabilities: Capabilities{Subscriptions: true, Transactions: true},
		Coverage:     SnapshotCoverage{SubscriptionsExhaustive: true},
	}
	for _, i := range kept {
		next := futureEnd
		snap.Subscriptions = append(snap.Subscriptions, RemoteSubscription{
			RailSubscriptionID: f.railSubs[i], Status: SubscriptionStatusActive, NextBillingAt: &next,
		})
	}
	for _, i := range stalled {
		snap.Subscriptions = append(snap.Subscriptions, RemoteSubscription{
			RailSubscriptionID: f.railSubs[i], Status: SubscriptionStatusPastDue,
		})
		snap.Transactions = append(snap.Transactions, RemoteTransaction{
			TransactionID:  fmt.Sprintf("txn-decline-%s-%d", f.suffix, i),
			SubscriptionID: f.railSubs[i],
			Type:           TransactionTypeSale,
			Success:        false,
			AmountCents:    5000,
			Currency:       "USD",
			// Dated at the pass's own observation: the #835 floor refuses to
			// cancel on evidence older than this deployment's first pull.
			OccurredAt: now,
			// 261 is bucket 3: the issuer withdrew the recurring mandate outright.
			// A soft decline would park as `unknown` instead — no certainty, no cancel.
			DeclineReason: "declined - stop all recurring payments",
			DeclineCode:   "261",
		})
	}
	return snap
}

func convergeEngine(appDB *db.DB, snap *RemoteSnapshot, now time.Time, withRuns bool) *Engine {
	e := &Engine{
		Fetchers: map[Provider]RailFetcher{ProviderNMI: &fakeFetcher{provider: ProviderNMI, snap: snap}},
		Store:    &PGStore{DB: appDB},
		Local:    &PGLocalStateLoader{DB: appDB},
		Writer:   &PGLocalWriter{DB: appDB},
		// The real deferred-delete path: a terminal cancel whose remote sub is
		// still alive durably queues an nmi_delete_subscription intent.
		Decisions: NewDecisionApplier(appDB, intents.NewNMIDeleteScheduler(appDB, nil, intents.OriginSystem, "converge rollback test")),
		Now:       func() time.Time { return now },
		Actor:     "or859-test",
	}
	if withRuns {
		e.Runs = &PGDestructiveRunRecorder{DB: appDB}
	}
	return e
}

type subState struct {
	status      string
	endedAt     *time.Time
	cancelledAt *time.Time
	cancelType  *string
	periodEnd   *time.Time
	graceEnds   *time.Time
	deletionAt  *time.Time
}

func readSubStates(t *testing.T, appDB *db.DB, baseCtx context.Context, ids []uuid.UUID) map[uuid.UUID]subState {
	t.Helper()
	out := map[uuid.UUID]subState{}
	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		rows, err := appDB.Qx(ctx).Query(ctx,
			`SELECT id, status::text, ended_at, cancelled_at, cancel_type, current_period_ends_at, grace_ends_at, deletion_scheduled_at
			   FROM openrails.subscriptions WHERE id = ANY($1)`, ids)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id uuid.UUID
			var s subState
			if err := rows.Scan(&id, &s.status, &s.endedAt, &s.cancelledAt, &s.cancelType, &s.periodEnd, &s.graceEnds, &s.deletionAt); err != nil {
				return err
			}
			out[id] = s
		}
		return rows.Err()
	}))
	return out
}

// appendOnlyCounts snapshots the tables a rollback must NEVER touch, at any
// scope: the money spine, the grant log, the lifecycle audit trail, and the
// forensic ledgers of what we did to the outside world.
func appendOnlyCounts(t *testing.T, appDB *db.DB, baseCtx context.Context) map[string]int {
	t.Helper()
	out := map[string]int{}
	mid := dbtest.TestMerchantID.UUID()
	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		for _, table := range []string{
			"ledger_transfers", "ledger_accounts", "grants",
			"subscription_status_transitions", "rail_intents", "rail_mutation_logs", "webhook_events",
		} {
			var n int
			if err := appDB.Qx(ctx).QueryRow(ctx,
				fmt.Sprintf(`SELECT count(*) FROM openrails.%s WHERE merchant_id = $1`, table), mid).Scan(&n); err != nil {
				return fmt.Errorf("%s: %w", table, err)
			}
			out[table] = n
		}
		return nil
	}))
	return out
}

func intentRows(t *testing.T, appDB *db.DB, baseCtx context.Context, subs []uuid.UUID) map[uuid.UUID]struct {
	ID     uuid.UUID
	Status string
	RunID  *uuid.UUID
} {
	t.Helper()
	out := map[uuid.UUID]struct {
		ID     uuid.UUID
		Status string
		RunID  *uuid.UUID
	}{}
	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		rows, err := appDB.Qx(ctx).Query(ctx,
			`SELECT id, subscription_id, status, destructive_run_id FROM openrails.rail_intents WHERE subscription_id = ANY($1)`, subs)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id, sub uuid.UUID
			var status string
			var runID *uuid.UUID
			if err := rows.Scan(&id, &sub, &status, &runID); err != nil {
				return err
			}
			out[sub] = struct {
				ID     uuid.UUID
				Status string
				RunID  *uuid.UUID
			}{id, status, runID}
		}
		return rows.Err()
	}))
	return out
}

// TestConvergeEnforceRun_IsReversible is the decisive one: the incident, then
// the undo, on the real enforce path.
func TestConvergeEnforceRun_IsReversible(t *testing.T) {
	appDB := startReconcilePostgres(t)
	baseCtx := merchant.WithID(context.Background(), dbtest.TestMerchantID)
	now := time.Now().UTC().Truncate(time.Second)
	// Every subscription's period lapsed 60 days ago: beyond the dunning window,
	// so the roster's verdict is terminal.
	periodEnd := now.Add(-60 * 24 * time.Hour)
	f := seedConvergeCohort(t, appDB, baseCtx, 6, periodEnd.Add(-30*24*time.Hour), periodEnd, now.Add(30*24*time.Hour))

	before := readSubStates(t, appDB, baseCtx, f.subs)
	require.Len(t, before, 6)
	appendOnlyBefore := appendOnlyCounts(t, appDB, baseCtx)

	// The bad snapshot: 3 kept alive, 1 stalled-and-declined (queues the vault
	// delete), 2 silently absent from a roster claiming to be exhaustive.
	snap := badRosterSnapshot(f, []int{0, 1, 2}, []int{3}, now, now.Add(30*24*time.Hour))
	eng := convergeEngine(appDB, snap, now, true)

	var runID string
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
		runID = res.Summary.Providers[string(ProviderNMI)].DestructiveRunID
		return nil
	}))

	// --- the run exists at all. Before this change converge-enforce opened NO
	// destructive run and captured NOTHING: this line is the whole gap.
	require.NotEmpty(t, runID, "an enforcing pass that overwrote subscription state opened no destructive run — there is nothing to reverse by")
	destRunID := uuid.MustParse(runID)

	// --- damage as measured -----------------------------------------------------
	damaged := readSubStates(t, appDB, baseCtx, f.subs)
	cancelled := 0
	for _, id := range f.subs {
		if damaged[id].status == "cancelled" {
			cancelled++
		}
	}
	require.Equal(t, 3, cancelled, "the bad roster should have cancelled the 2 absent + 1 stalled subscriptions")

	var liveEnts int
	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		return appDB.Qx(ctx).QueryRow(ctx,
			`SELECT count(*) FROM openrails.entitlements WHERE source_id = ANY($1) AND revoked_at IS NULL`, f.subs).Scan(&liveEnts)
	}))
	require.Less(t, liveEnts, 6, "the cancellations should have revoked access — otherwise this test is not reproducing the incident")

	// The run header: merchant-scoped, PSP-bound, carrying the coverage proof
	// that authorised it.
	var (
		gotKind    string
		gotPsp     *uuid.UUID
		gotCovProv bool
		gotStatus  string
		gotExpect  *int64
	)
	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		return appDB.Qx(ctx).QueryRow(ctx,
			`SELECT kind, psp_id, status, expected_rows, (coverage->>'subscriptions_exhaustive')::boolean
			   FROM openrails.destructive_runs WHERE id=$1`, destRunID).
			Scan(&gotKind, &gotPsp, &gotStatus, &gotExpect, &gotCovProv)
	}))
	require.Equal(t, DestructiveRunKindConvergeEnforce, gotKind)
	require.NotNil(t, gotPsp)
	require.Equal(t, f.pspID, *gotPsp, "the pass was account-bound, so the run must be too")
	require.True(t, gotCovProv, "the absence proof that authorised the pass must be stored WITH the run")
	require.Equal(t, "completed", gotStatus)
	require.NotNil(t, gotExpect)

	// Before-images: one per overwritten subscription, plus the entitlement
	// windows the pass closed.
	var imgSubs, imgEnts int
	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		return appDB.Qx(ctx).QueryRow(ctx,
			`SELECT count(*) FILTER (WHERE table_name='subscriptions'), count(*) FILTER (WHERE table_name='entitlements')
			   FROM openrails.destructive_run_before_images WHERE destructive_run_id=$1`, destRunID).Scan(&imgSubs, &imgEnts)
	}))
	require.GreaterOrEqual(t, imgSubs, 3, "every subscription the pass overwrote needs a before-image")
	require.Positive(t, imgEnts, "the entitlement windows the pass closed are captured as evidence")

	// The unfired provider write: a deferred NMI vault delete, attributed to
	// this run and still pending.
	queued := intentRows(t, appDB, baseCtx, f.subs)
	stalledIntent, ok := queued[f.subs[3]]
	require.True(t, ok, "the stalled subscription's terminal cancel should have queued a deferred NMI vault delete")
	require.Equal(t, "pending", stalledIntent.Status)
	require.NotNil(t, stalledIntent.RunID, "an intent a destructive run queued must be attributable to it")
	require.Equal(t, destRunID, *stalledIntent.RunID)

	// --- the reverse ------------------------------------------------------------
	var rollback ConvergeRollbackResult
	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		var e error
		rollback, e = RollbackConvergeEnforceRun(ctx, appDB, destRunID, "operator", nil)
		return e
	}))

	require.True(t, rollback.Complete(), "no intent fired, so this reversal must report itself complete: %+v", rollback)
	require.Equal(t, 1, rollback.IntentsSuperseded)
	require.GreaterOrEqual(t, rollback.SubscriptionsRestored, int64(3))
	require.True(t, rollback.EnforcementDisarmed)

	// (a) every subscription is back to its prior state, field by field.
	restored := readSubStates(t, appDB, baseCtx, f.subs)
	for _, id := range f.subs {
		require.Equal(t, before[id], restored[id], "subscription %s was not restored to its prior state", id)
	}

	// (b) the unfired vault delete is superseded — a forward lifecycle
	// transition, not a deletion: the row is still there.
	after := intentRows(t, appDB, baseCtx, f.subs)
	require.Equal(t, "superseded", after[f.subs[3]].Status)
	require.Equal(t, stalledIntent.ID, after[f.subs[3]].ID, "the intent row must be transitioned, never deleted")

	// (c) entitlements are INVALIDATED, never restored: no image is replayed, and
	// the rows the run closed are soft-deleted so Converge rebuilds them from the
	// grant log. (The rebuild itself is proven in the external-package test,
	// which can import `converge`.)
	var unreplayed int
	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		return appDB.Qx(ctx).QueryRow(ctx,
			`SELECT count(*) FROM openrails.destructive_run_before_images
			  WHERE destructive_run_id=$1 AND table_name='entitlements' AND restored_at IS NULL`, destRunID).Scan(&unreplayed)
	}))
	require.Equal(t, imgEnts, unreplayed, "entitlement before-images must be evidence only; restoring one directly could make it disagree with its grant")
	require.Equal(t, int64(imgEnts), rollback.EntitlementsInvalidated)
	require.False(t, rollback.Recomputed, "no Recomputer was wired, so the reverse must not claim it re-derived")
	var invalidatedStamped int
	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		return appDB.Qx(ctx).QueryRow(ctx,
			`SELECT count(*) FROM openrails.entitlements WHERE destructive_run_id=$1 AND deleted_at IS NOT NULL`, destRunID).Scan(&invalidatedStamped)
	}))
	require.Equal(t, imgEnts, invalidatedStamped, "an invalidation must itself be attributable to exactly one run")

	// (d) the append-only spine is untouched. rail_intents included: a status
	// transition is not a row loss.
	require.Equal(t, appendOnlyBefore["ledger_transfers"], appendOnlyCounts(t, appDB, baseCtx)["ledger_transfers"])
	appendOnlyAfter := appendOnlyCounts(t, appDB, baseCtx)
	for _, table := range []string{"ledger_transfers", "ledger_accounts", "grants", "webhook_events", "rail_mutation_logs"} {
		require.Equal(t, appendOnlyBefore[table], appendOnlyAfter[table], "%s must never be rolled back", table)
	}
	require.GreaterOrEqual(t, appendOnlyAfter["subscription_status_transitions"], appendOnlyBefore["subscription_status_transitions"],
		"the lifecycle audit trail is append-only: a reversal adds to it, never rewinds it")
	require.GreaterOrEqual(t, appendOnlyAfter["rail_intents"], appendOnlyBefore["rail_intents"],
		"the intent ledger is append-only: superseding is a forward transition, not a delete")

	// (e) the post-rollback gates are closed: the book is definitionally
	// incomplete now, so nothing may retract against it and the next pull is
	// advisory until an operator re-arms.
	var armed *time.Time
	var destructiveEnabled bool
	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		return appDB.Qx(ctx).QueryRow(ctx,
			`SELECT enforce_armed_at, destructive_actions_enabled FROM openrails.merchant_destructive_policy WHERE merchant_id=$1`,
			dbtest.TestMerchantID.UUID()).Scan(&armed, &destructiveEnabled)
	}))
	require.Nil(t, armed, "first-enforce arming must be cleared so the post-rollback pull runs advisory")
	require.False(t, destructiveEnabled)

	// The run is marked reversed, and reversing twice is refused.
	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		_, e := RollbackConvergeEnforceRun(ctx, appDB, destRunID, "operator", nil)
		require.ErrorContains(t, e, "already reversed")
		return nil
	}))
}

// TestConvergeEnforceRollback_ReportsFiredIntentsAsIrreversibleDivergence is the
// honest-reporting half. An intent the runner already executed deleted a vault
// entry at NMI. Nothing local can bring that back, and a reverse that counted it
// among the writes it "undid" would be lying to the operator at exactly the
// moment they most need the truth.
func TestConvergeEnforceRollback_ReportsFiredIntentsAsIrreversibleDivergence(t *testing.T) {
	appDB := startReconcilePostgres(t)
	baseCtx := merchant.WithID(context.Background(), dbtest.TestMerchantID)
	now := time.Now().UTC().Truncate(time.Second)
	periodEnd := now.Add(-60 * 24 * time.Hour)
	f := seedConvergeCohort(t, appDB, baseCtx, 4, periodEnd.Add(-30*24*time.Hour), periodEnd, now.Add(30*24*time.Hour))

	// Two stalled subscriptions, so two deferred vault deletes are queued.
	snap := badRosterSnapshot(f, []int{0, 1}, []int{2, 3}, now, now.Add(30*24*time.Hour))
	eng := convergeEngine(appDB, snap, now, true)

	var destRunID uuid.UUID
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
		destRunID = uuid.MustParse(res.Summary.Providers[string(ProviderNMI)].DestructiveRunID)
		return nil
	}))

	queued := intentRows(t, appDB, baseCtx, f.subs)
	fired, stillQueued := queued[f.subs[2]], queued[f.subs[3]]
	require.NotEqual(t, uuid.Nil, fired.ID)
	require.NotEqual(t, uuid.Nil, stillQueued.ID)

	// The runner got to one of them first: the NMI vault entry is gone.
	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		_, err := appDB.Qx(ctx).Exec(ctx,
			`UPDATE openrails.rail_intents SET status='succeeded', executed_at=now() WHERE id=$1`, fired.ID)
		return err
	}))

	var rollback ConvergeRollbackResult
	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		var e error
		rollback, e = RollbackConvergeEnforceRun(ctx, appDB, destRunID, "operator", nil)
		return e
	}))

	require.False(t, rollback.Complete(), "a fired provider write means the reversal is NOT complete")
	require.Equal(t, 1, rollback.IntentsSuperseded, "only the unfired intent may be counted as neutralised")
	require.Len(t, rollback.IntentsIrreversible, 1)
	require.Equal(t, fired.ID, rollback.IntentsIrreversible[0].IntentID)
	require.Equal(t, "succeeded", rollback.IntentsIrreversible[0].Status)
	require.Contains(t, rollback.IntentsIrreversible[0].Consequence, "vault",
		"the report must spell out the provider-side consequence, not just a count")

	// The fired intent is left exactly as the runner left it — a reverse never
	// rewrites the record of what reached the outside world.
	after := intentRows(t, appDB, baseCtx, f.subs)
	require.Equal(t, "succeeded", after[f.subs[2]].Status)
	require.Equal(t, "superseded", after[f.subs[3]].Status)
}

// TestConvergeEnforceRollback_InFlightIntentIsAmbiguousNotUndone: the executor
// holds a lease and may already be on the wire. Superseding it would be a claim
// we cannot support, so it is reported as ambiguous and left to the verifier.
// This is the race, decided the other way.
func TestConvergeEnforceRollback_InFlightIntentIsAmbiguousNotUndone(t *testing.T) {
	appDB := startReconcilePostgres(t)
	baseCtx := merchant.WithID(context.Background(), dbtest.TestMerchantID)
	now := time.Now().UTC().Truncate(time.Second)
	periodEnd := now.Add(-60 * 24 * time.Hour)
	f := seedConvergeCohort(t, appDB, baseCtx, 4, periodEnd.Add(-30*24*time.Hour), periodEnd, now.Add(30*24*time.Hour))

	snap := badRosterSnapshot(f, []int{0, 1}, []int{2, 3}, now, now.Add(30*24*time.Hour))
	eng := convergeEngine(appDB, snap, now, true)

	var destRunID uuid.UUID
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
		destRunID = uuid.MustParse(res.Summary.Providers[string(ProviderNMI)].DestructiveRunID)
		return nil
	}))

	claimed := intentRows(t, appDB, baseCtx, f.subs)[f.subs[2]]
	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		_, err := appDB.Qx(ctx).Exec(ctx,
			`UPDATE openrails.rail_intents SET status='in_flight', claimed_until=now()+interval '5 minutes' WHERE id=$1`, claimed.ID)
		return err
	}))

	var rollback ConvergeRollbackResult
	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		var e error
		rollback, e = RollbackConvergeEnforceRun(ctx, appDB, destRunID, "operator", nil)
		return e
	}))

	require.False(t, rollback.Complete())
	require.Empty(t, rollback.IntentsIrreversible)
	require.Len(t, rollback.IntentsAmbiguous, 1)
	require.Equal(t, "in_flight", rollback.IntentsAmbiguous[0].Status)

	// Left alone: the executor owns that lease and its own relevance re-check
	// decides. Stealing it here would race a live HTTP call.
	require.Equal(t, "in_flight", intentRows(t, appDB, baseCtx, f.subs)[f.subs[2]].Status)
}

// TestConvergeEnforce_RefusesWithoutARunRecorder is the no-bypass guard: an
// enforce pass that would overwrite subscription state and cannot record how to
// undo it does not run. The alternative — quietly doing irreversible damage
// because a wiring seam was left nil — is the failure mode or#859 exists to
// close.
func TestConvergeEnforce_RefusesWithoutARunRecorder(t *testing.T) {
	appDB := startReconcilePostgres(t)
	baseCtx := merchant.WithID(context.Background(), dbtest.TestMerchantID)
	now := time.Now().UTC().Truncate(time.Second)
	periodEnd := now.Add(-60 * 24 * time.Hour)
	f := seedConvergeCohort(t, appDB, baseCtx, 4, periodEnd.Add(-30*24*time.Hour), periodEnd, now.Add(30*24*time.Hour))

	before := readSubStates(t, appDB, baseCtx, f.subs)
	snap := badRosterSnapshot(f, []int{0, 1}, []int{2}, now, now.Add(30*24*time.Hour))
	eng := convergeEngine(appDB, snap, now, false) // no recorder wired

	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		_, err := eng.Run(ctx, RunParams{
			Mode:        ModeEnforce,
			Mutations:   &LocalMutationPolicy{Overwrite: true},
			Providers:   []Provider{ProviderNMI},
			PSPs:        map[Provider]PSPBinding{ProviderNMI: f.binding},
			PSPCoverage: map[Provider]PSPCoverage{ProviderNMI: {Declared: 1, Pulled: 1, Binding: f.binding}},
		})
		require.ErrorContains(t, err, "DestructiveRunRecorder")
		return nil
	}))

	require.Equal(t, before, readSubStates(t, appDB, baseCtx, f.subs),
		"a pass that cannot record its undo must change nothing")
}
