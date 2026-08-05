//go:build integration

package reconcile

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/destructive"
	"github.com/open-rails/openrails/pkg/merchant"
)

// #835 the second pass. The arming gate makes the FIRST pull advisory, but
// arming is one flip and reading the advisory findings carefully is not — so
// the realistic failure is an operator who arms, and a second pass that then
// acts on records which arrived with the imported book.
//
// The shape below is the one that still cancelled after #821 closed the
// stale-date hole: a legacy NMI row wedged past its next billing date whose
// history contains a HARD decline (NMI 261, "stop all recurring payments").
// That is genuine certainty by KIND — and it is inherited: nothing this
// deployment observed corroborates it.

// armMerchantWithFirstPull puts the merchant in the post-arming state: surveyed
// by one completed pull at `firstPull`, then blessed by an operator. Restores
// the policy row afterwards so the floor does not leak into sibling tests.
func armMerchantWithFirstPull(t *testing.T, appDB *db.DB, baseCtx context.Context, firstPull time.Time) {
	t.Helper()
	gate := destructive.New(appDB)
	mid := dbtest.TestMerchantID.UUID()
	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		require.NoError(t, gate.RecordFirstPull(ctx, mid, firstPull))
		require.NoError(t, gate.Arm(ctx, mid, firstPull.Add(time.Hour), "operator", "reviewed the advisory findings"))
		require.Equal(t, firstPull.UTC(), gate.EvidenceFloor(ctx, mid), "the floor is the first completed pull")
		return nil
	}))
	t.Cleanup(func() {
		_ = appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.merchant_destructive_policy WHERE merchant_id=$1`, mid)
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.reconciliation_findings WHERE finding_type=$1`, string(FindingEvidenceStale))
			return nil
		})
	})
}

// legacyDeclineSnapshot: every cohort row on the roster, wedged, each carrying
// one non-retryable decline at `declinedAt`.
func legacyDeclineSnapshot(now time.Time, periodEnd time.Time, c guardCohort, declinedAt time.Time) *RemoteSnapshot {
	snap := &RemoteSnapshot{
		Provider:     ProviderNMI,
		FetchedAt:    now,
		Capabilities: Capabilities{Subscriptions: true},
	}
	wedged := periodEnd
	for _, rs := range c.railSubs {
		snap.Subscriptions = append(snap.Subscriptions, RemoteSubscription{
			RailSubscriptionID: rs, Status: SubscriptionStatusPastDue, NextBillingAt: &wedged,
		})
		snap.Transactions = append(snap.Transactions, RemoteTransaction{
			TransactionID: "txn-" + rs, SubscriptionID: rs,
			Type: TransactionTypeDecline, OccurredAt: declinedAt, DeclineCode: "261",
		})
	}
	return snap
}

func enforcingPullEngine(appDB *db.DB, snap *RemoteSnapshot, now time.Time) *Engine {
	return &Engine{
		Fetchers:  map[Provider]RailFetcher{ProviderNMI: &fakeFetcher{provider: ProviderNMI, snap: snap}},
		Store:     &PGStore{DB: appDB},
		Local:     &PGLocalStateLoader{DB: appDB},
		Writer:    &PGLocalWriter{DB: appDB},
		Decisions: NewDecisionApplier(appDB, nil),
		Runs:      &PGDestructiveRunRecorder{DB: appDB},
		Policy:    destructive.New(appDB),
		Now:       func() time.Time { return now },
	}
}

func runEnforcingPull(t *testing.T, appDB *db.DB, baseCtx context.Context, eng *Engine, psp PSPBinding) {
	t.Helper()
	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		_, err := eng.Run(ctx, RunParams{
			Mode:      ModeEnforce,
			Mutations: &LocalMutationPolicy{Insert: false, Overwrite: true},
			Providers: []Provider{ProviderNMI},
			PSPs:      map[Provider]PSPBinding{ProviderNMI: psp},
		})
		return err
	}))
}

