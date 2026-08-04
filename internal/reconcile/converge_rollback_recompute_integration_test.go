//go:build integration

package reconcile_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/reconcile"
	"github.com/open-rails/openrails/internal/reconcile/converge"
	"github.com/open-rails/openrails/pkg/merchant"
)

// or#859's load-bearing doctrinal claim, proven on the real path: after a
// reversal, ACCESS comes back by RECOMPUTATION, not by restoration.
//
// The reverse never replays an entitlement before-image. It puts the
// subscription back, leaves the append-only grant log alone, and Converge
// re-derives the effect from that log. A restored effect can silently disagree
// with its grant; a re-derived one cannot. This test lives in the external test
// package because `converge` imports `reconcile`.

// rollbackFakeFetcher serves one canned snapshot.
type rollbackFakeFetcher struct{ snap *reconcile.RemoteSnapshot }

func (f *rollbackFakeFetcher) Name() string                         { return string(reconcile.ProviderNMI) }
func (f *rollbackFakeFetcher) Capabilities() reconcile.Capabilities { return f.snap.Capabilities }
func (f *rollbackFakeFetcher) Fetch(context.Context, reconcile.FetchParams) (*reconcile.RemoteSnapshot, error) {
	return f.snap, nil
}

func TestConvergeEnforceRollback_AccessReturnsByRecomputationNotRestoration(t *testing.T) {
	_, appDSN := dbtest.SharedRLSPostgres(t)
	pool, err := pgxpool.New(context.Background(), appDSN)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	appDB, err := db.NewWithPGXPool(pool, "")
	require.NoError(t, err)

	baseCtx := merchant.WithID(context.Background(), dbtest.TestMerchantID)
	merchantID := dbtest.TestMerchantID.UUID()
	now := time.Now().UTC().Truncate(time.Second)
	suffix := uuid.NewString()[:8]
	pspID := uuid.New()

	// Two account-bound NMI subscriptions, both paid up: their period and their
	// access window both run 30 days into the future.
	subs := make([]uuid.UUID, 2)
	railSubs := make([]string, 2)
	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		exec := func(sql string, args ...any) {
			_, e := appDB.Qx(ctx).Exec(ctx, sql, args...)
			require.NoError(t, e)
		}
		exec(`INSERT INTO openrails.psps (id, merchant_id, rail, account_id, archived) VALUES ($1,$2,'nmi',$3,false)`,
			pspID, merchantID, "acct-rc-"+suffix)
		for i := range subs {
			cust := dbtest.EnsureCustomerIDPgx(ctx, t, appDB.Qx(ctx), uuid.NewString())
			prod, price := uuid.New(), uuid.New()
			subs[i], railSubs[i] = uuid.New(), fmt.Sprintf("rs-rc-%s-%d", suffix, i)
			exec(`INSERT INTO openrails.products (id,key,display_name,entitlements_spec,merchant_id)
			      VALUES ($1,$2,$2,jsonb_build_object('premium',null),$3)`, prod, fmt.Sprintf("rc-%s-%d", suffix, i), merchantID)
			exec(`INSERT INTO openrails.prices (id,product_id,amount,currency,access_duration_hours,auto_renew,merchant_id)
			      VALUES ($1,$2,5000000,'USD',720,true,$3)`, price, prod, merchantID)
			exec(`INSERT INTO openrails.subscriptions
			        (id,merchant_id,customer_id,product_id,price_id,status,rail,rail_subscription_id,psp_id,
			         started_at,current_period_starts_at,current_period_ends_at,entitlements_spec_snapshot)
			      VALUES ($1,$2,$3,$4,$5,'active','nmi',$6,$7,$8,$8,$9,jsonb_build_object('premium',null))`,
				subs[i], merchantID, cust, prod, price, railSubs[i], pspID, now.Add(-30*24*time.Hour), now.Add(30*24*time.Hour))
			exec(`INSERT INTO openrails.entitlements (merchant_id,customer_id,entitlement,start_at,end_at,source_id,source_type)
			      VALUES ($1,$2,'premium',$3,$4,$5,'subscription')`,
				merchantID, cust, now.Add(-30*24*time.Hour), now.Add(30*24*time.Hour), subs[i])
		}
		return nil
	}))
	t.Cleanup(func() {
		_ = appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
			for _, s := range subs {
				for _, sql := range []string{
					`DELETE FROM openrails.rail_intents WHERE subscription_id=$1`,
					`DELETE FROM openrails.entitlements WHERE source_id=$1`,
					`DELETE FROM openrails.grants WHERE source_id=$1::text`,
					`DELETE FROM openrails.subscription_status_transitions WHERE subscription_id=$1`,
					`DELETE FROM openrails.destructive_run_before_images WHERE row_id=$1`,
					`DELETE FROM openrails.subscriptions WHERE id=$1`,
				} {
					_, _ = appDB.Qx(ctx).Exec(ctx, sql, s)
				}
			}
			_, _ = appDB.Qx(ctx).Exec(ctx,
				`DELETE FROM openrails.destructive_run_before_images WHERE destructive_run_id IN (SELECT id FROM openrails.destructive_runs WHERE psp_id=$1)`, pspID)
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.destructive_runs WHERE psp_id=$1`, pspID)
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.prices WHERE product_id IN (SELECT id FROM openrails.products WHERE key LIKE 'rc-'||$1||'-%')`, suffix)
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.products WHERE key LIKE 'rc-'||$1||'-%'`, suffix)
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.psps WHERE id=$1`, pspID)
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.merchant_destructive_policy WHERE merchant_id=$1`, merchantID)
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.reconciliation_state WHERE merchant_id=$1`, merchantID)
			return nil
		})
	})

	liveAccess := func(sub uuid.UUID) int {
		var n int
		require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
			return appDB.Qx(ctx).QueryRow(ctx,
				`SELECT count(*) FROM openrails.entitlements
				  WHERE source_id=$1 AND revoked_at IS NULL AND deleted_at IS NULL
				    AND start_at <= now() AND (end_at IS NULL OR end_at > now())`, sub).Scan(&n)
		}))
		return n
	}
	grantCount := func() int {
		var n int
		require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
			return appDB.Qx(ctx).QueryRow(ctx, `SELECT count(*) FROM openrails.grants WHERE merchant_id=$1`, merchantID).Scan(&n)
		}))
		return n
	}

	require.Equal(t, 1, liveAccess(subs[0]), "the victim starts with live access")
	grantsBefore := grantCount()

	// The bad roster: subs[1] listed, subs[0] silently absent from a roster
	// claiming to be exhaustive.
	next := now.Add(30 * 24 * time.Hour)
	snap := &reconcile.RemoteSnapshot{
		Provider:     reconcile.ProviderNMI,
		FetchedAt:    now,
		Capabilities: reconcile.Capabilities{Subscriptions: true},
		Coverage:     reconcile.SnapshotCoverage{SubscriptionsExhaustive: true},
		Subscriptions: []reconcile.RemoteSubscription{{
			RailSubscriptionID: railSubs[1], Status: reconcile.SubscriptionStatusActive, NextBillingAt: &next,
		}},
	}
	binding := reconcile.RailMerchantAccountBinding{ID: pspID, Rail: "nmi", AccountID: "acct-rc-" + suffix}
	eng := &reconcile.Engine{
		Fetchers:  map[reconcile.Provider]reconcile.RailFetcher{reconcile.ProviderNMI: &rollbackFakeFetcher{snap: snap}},
		Store:     &reconcile.PGStore{DB: appDB},
		Local:     &reconcile.PGLocalStateLoader{DB: appDB},
		Writer:    &reconcile.PGLocalWriter{DB: appDB},
		Decisions: reconcile.NewDecisionApplier(appDB, intents.NewNMIDeleteScheduler(appDB, nil, intents.OriginSystem, "or859 recompute test")),
		Runs:      &reconcile.PGDestructiveRunRecorder{DB: appDB},
		Now:       func() time.Time { return now },
		Actor:     "or859-recompute-test",
	}

	var runID uuid.UUID
	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		res, e := eng.Run(ctx, reconcile.RunParams{
			Mode:        reconcile.ModeEnforce,
			Mutations:   &reconcile.LocalMutationPolicy{Overwrite: true},
			Providers:   []reconcile.Provider{reconcile.ProviderNMI},
			PSPs:        map[reconcile.Provider]reconcile.RailMerchantAccountBinding{reconcile.ProviderNMI: binding},
			PSPCoverage: map[reconcile.Provider]reconcile.PSPCoverage{reconcile.ProviderNMI: {Declared: 1, Pulled: 1, Binding: binding}},
		})
		if e != nil {
			return e
		}
		runID = uuid.MustParse(res.Summary.Providers[string(reconcile.ProviderNMI)].DestructiveRunID)
		return nil
	}))

	require.Zero(t, liveAccess(subs[0]), "the bad pass should have taken the customer's access away")

	// The reverse, wired exactly as `openrails converge rollback` wires it: the
	// re-derivation is a callback because `converge` imports `reconcile`, and it
	// runs strictly AFTER the reversal's transaction commits.
	recompute := func(ctx context.Context) error {
		_, cerr := converge.NewConvergeEngine(appDB).Converge(ctx, converge.Scope{Merchant: dbtest.TestMerchantID})
		return cerr
	}
	var res reconcile.ConvergeRollbackResult
	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		var e error
		res, e = reconcile.RollbackConvergeEnforceRun(ctx, appDB, runID, "operator", recompute)
		return e
	}))
	require.True(t, res.Complete())
	require.True(t, res.Recomputed)
	require.Positive(t, res.EntitlementsInvalidated, "the windows the run closed must be invalidated, not left in place")

	var status string
	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		return appDB.Qx(ctx).QueryRow(ctx, `SELECT status::text FROM openrails.subscriptions WHERE id=$1`, subs[0]).Scan(&status)
	}))
	require.Equal(t, "active", status, "the subscription is restored from its before-image")

	// Not one entitlement image was replayed — access came back another way.
	var replayed int
	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		return appDB.Qx(ctx).QueryRow(ctx,
			`SELECT count(*) FROM openrails.destructive_run_before_images
			  WHERE destructive_run_id=$1 AND table_name='entitlements' AND restored_at IS NOT NULL`, runID).Scan(&replayed)
	}))
	require.Zero(t, replayed, "an entitlement before-image must never be replayed")

	require.Equal(t, 1, liveAccess(subs[0]),
		"access must come back through Converge re-deriving the grant effect, not through the rollback replaying an image")

	// The grant log only ever grows: the rollback never deleted from it, and the
	// re-derivation appended the grant that now justifies the rebuilt window.
	require.Greater(t, grantCount(), grantsBefore)

	var derivedFromGrant int
	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		return appDB.Qx(ctx).QueryRow(ctx,
			`SELECT count(*) FROM openrails.entitlements
			  WHERE source_id=$1 AND deleted_at IS NULL AND revoked_at IS NULL AND grant_id IS NOT NULL`, subs[0]).Scan(&derivedFromGrant)
	}))
	require.Equal(t, 1, derivedFromGrant,
		"the live window must be the one derived from a grant — that is what makes it impossible for it to disagree with the grant")
}
