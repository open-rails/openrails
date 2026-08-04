//go:build integration

package reconcile

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/pkg/merchant"
)

// or#858. Three properties a prune must have, each of which the pre-or#858 code
// did not:
//
//  1. An EMPTY provider roster refuses. It used to mean "everything is excess"
//     (`COALESCE(cardinality(present_ids),0) = 0 OR ...`), so one
//     successful-but-empty pull with --prune --apply hard-deleted a PSP's whole
//     local book.
//  2. A prune SOFT-deletes: the rows vanish from every live read path but the
//     data is still there.
//  3. The whole run reverses by id, restoring the subscription, its checkout
//     sessions and its entitlements together.
type pruneFixture struct {
	pspID    uuid.UUID
	subID    uuid.UUID
	payID    uuid.UUID
	sessID   uuid.UUID
	entID    uuid.UUID
	customer uuid.UUID
	railSub  string
	railTxn  string
	binding  RailMerchantAccountBinding
}

func seedPruneFixture(t *testing.T, appDB *db.DB, baseCtx context.Context) pruneFixture {
	t.Helper()
	merchantID := dbtest.TestMerchantID.UUID()
	suffix := uuid.NewString()[:8]
	f := pruneFixture{
		pspID:   uuid.New(),
		subID:   uuid.New(),
		payID:   uuid.New(),
		sessID:  uuid.New(),
		entID:   uuid.New(),
		railSub: "psub-sd-" + suffix,
		railTxn: "txn-sd-" + suffix,
	}
	productID, priceID := uuid.New(), uuid.New()

	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		f.customer = dbtest.EnsureCustomerIDPgx(ctx, t, appDB.Qx(ctx), uuid.NewString())
		exec := func(sql string, args ...any) {
			_, err := appDB.Qx(ctx).Exec(ctx, sql, args...)
			require.NoError(t, err)
		}
		exec(`INSERT INTO openrails.psps (id, merchant_id, rail, account_id, archived) VALUES ($1,$2,'nmi',$3,false)`,
			f.pspID, merchantID, "acct-sd-"+suffix)
		exec(`INSERT INTO openrails.products (id, key, display_name, tier_group, entitlements_spec, merchant_id) VALUES ($1,$2,$2,$3,'{"pro": null}'::jsonb,$4)`,
			productID, "sd-"+suffix, "sd-tier-"+suffix, merchantID)
		exec(`INSERT INTO openrails.prices (id, product_id, amount, currency, access_duration_hours, auto_renew, merchant_id) VALUES ($1,$2,999,'USD',720,true,$3)`,
			priceID, productID, merchantID)
		exec(`INSERT INTO openrails.subscriptions (id, price_id, product_id, status, rail, rail_subscription_id, psp_id, current_period_starts_at, current_period_ends_at, started_at, entitlements_spec_snapshot, customer_id, merchant_id)
		      VALUES ($1,$2,$3,'active','nmi',$4,$5,$6,$7,$6,'{}'::jsonb,$8,$9)`,
			f.subID, priceID, productID, f.railSub, f.pspID, time.Now().Add(-20*24*time.Hour), time.Now().Add(10*24*time.Hour), f.customer, merchantID)
		exec(`INSERT INTO openrails.payments (id, price_id, rail, transaction_id, amount, list_amount, currency, status, purchased_at, customer_id, merchant_id, psp_id, subscription_id)
		      VALUES ($1,$2,'nmi',$3,999,999,'USD','completed',$4,$5,$6,$7,$8)`,
			f.payID, priceID, f.railTxn, time.Now().Add(-20*24*time.Hour), f.customer, merchantID, f.pspID, f.subID)
		exec(`INSERT INTO openrails.checkout_sessions (id, merchant_id, customer_id, price_id, mode, rail, status, amount, currency, subscription_id, psp_id)
		      VALUES ($1,$2,$3,$4,'subscription','nmi','completed',999,'USD',$5,$6)`,
			f.sessID, merchantID, f.customer, priceID, f.subID, f.pspID)
		exec(`INSERT INTO openrails.entitlements (id, merchant_id, customer_id, entitlement, source_type, source_id, start_at, end_at)
		      VALUES ($1,$2,$3,'pro','subscription',$4,$5,$6)`,
			f.entID, merchantID, f.customer, f.subID, time.Now().Add(-20*24*time.Hour), time.Now().Add(10*24*time.Hour))
		return nil
	}))
	t.Cleanup(func() {
		_ = appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
			for _, sql := range []string{
				`DELETE FROM openrails.entitlements WHERE id=$1`,
				`DELETE FROM openrails.checkout_sessions WHERE id=$2`,
				`DELETE FROM openrails.payments WHERE id=$3`,
				`DELETE FROM openrails.subscriptions WHERE id=$4`,
			} {
				_, _ = appDB.Qx(ctx).Exec(ctx, sql, f.entID, f.sessID, f.payID, f.subID)
			}
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.destructive_runs WHERE psp_id=$1`, f.pspID)
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.prices WHERE id=$1`, priceID)
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.products WHERE id=$1`, productID)
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.psps WHERE id=$1`, f.pspID)
			return nil
		})
	})
	f.binding = RailMerchantAccountBinding{ID: f.pspID, Rail: "nmi", AccountID: "acct-sd-" + suffix}
	return f
}