func subscriptionStatuses(t *testing.T, appDB *db.DB, baseCtx context.Context, c guardCohort) map[string]int {
	t.Helper()
	out := map[string]int{}
	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		rows, err := appDB.Qx(ctx).Query(ctx,
			`SELECT status::text, count(*) FROM openrails.subscriptions WHERE id = ANY($1) GROUP BY 1`, c.subs)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var s string
			var n int
			require.NoError(t, rows.Scan(&s, &n))
			out[s] = n
		}
		return rows.Err()
	}))
	return out
}

// The decisive case: an ARMED merchant, a SECOND enforcing pass, evidence that
// predates the first pull. Zero cancellations, every row parked `unknown` with
// its access intact, and the withheld action visible in the operator queue.
func TestPull_ArmedMerchantWillNotCancelOnEvidencePredatingTheFirstPull(t *testing.T) {
	appDB := startReconcilePostgres(t)
	baseCtx := merchant.WithID(context.Background(), dbtest.TestMerchantID)
	now := time.Now().UTC().Truncate(time.Second)

	armMerchantWithFirstPull(t, appDB, baseCtx, now.Add(-7*24*time.Hour))

	periodEnd := now.Add(-60 * 24 * time.Hour) // far beyond the 14d dunning window
	cohort := seedGuardCohort(t, appDB, baseCtx, 3, "active", periodEnd)

	// The decline landed 50 days ago — 43 days before this deployment first
	// looked at the merchant. It came with the imported book.
	snap := legacyDeclineSnapshot(now, periodEnd, cohort, now.Add(-50*24*time.Hour))
	runEnforcingPull(t, appDB, baseCtx, enforcingPullEngine(appDB, snap, now), cohort.psp)

	cancelled, live := guardCounts(t, appDB, baseCtx, cohort)
	require.Zero(t, cancelled, "%d subscriptions were cancelled on evidence this deployment never observed", cancelled)
	require.Equal(t, len(cohort.subs), live, "entitlements survive: access is never lost to inherited data")
	require.Equal(t, map[string]int{"unknown": len(cohort.subs)}, subscriptionStatuses(t, appDB, baseCtx, cohort),
		"a floored row parks as `unknown` — provider verification resolves it, it is not left as-is")

	// No irreversible remote delete was armed either.
	var intents int
	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		return appDB.Qx(ctx).QueryRow(ctx,
			`SELECT count(*) FROM openrails.rail_intents WHERE subscription_id = ANY($1)`, cohort.subs).Scan(&intents)
	}))
	require.Zero(t, intents, "inherited evidence must never queue the irreversible NMI delete")

	subjects := guardFindings(t, appDB, baseCtx, FindingEvidenceStale)
	require.Len(t, subjects, len(cohort.subs), "every withheld cancel reaches the operator queue: unchecked is not disappeared")
	for _, id := range cohort.subs {
		require.Contains(t, subjects, evidenceStaleSubjectKey(ProviderNMI, id.String()))
	}
}

// The converse: the floor gates provenance, not the engine. The same armed
// merchant, the same pass, a decline this deployment WATCHED happen — converges
// normally, cancels, and raises no staleness finding.
func TestPull_ArmedMerchantStillConvergesOnEvidenceAfterTheFirstPull(t *testing.T) {
	appDB := startReconcilePostgres(t)
	baseCtx := merchant.WithID(context.Background(), dbtest.TestMerchantID)
	now := time.Now().UTC().Truncate(time.Second)

	armMerchantWithFirstPull(t, appDB, baseCtx, now.Add(-7*24*time.Hour))

	periodEnd := now.Add(-60 * 24 * time.Hour)
	cohort := seedGuardCohort(t, appDB, baseCtx, 3, "active", periodEnd)

	// Declined three days ago — four days after the first pull.
	snap := legacyDeclineSnapshot(now, periodEnd, cohort, now.Add(-3*24*time.Hour))
	runEnforcingPull(t, appDB, baseCtx, enforcingPullEngine(appDB, snap, now), cohort.psp)

	cancelled, live := guardCounts(t, appDB, baseCtx, cohort)
	require.Equal(t, len(cohort.subs), cancelled, "the floor must not freeze convergence on evidence we observed")
	require.Zero(t, live, "a certain cancel still revokes")
	require.Empty(t, guardFindings(t, appDB, baseCtx, FindingEvidenceStale))
}
