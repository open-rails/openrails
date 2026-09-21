//go:build integration

package reconcile

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/pkg/merchant"
)

// Only the provider is simulated. State loading, finding persistence, mutations,
// lifecycle decisions and destructive-run receipts all use production PostgreSQL code.
func newPGReconcileEngine(appDB *db.DB, snap *RemoteSnapshot) *Engine {
	return &Engine{
		Fetchers: map[Provider]RailFetcher{ProviderNMI: &fakeFetcher{provider: ProviderNMI, snap: snap}},
		Store:    &PGStore{DB: appDB}, Local: &PGLocalStateLoader{DB: appDB},
		Writer: &PGLocalWriter{DB: appDB}, Decisions: NewDecisionApplier(appDB, nil),
		Runs: &PGDestructiveRunRecorder{DB: appDB},
	}
}

// Observe the rows the old fake writer simulated, including timestamps and ids.
// Reconciliation run/finding rows may change even when the billing book must not.
func reconcileBillingState(t *testing.T, appDB *db.DB, ctx context.Context) string {
	t.Helper()
	var state string
	require.NoError(t, appDB.RunInMerchantConn(ctx, func(ctx context.Context) error {
		return appDB.Qx(ctx).QueryRow(ctx, `SELECT jsonb_build_object(
		  'subscriptions', (SELECT coalesce(jsonb_agg(to_jsonb(s) ORDER BY id), '[]') FROM billing.subscriptions s WHERE merchant_id=billing.current_merchant_id()),
		  'payments', (SELECT coalesce(jsonb_agg(to_jsonb(p) ORDER BY id), '[]') FROM billing.payments p WHERE merchant_id=billing.current_merchant_id()),
		  'payment_methods', (SELECT coalesce(jsonb_agg(to_jsonb(p) ORDER BY id), '[]') FROM billing.payment_methods p WHERE merchant_id=billing.current_merchant_id()),
		  'entitlements', (SELECT coalesce(jsonb_agg(to_jsonb(e) ORDER BY id), '[]') FROM billing.entitlements e WHERE merchant_id=billing.current_merchant_id())
		)::text`).Scan(&state)
	}))
	return state
}

func TestReconcilePullProofsAndPSPBinding(t *testing.T) {
	appDB := startReconcilePostgres(t)
	mid := newReconcileMerchant(t, appDB)
	ctx := merchant.WithID(context.Background(), mid)
	nmi := seedTestPSPBindingFor(t, appDB, ctx, mid.UUID(), "nmi")
	stripe := seedTestPSPBindingFor(t, appDB, ctx, mid.UUID(), "stripe")
	var snap *RemoteSnapshot
	require.NoError(t, appDB.RunInMerchantConn(ctx, func(ctx context.Context) error {
		seeded := seedReconcileFixtures(t, ctx, appDB, mid.UUID(), nmi.ID)
		snap = reconcileSnapshot(t, ctx, appDB, seeded)
		return nil
	}))
	before := reconcileBillingState(t, appDB, ctx)
	engine := newPGReconcileEngine(appDB, snap)
	engine.Fetchers[ProviderStripe] = &fakeFetcher{provider: ProviderStripe, err: errors.New("stripe down")}
	require.NoError(t, appDB.RunInMerchantConn(ctx, func(ctx context.Context) error {
		result, err := engine.Run(ctx, RunParams{Mode: ModeAdvisory, Providers: []Provider{ProviderNMI, ProviderStripe}, PSPs: map[Provider]PSPBinding{ProviderNMI: nmi, ProviderStripe: stripe}})
		require.ErrorContains(t, err, "stripe down")
		require.NotNil(t, result)
		proofs := result.PullProofs()
		require.Len(t, proofs, 1)
		require.True(t, proofs[ProviderNMI].Coverage.SubscriptionsExhaustive)
		require.NotContains(t, proofs, ProviderStripe)
		run, err := (&PGStore{DB: appDB}).GetRun(ctx, result.RunID)
		require.NoError(t, err)
		require.Equal(t, "failed", run.Status)
		return nil
	}))
	require.Equal(t, before, reconcileBillingState(t, appDB, ctx))

	for _, bindings := range []map[Provider]PSPBinding{nil, {ProviderNMI: {Rail: "nmi", AccountID: "missing-id"}}} {
		require.NoError(t, appDB.RunInMerchantConn(ctx, func(ctx context.Context) error {
			_, err := engine.Run(ctx, RunParams{Mode: ModeEnforce, Providers: []Provider{ProviderNMI}, PSPs: bindings})
			require.ErrorContains(t, err, "no PSP binding for provider nmi")
			return nil
		}))
		require.Equal(t, before, reconcileBillingState(t, appDB, ctx), "an unbound pull must not mutate any billing row")
	}
}

func TestReconcileImplausibleRosterPreservesBilling(t *testing.T) {
	for _, tc := range []struct{ local, remote int }{{1, 0}, {5, 0}, {9, 0}, {20, 1}} {
		t.Run(fmt.Sprintf("%d_of_%d", tc.remote, tc.local), func(t *testing.T) {
			appDB := startReconcilePostgres(t)
			ctx := merchant.WithID(context.Background(), dbtest.TestMerchantID)
			cohort := seedGuardCohort(t, appDB, ctx, tc.local, "active", time.Now().Add(30*24*time.Hour))
			snap := &RemoteSnapshot{Provider: ProviderNMI, Capabilities: Capabilities{Subscriptions: true}, Coverage: SnapshotCoverage{SubscriptionsExhaustive: true}}
			for i := 0; i < tc.remote; i++ {
				snap.Subscriptions = append(snap.Subscriptions, RemoteSubscription{RailSubscriptionID: cohort.railSubs[i], Status: SubscriptionStatusActive})
			}
			before := reconcileBillingState(t, appDB, ctx)
			require.NoError(t, appDB.RunInMerchantConn(ctx, func(ctx context.Context) error {
				result, err := newPGReconcileEngine(appDB, snap).Run(ctx, RunParams{Mode: ModeEnforce, Providers: []Provider{ProviderNMI}, PSPs: map[Provider]PSPBinding{ProviderNMI: cohort.psp}})
				require.ErrorContains(t, err, "circuit breaker")
				require.NotNil(t, result)
				require.Equal(t, "failed", result.Status)
				require.True(t, result.Summary.Providers["nmi"].Aborted)
				require.Empty(t, result.Findings)
				var persisted int
				require.NoError(t, appDB.Qx(ctx).QueryRow(ctx, `SELECT count(*) FROM billing.reconciliation_findings WHERE last_seen_run=$1`, result.RunID).Scan(&persisted))
				require.Zero(t, persisted, "a refused roster must not persist absence findings")
				return nil
			}))
			require.Equal(t, before, reconcileBillingState(t, appDB, ctx))
			cancelled, live := guardCounts(t, appDB, ctx, cohort)
			require.Zero(t, cancelled)
			require.Equal(t, tc.local, live)
		})
	}
}