// TestPruneRefusesEmptyRemoteSet is the or#858 hazard, directly: a snapshot that
// CLAIMS complete coverage and lists nothing. or#842 closed this for NMI only
// (its fetcher stops stamping SubscriptionsExhaustive on an empty roster) —
// every other fetcher, Stripe included, still stamps it unconditionally, so the
// refusal has to live here where it covers all of them.
func TestPruneRefusesEmptyRemoteSet(t *testing.T) {
	appDB := startReconcilePostgres(t)
	baseCtx := merchant.WithID(context.Background(), dbtest.TestMerchantID)
	f := seedPruneFixture(t, appDB, baseCtx)

	since := time.Now().Add(-90 * 24 * time.Hour).UTC()
	until := time.Now().Add(24 * time.Hour).UTC()
	empty := &RemoteSnapshot{
		Provider:     ProviderNMI,
		FetchedAt:    time.Now().UTC(),
		Capabilities: Capabilities{Subscriptions: true, Transactions: true},
		Coverage: SnapshotCoverage{
			SubscriptionsExhaustive:       true,
			TransactionsExhaustive:        true,
			TransactionsPaginatedComplete: true,
			TransactionWindowSince:        &since,
			TransactionWindowUntil:        &until,
		},
	}
	fetcher := &fakeFetcher{provider: ProviderNMI, snap: empty}

	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		// Refuses even as a dry-run: a plan that says "prune the whole book" is
		// itself the dangerous artifact.
		_, err := PruneRailMerchantAccountExcess(ctx, appDB, fetcher, ProviderNMI, f.binding, PruneParams{Since: since, Until: until})
		require.Error(t, err)
		require.IsType(t, &ErrPruneEmptyRemoteSet{}, err)
		require.ErrorContains(t, err, "listed ZERO subscriptions")

		_, err = PruneRailMerchantAccountExcess(ctx, appDB, fetcher, ProviderNMI, f.binding,
			PruneParams{Since: since, Until: until, Apply: true, ExpectedRows: intp(1)})
		require.IsType(t, &ErrPruneEmptyRemoteSet{}, err)

		require.True(t, subLive(ctx, t, appDB, f.subID), "the book survives an empty roster")
		require.True(t, payLive(ctx, t, appDB, f.payID))
		return nil
	}))

	// Defence in depth: even if a caller skipped the refusal, the SQL itself
	// matches NOTHING on an empty set — it used to match everything.
	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		rows, err := appDB.Gen(ctx).ListExcessSubscriptionsForPSP(ctx, gen.ListExcessSubscriptionsForPSPParams{
			MerchantID: dbtest.TestMerchantID.UUID(), PspID: f.pspID, PresentIds: nil,
		})
		require.NoError(t, err)
		require.Empty(t, rows, "an empty present set must match no rows at all")
		return nil
	}))
}

