//go:build integration

package converge

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/reconcile"
	"github.com/open-rails/openrails/pkg/merchant"
)

// cannedFetcher serves one fixed snapshot (the FIXED evidence bundle).
type cannedFetcher struct{ snap *reconcile.RemoteSnapshot }

func (f *cannedFetcher) Name() string { return "nmi" }
func (f *cannedFetcher) Capabilities() reconcile.Capabilities {
	return reconcile.Capabilities{Subscriptions: true, Transactions: true}
}
func (f *cannedFetcher) Fetch(ctx context.Context, params reconcile.FetchParams) (*reconcile.RemoteSnapshot, error) {
	return f.snap, nil
}

// #665 acceptance, on real Postgres: for a FIXED evidence bundle, the order in
// which the planes run (LIFE sweep first vs unknown-resolution first) yields
// the SAME terminal state. Two fixtures — a provider-billed renewal and a
// provider-confirmed-dead sub — each seeded twice, driven through opposite
// plane orderings, then compared. Complements the pure property test
// (reconcile/decider_property_test.go), which sweeps all interleavings.
func TestDeciderPlaneInterleaving_SameTerminalState(t *testing.T) {
	appDB := startReconcilePostgres(t)
	merchantID := dbtest.TestMerchantID.UUID()
	baseCtx := merchant.WithID(context.Background(), dbtest.TestMerchantID)
	lc := subscriptions.NewSubscriptionLifecycleService(appDB, nil, nil, nil, nil, nil, clockwork.NewRealClock())
	now := time.Now().UTC().Truncate(time.Second)
	periodEnd := now.Add(-5 * 24 * time.Hour) // lapsed 5d (past PeriodGrace, within dunning window)
	start := now.Add(-35 * 24 * time.Hour)
	nextEnd := now.Add(25 * 24 * time.Hour)
	sfx := uuid.NewString()[:8]

	type fixture struct {
		sub     uuid.UUID
		cust    uuid.UUID
		railSub string
	}
	mk := func(ctx context.Context, key, railSub string, withEnt bool) fixture {
		f := fixture{sub: uuid.New(), railSub: railSub}
		f.cust = dbtest.EnsureCustomerIDPgx(ctx, t, appDB.Qx(ctx), uuid.NewString())
		prod, price := uuid.New(), uuid.New()
		exec := func(sql string, args ...any) {
			_, err := appDB.Qx(ctx).Exec(ctx, sql, args...)
			require.NoError(t, err)
		}
		exec(`INSERT INTO openrails.products (id,key,display_name,entitlements_spec,merchant_id) VALUES ($1,$2,$2,'{}'::jsonb,$3)`, prod, "il-"+key+"-"+sfx, merchantID)
		exec(`INSERT INTO openrails.prices (id,product_id,amount,currency,merchant_id) VALUES ($1,$2,5000000,'usd',$3)`, price, prod, merchantID)
		exec(`INSERT INTO openrails.subscriptions (id,merchant_id,customer_id,product_id,price_id,status,rail,rail_subscription_id,started_at,current_period_starts_at,current_period_ends_at)
		      VALUES ($1,$2,$3,$4,$5,'active','nmi',$6,$7,$7,$8)`, f.sub, merchantID, f.cust, prod, price, railSub, start, periodEnd)
		if withEnt {
			exec(`INSERT INTO openrails.entitlements (id, entitlement, start_at, end_at, source_id, source_type, customer_id, merchant_id)
			      VALUES ($1,'premium-il-'||$2, $3, $4, $5, 'subscription', $6, $7)`,
				uuid.New(), key+sfx, start, now.Add(10*24*time.Hour), f.sub, f.cust, merchantID)
		}
		return f
	}

	var renewA, renewB, goneA, goneB fixture
	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		renewA = mk(ctx, "ra", "il-renew-a-"+sfx, false)
		renewB = mk(ctx, "rb", "il-renew-b-"+sfx, false)
		goneA = mk(ctx, "ga", "il-gone-a-"+sfx, true)
		goneB = mk(ctx, "gb", "il-gone-b-"+sfx, true)
		return nil
	}))
	t.Cleanup(func() {
		_ = appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
			for _, f := range []fixture{renewA, renewB, goneA, goneB} {
				_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.entitlements WHERE source_id=$1`, f.sub)
				_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.payments WHERE subscription_id=$1`, f.sub)
				_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.reconciliation_findings WHERE merchant_id=$1 AND subject_key=$2`, merchantID, "subscription:"+f.sub.String())
				_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.subscriptions WHERE id=$1`, f.sub)
			}
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.prices WHERE product_id IN (SELECT id FROM openrails.products WHERE key LIKE 'il-%'||$1)`, "-"+sfx)
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.products WHERE key LIKE 'il-%'||$1`, "-"+sfx)
			return nil
		})
	})

	// The FIXED bundle: one fake NMI snapshot covering all four subs.
	// Renewal rows: roster-active with a future boundary + a verified renewal
	// charge. Gone rows: roster-declared cancelled (present in the roster, so
	// the fixture never relies on exhaustive-absence and cannot touch
	// unrelated rows sharing the test merchant).
	snap := &reconcile.RemoteSnapshot{
		Provider: reconcile.ProviderNMI,
		Subscriptions: []reconcile.RemoteSubscription{
			{RailSubscriptionID: renewA.railSub, Status: reconcile.SubscriptionStatusActive, NextBillingAt: &nextEnd},
			{RailSubscriptionID: renewB.railSub, Status: reconcile.SubscriptionStatusActive, NextBillingAt: &nextEnd},
			{RailSubscriptionID: goneA.railSub, Status: reconcile.SubscriptionStatusCancelled},
			{RailSubscriptionID: goneB.railSub, Status: reconcile.SubscriptionStatusCancelled},
		},
		Transactions: []reconcile.RemoteTransaction{
			{TransactionID: "il-tx-a-" + sfx, SubscriptionID: renewA.railSub, Type: reconcile.TransactionTypeSale, Success: true, AmountCents: 5000, Currency: "usd", OccurredAt: periodEnd.Add(time.Hour)},
			{TransactionID: "il-tx-b-" + sfx, SubscriptionID: renewB.railSub, Type: reconcile.TransactionTypeSale, Success: true, AmountCents: 5000, Currency: "usd", OccurredAt: periodEnd.Add(time.Hour)},
		},
	}
	fetchers := map[reconcile.Provider]reconcile.RailFetcher{reconcile.ProviderNMI: &cannedFetcher{snap: snap}}

	sweep := func(ctx context.Context, cust uuid.UUID) {
		e := NewConvergeEngine(appDB)
		_, err := e.Converge(ctx, Scope{Merchant: dbtest.TestMerchantID, Customer: &cust})
		require.NoError(t, err)
	}
	resolve := func(ctx context.Context) {
		_, err := reconcile.ReconcileUnknownCohort(ctx, appDB, lc, fetchers, nil, dbtest.TestMerchantID, time.Now().UTC(), reconcile.UnknownReconcileOptions{})
		require.NoError(t, err)
	}

	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		// Ordering A: LIFE sweep first (parks), then unknown-resolution, then
		// sweep again (fixpoint).
		for _, cust := range []uuid.UUID{renewA.cust, goneA.cust} {
			sweep(ctx, cust)
		}
		resolve(ctx)
		for _, cust := range []uuid.UUID{renewA.cust, goneA.cust} {
			sweep(ctx, cust)
		}

		// Ordering B: unknown-resolution first (no-op: rows still active), then
		// sweep (parks), then resolution, then sweep.
		resolve(ctx)
		for _, cust := range []uuid.UUID{renewB.cust, goneB.cust} {
			sweep(ctx, cust)
		}
		resolve(ctx)
		for _, cust := range []uuid.UUID{renewB.cust, goneB.cust} {
			sweep(ctx, cust)
		}
		return nil
	}))

	type terminal struct {
		status    string
		periodEnd *time.Time
		payments  int
		liveEnts  int
	}
	load := func(ctx context.Context, f fixture) terminal {
		var out terminal
		require.NoError(t, appDB.Qx(ctx).QueryRow(ctx,
			`SELECT status::text, current_period_ends_at FROM openrails.subscriptions WHERE id=$1`, f.sub).
			Scan(&out.status, &out.periodEnd))
		require.NoError(t, appDB.Qx(ctx).QueryRow(ctx,
			`SELECT count(*) FROM openrails.payments WHERE subscription_id=$1 AND status='completed'`, f.sub).Scan(&out.payments))
		require.NoError(t, appDB.Qx(ctx).QueryRow(ctx,
			`SELECT count(*) FROM openrails.entitlements WHERE source_id=$1 AND revoked_at IS NULL AND deleted_at IS NULL AND (end_at IS NULL OR end_at > now())`, f.sub).Scan(&out.liveEnts))
		return out
	}

	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		ra, rb := load(ctx, renewA), load(ctx, renewB)
		require.Equal(t, "active", ra.status, "renewal terminal is active")
		require.Equal(t, ra.status, rb.status, "ordering must not change the renewal terminal status")
		require.NotNil(t, ra.periodEnd)
		require.NotNil(t, rb.periodEnd)
		require.True(t, ra.periodEnd.Equal(*rb.periodEnd), "ordering must not change the adopted period end")
		require.True(t, ra.periodEnd.Equal(nextEnd), "period advanced to the provider's next billing")
		require.Equal(t, 1, ra.payments, "renewal charge backfilled exactly once")
		require.Equal(t, ra.payments, rb.payments)

		ga, gb := load(ctx, goneA), load(ctx, goneB)
		require.Equal(t, "cancelled", ga.status, "provider-confirmed dead terminal is cancelled")
		require.Equal(t, ga.status, gb.status, "ordering must not change the cancel terminal status")
		require.Zero(t, ga.liveEnts, "entitlements revoked with the certain cancel")
		require.Equal(t, ga.liveEnts, gb.liveEnts)
		return nil
	}))
}