// TestPruneRefusesWrongExpectedRowCount: --apply is not a bare bool. An operator
// who believes the pass will remove 5 rows and is wrong is stopped, not obeyed.
func TestPruneRefusesWrongExpectedRowCount(t *testing.T) {
	appDB := startReconcilePostgres(t)
	baseCtx := merchant.WithID(context.Background(), dbtest.TestMerchantID)
	f := seedPruneFixture(t, appDB, baseCtx)

	snap := &RemoteSnapshot{
		Provider:      ProviderNMI,
		FetchedAt:     time.Now().UTC(),
		Capabilities:  Capabilities{Subscriptions: true},
		Coverage:      SnapshotCoverage{SubscriptionsExhaustive: true},
		Subscriptions: []RemoteSubscription{{RailSubscriptionID: "psub-someone-else", Status: SubscriptionStatusActive}},
	}
	fetcher := &fakeFetcher{provider: ProviderNMI, snap: snap}

	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		_, err := PruneRailMerchantAccountExcess(ctx, appDB, fetcher, ProviderNMI, f.binding,
			PruneParams{Apply: true, ExpectedRows: intp(5)})
		require.IsType(t, &ErrPruneCountMismatch{}, err)
		require.ErrorContains(t, err, "says 5, this pass found 1")
		require.True(t, subLive(ctx, t, appDB, f.subID), "a miscounted confirmation writes nothing")

		var runs int
		require.NoError(t, appDB.Qx(ctx).QueryRow(ctx, `SELECT count(*) FROM openrails.destructive_runs WHERE psp_id=$1`, f.pspID).Scan(&runs))
		require.Zero(t, runs, "a refused prune opens no run")
		return nil
	}))
}

// TestPruneSoftDeletesAndRollbackRestores: the pruned subscription disappears
// from the live read paths (direct get, customer list, provider-id lookup, the
// standing-access projection) together with its checkout session and
// entitlement — and one rollback brings all of them back.
func TestPruneSoftDeletesAndRollbackRestores(t *testing.T) {
	appDB := startReconcilePostgres(t)
	merchantID := dbtest.TestMerchantID.UUID()
	baseCtx := merchant.WithID(context.Background(), dbtest.TestMerchantID)
	f := seedPruneFixture(t, appDB, baseCtx)

	snap := &RemoteSnapshot{
		Provider:      ProviderNMI,
		FetchedAt:     time.Now().UTC(),
		Capabilities:  Capabilities{Subscriptions: true},
		Coverage:      SnapshotCoverage{SubscriptionsExhaustive: true},
		Subscriptions: []RemoteSubscription{{RailSubscriptionID: "psub-someone-else", Status: SubscriptionStatusActive}},
	}
	fetcher := &fakeFetcher{provider: ProviderNMI, snap: snap}

	var runID uuid.UUID
	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		res, err := PruneRailMerchantAccountExcess(ctx, appDB, fetcher, ProviderNMI, f.binding,
			PruneParams{Apply: true, ExpectedRows: intp(1), Actor: "tester"})
		require.NoError(t, err)
		runID = res.RunID
		require.Equal(t, 1, res.Subscriptions)
		require.Equal(t, 1, res.CheckoutSessions)
		require.Equal(t, 1, res.Entitlements)
		return nil
	}))

	// Gone from every live read path...
	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		q := appDB.Gen(ctx)
		_, err := q.GetSubscriptionByID(ctx, f.subID)
		require.Error(t, err, "GetSubscriptionByID must not see a pruned row")

		_, err = q.GetSubscriptionByRailSubID(ctx, gen.GetSubscriptionByRailSubIDParams{Rail: "nmi", RailSubscriptionID: f.railSub})
		require.Error(t, err, "provider-id lookup must not see a pruned row")

		live, err := q.ListActiveSubscriptionsByCustomer(ctx, f.customer)
		require.NoError(t, err)
		require.Empty(t, live, "the customer's active list must not include a pruned sub")

		standing, err := q.SubscriptionProjectsStandingAccess(ctx, gen.SubscriptionProjectsStandingAccessParams{MerchantID: merchantID, ID: f.subID})
		require.NoError(t, err)
		require.False(t, standing, "a pruned subscription projects no standing access")

		ents, err := q.ListEntitlementsByCustomer(ctx, f.customer)
		require.NoError(t, err)
		require.Empty(t, ents, "the pruned subscription's entitlement is gone with it")

		_, err = q.GetCheckoutSessionByID(ctx, f.sessID)
		require.Error(t, err, "the subscription's checkout session went with it")
		return nil
	}))

	// ...but nothing was destroyed.
	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		var n int
		require.NoError(t, appDB.Qx(ctx).QueryRow(ctx,
			`SELECT count(*) FROM openrails.subscriptions WHERE id=$1 AND deleted_at IS NOT NULL AND destructive_run_id=$2`, f.subID, runID).Scan(&n))
		require.Equal(t, 1, n, "the row is still there, stamped with its run")

		run, err := appDB.Gen(ctx).GetDestructiveRun(ctx, gen.GetDestructiveRunParams{MerchantID: merchantID, ID: runID})
		require.NoError(t, err)
		require.Equal(t, "prune", run.Kind)
		require.Equal(t, "completed", run.Status)
		require.Equal(t, "tester", run.Actor)
		require.NotNil(t, run.Coverage, "the run records the absence proof it relied on")
		require.Contains(t, string(run.Coverage), "subscriptions_exhaustive")
		return nil
	}))

	// One command, everything back.
	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		rb, err := RollbackDestructiveRun(ctx, appDB, runID, "tester")
		require.NoError(t, err)
		require.EqualValues(t, 1, rb.Subscriptions)
		require.EqualValues(t, 1, rb.CheckoutSessions)
		require.EqualValues(t, 1, rb.Entitlements)

		q := appDB.Gen(ctx)
		sub, err := q.GetSubscriptionByID(ctx, f.subID)
		require.NoError(t, err)
		require.Nil(t, sub.DeletedAt)
		require.Nil(t, sub.DestructiveRunID, "the stamp is cleared, so a second rollback cannot double-restore")

		_, err = q.GetCheckoutSessionByID(ctx, f.sessID)
		require.NoError(t, err)
		ents, err := q.ListEntitlementsByCustomer(ctx, f.customer)
		require.NoError(t, err)
		require.Len(t, ents, 1, "access comes back with the subscription")

		run, err := q.GetDestructiveRun(ctx, gen.GetDestructiveRunParams{MerchantID: merchantID, ID: runID})
		require.NoError(t, err)
		require.Equal(t, "reversed", run.Status)
		return nil
	}))
}

func subLive(ctx context.Context, t *testing.T, appDB *db.DB, id uuid.UUID) bool {
	t.Helper()
	var n int
	require.NoError(t, appDB.Qx(ctx).QueryRow(ctx, `SELECT count(*) FROM openrails.subscriptions WHERE id=$1 AND deleted_at IS NULL`, id).Scan(&n))
	return n == 1
}

func payLive(ctx context.Context, t *testing.T, appDB *db.DB, id uuid.UUID) bool {
	t.Helper()
	var n int
	require.NoError(t, appDB.Qx(ctx).QueryRow(ctx, `SELECT count(*) FROM openrails.payments WHERE id=$1 AND deleted_at IS NULL`, id).Scan(&n))
	return n == 1
}
