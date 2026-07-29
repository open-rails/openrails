//go:build integration

package grants_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/modules/grants"
	"github.com/open-rails/openrails/internal/modules/money/ledger"
)

func TestMain(m *testing.M) { dbtest.RunMain(m) }

func short() string { return strings.ReplaceAll(uuid.NewString(), "-", "")[:10] }

func testGrants(t *testing.T) (*grants.Ledger, *pgxpool.Pool, context.Context, uuid.UUID, uuid.UUID, uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	dbi := dbtest.OpenAppDB(t, dbtest.MerchantPinnedDSN(t, dbtest.TestMerchantID.UUID()))
	pool := dbi.Pool()
	dbtest.EnsureTestMerchant(ctx, t, pool)
	merchantID := dbtest.TestMerchantID.UUID()
	customer := uuid.New()
	product := uuid.New()
	_, err := pool.Exec(ctx, `INSERT INTO openrails.customers (id, merchant_id) VALUES ($1, $2) ON CONFLICT DO NOTHING`, customer, merchantID)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `INSERT INTO openrails.products (id, key, display_name, merchant_id) VALUES ($1, $2, $3, $4) ON CONFLICT DO NOTHING`,
		product, "grant-test-"+short(), "Grant Test Product", merchantID)
	require.NoError(t, err)

	t.Cleanup(func() {
		// child (terminating) events before parent grant rows (self-FK).
		_, _ = pool.Exec(ctx, `DELETE FROM openrails.grants WHERE merchant_id = $1 AND customer_id = $2 AND event <> 'grant'`, merchantID, customer)
		_, _ = pool.Exec(ctx, `DELETE FROM openrails.grants WHERE merchant_id = $1 AND customer_id = $2`, merchantID, customer)
		_, _ = pool.Exec(ctx, `DELETE FROM openrails.entitlements WHERE merchant_id = $1 AND customer_id = $2`, merchantID, customer)
		_, _ = pool.Exec(ctx, `DELETE FROM openrails.ledger_transfers WHERE merchant_id = $1 AND customer_id = $2`, merchantID, customer)
	})
	return grants.New(gen.New(pool), merchantID), pool, ctx, customer, product, merchantID
}

// A grant materializes to an entitlement window dated to the grant; re-deriving
// is idempotent; a past-dated grant replays as an already-expired window.
func TestGrants_EntitlementProjection(t *testing.T) {
	l, pool, ctx, customer, product, merchantID := testGrants(t)

	g, err := l.Grant(ctx, grants.GrantInput{
		Customer: customer, Product: &product, Kind: grants.Entitlement,
		Source: grants.Purchase, SourceID: "pay_" + short(),
		Spec: &grants.Spec{Entitlements: []string{"premium"}},
	})
	require.NoError(t, err)
	require.NoError(t, l.MaterializeGrant(ctx, g))

	require.Equal(t, 1, entWindows(t, ctx, pool, merchantID, g.ID, "premium", false))
	// Idempotent — re-derive does not duplicate.
	require.NoError(t, l.MaterializeGrant(ctx, g))
	require.Equal(t, 1, entWindows(t, ctx, pool, merchantID, g.ID, "premium", false))

	// Historical replay: a 2025-06-30 90-day grant rebuilds as an already-expired window.
	start := time.Date(2025, 6, 30, 0, 0, 0, 0, time.UTC)
	end := start.Add(90 * 24 * time.Hour)
	hg, err := l.Grant(ctx, grants.GrantInput{
		Customer: customer, Product: &product, Kind: grants.Entitlement,
		Source: grants.Purchase, SourceID: "pay_" + short(),
		Spec: &grants.Spec{Entitlements: []string{"legacy90"}}, StartsAt: start, EndsAt: &end,
	})
	require.NoError(t, err)
	require.NoError(t, l.MaterializeGrant(ctx, hg))
	var gotEnd time.Time
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT end_at FROM openrails.entitlements WHERE merchant_id=$1 AND grant_id=$2 AND entitlement='legacy90' AND deleted_at IS NULL`,
		merchantID, hg.ID).Scan(&gotEnd))
	require.True(t, gotEnd.Equal(end), "window dated to the source event")
	require.True(t, gotEnd.Before(time.Now()), "a 2025 grant replays already-expired")
}

// Revoke is a new event; derive-2 retracts the projection; a grant terminates once.
func TestGrants_Revoke(t *testing.T) {
	l, pool, ctx, customer, product, merchantID := testGrants(t)
	g, err := l.Grant(ctx, grants.GrantInput{
		Customer: customer, Product: &product, Kind: grants.Entitlement, Source: grants.Purchase, SourceID: "pay_" + short(),
		Spec: &grants.Spec{Entitlements: []string{"premium"}},
	})
	require.NoError(t, err)
	require.NoError(t, l.MaterializeGrant(ctx, g))
	require.Equal(t, 1, entWindows(t, ctx, pool, merchantID, g.ID, "premium", false)) // live

	_, err = l.Revoke(ctx, g.ID, "admin removed")
	require.NoError(t, err)
	require.NoError(t, l.MaterializeGrant(ctx, g))
	require.Equal(t, 0, entWindows(t, ctx, pool, merchantID, g.ID, "premium", false)) // no longer live
	require.Equal(t, 1, entWindows(t, ctx, pool, merchantID, g.ID, "premium", true))  // recorded-revoked

	// A grant terminates at most once.
	_, err = l.Revoke(ctx, g.ID, "again")
	require.Error(t, err)
}

// Revoking a grant whose window already closed (ends_at in the past) succeeds:
// the termination row leaves ends_at NULL, so a now()-dated revoke of an
// already-expired subscription grant does not violate grants_valid_window. This
// is the bulk-reconcile-of-migrated-subscriptions case, and the natural
// cancel-a-day-after-period-end case.
func TestGrants_RevokeAlreadyExpired(t *testing.T) {
	l, pool, ctx, customer, product, merchantID := testGrants(t)
	start := time.Date(2023, 1, 1, 0, 0, 0, 0, time.UTC)
	end := time.Now().UTC().Add(-48 * time.Hour) // window closed two days ago
	g, err := l.Grant(ctx, grants.GrantInput{
		Customer: customer, Product: &product, Kind: grants.Entitlement,
		Source: grants.Subscription, SourceID: uuid.NewString(),
		Spec: &grants.Spec{Entitlements: []string{"premium"}}, StartsAt: start, EndsAt: &end,
	})
	require.NoError(t, err)
	require.NoError(t, l.MaterializeGrant(ctx, g))

	_, err = l.Revoke(ctx, g.ID, "subscription source revoked")
	require.NoError(t, err, "revoking a past-window grant must not violate grants_valid_window")

	var endsAtNull bool
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT ends_at IS NULL FROM openrails.grants WHERE merchant_id=$1 AND supersedes_id=$2 AND event='revoke'`,
		merchantID, g.ID).Scan(&endsAtNull))
	require.True(t, endsAtNull, "termination row leaves ends_at NULL")
}

// #658: the DB refuses a termination row that carries a window — the invariant
// that makes the already-expired-revoke crash structurally impossible, not just a
// code convention in terminate().
func TestGrants_TerminationRejectsWindow(t *testing.T) {
	l, pool, ctx, customer, product, merchantID := testGrants(t)
	g, err := l.Grant(ctx, grants.GrantInput{
		Customer: customer, Product: &product, Kind: grants.Entitlement,
		Source: grants.Subscription, SourceID: uuid.NewString(),
		Spec: &grants.Spec{Entitlements: []string{"premium"}},
	})
	require.NoError(t, err)

	// Otherwise-valid revoke row (satisfies grants_valid_window with a future
	// window) but carrying an ends_at — must be rejected by grants_termination_no_window.
	_, err = pool.Exec(ctx, `
		INSERT INTO openrails.grants
			(merchant_id, customer_id, kind, source_type, source_id, event, supersedes_id, starts_at, ends_at)
		VALUES ($1, $2, 'entitlement', 'subscription', $3, 'revoke', $4, now(), now() + interval '1 hour')`,
		merchantID, customer, g.SourceID, g.ID)
	require.Error(t, err, "a termination row with a window must be rejected")
	require.Contains(t, err.Error(), "grants_termination_no_window")
}

// #658: a backdated revoke records the EFFECTIVE instant (valid time) as the
// ownership revoked_at, not the convergence wall-clock — the payoff of threading
// asOf onto the termination's starts_at + reading it back for ownership status.
func TestGrants_RevokeAsOfOwnershipRevokedAt(t *testing.T) {
	l, pool, ctx, customer, product, merchantID := testGrants(t)
	g, err := l.Grant(ctx, grants.GrantInput{
		Customer: customer, Product: &product, Kind: grants.Ownership,
		Source: grants.Purchase, SourceID: "pay_" + short(),
	})
	require.NoError(t, err)
	require.NoError(t, l.MaterializeGrant(ctx, g))

	asOf := time.Date(2025, 3, 4, 5, 6, 7, 0, time.UTC) // effective end, well in the past
	_, err = l.RevokeAsOf(ctx, g.ID, "subscription source revoked", asOf)
	require.NoError(t, err)

	rows, err := gen.New(pool).ListOwnershipGrantsWithStatus(ctx, gen.ListOwnershipGrantsWithStatusParams{
		MerchantID: merchantID, CustomerID: customer,
	})
	require.NoError(t, err)
	var found bool
	for _, r := range rows {
		if r.ID != g.ID {
			continue
		}
		found = true
		require.NotNil(t, r.RevokedAt)
		require.True(t, r.RevokedAt.Equal(asOf), "ownership revoked_at is the effective instant, got %s", r.RevokedAt)
	}
	require.True(t, found, "ownership grant should appear in status list")
}

// A credit grant materializes as a #512 ledger deposit; idempotent.
func TestGrants_CreditDepositSeam(t *testing.T) {
	l, pool, ctx, customer, product, merchantID := testGrants(t)
	cur := "TC" + strings.ToUpper(short())
	amount := int64(5000)
	g, err := l.Grant(ctx, grants.GrantInput{
		Customer: customer, Product: &product, Kind: grants.Credit, Source: grants.Purchase, SourceID: "pay_" + short(),
		Amount: &amount, Currency: &cur,
	})
	require.NoError(t, err)
	require.NoError(t, l.MaterializeGrant(ctx, g))

	ml := ledger.New(gen.New(pool), merchantID)
	custAcc, err := ml.EnsureCustomerBalance(ctx, customer, cur)
	require.NoError(t, err)
	bal, err := ml.Balance(ctx, custAcc)
	require.NoError(t, err)
	require.Equal(t, amount, bal, "credit grant deposited into the #512 ledger")

	requireConserved(t, ctx, pool, merchantID, cur)

	// Idempotent — no double deposit.
	require.NoError(t, l.MaterializeGrant(ctx, g))
	bal2, err := ml.Balance(ctx, custAcc)
	require.NoError(t, err)
	require.Equal(t, amount, bal2)

	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM openrails.ledger_accounts WHERE merchant_id=$1 AND currency=$2`, merchantID, cur)
	})
}

// Ownership needs no projection — the live grant roster IS the record.
func TestGrants_Ownership(t *testing.T) {
	l, pool, ctx, customer, product, merchantID := testGrants(t)
	g, err := l.Grant(ctx, grants.GrantInput{
		Customer: customer, Product: &product, Kind: grants.Ownership, Source: grants.Purchase, SourceID: "pay_" + short(),
	})
	require.NoError(t, err)
	require.NoError(t, l.MaterializeGrant(ctx, g)) // no-op

	require.Equal(t, 1, liveOwnershipCount(t, ctx, pool, merchantID, customer, product), "owned before revoke")

	_, err = l.Revoke(ctx, g.ID, "refund")
	require.NoError(t, err)
	require.Equal(t, 0, liveOwnershipCount(t, ctx, pool, merchantID, customer, product), "not owned after revoke")
}

func TestGrants_OwnershipIncludes(t *testing.T) {
	l, pool, ctx, customer, bundle, merchantID := testGrants(t)
	childA := uuid.New()
	childB := uuid.New()
	_, err := pool.Exec(ctx, `INSERT INTO openrails.products (id, key, display_name, merchant_id) VALUES ($1, $2, $3, $4), ($5, $6, $7, $8)`,
		childA, "included-a-"+short(), "Included A", merchantID,
		childB, "included-b-"+short(), "Included B", merchantID)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `INSERT INTO openrails.product_includes (merchant_id, product_id, included_product_id) VALUES ($1, $2, $3), ($1, $2, $4)`,
		merchantID, bundle, childA, childB)
	require.NoError(t, err)

	g, err := l.Grant(ctx, grants.GrantInput{
		Customer: customer, Product: &bundle, Kind: grants.Ownership, Source: grants.Purchase, SourceID: "pay_" + short(),
	})
	require.NoError(t, err)
	require.NoError(t, l.MaterializeGrant(ctx, g))
	require.Equal(t, 1, liveOwnershipCount(t, ctx, pool, merchantID, customer, childA))
	require.Equal(t, 1, liveOwnershipCount(t, ctx, pool, merchantID, customer, childB))

	require.NoError(t, l.MaterializeGrant(ctx, g))
	require.Equal(t, 1, liveOwnershipCount(t, ctx, pool, merchantID, customer, childA), "replay must not duplicate included ownership")
	require.Equal(t, 1, liveOwnershipCount(t, ctx, pool, merchantID, customer, childB), "replay must not duplicate included ownership")

	_, err = l.Revoke(ctx, g.ID, "refund")
	require.NoError(t, err)
	require.NoError(t, l.MaterializeGrant(ctx, g))
	require.Equal(t, 0, liveOwnershipCount(t, ctx, pool, merchantID, customer, childA))
	require.Equal(t, 0, liveOwnershipCount(t, ctx, pool, merchantID, customer, childB))
}

// The grant ledger is append-only at the role level (SELECT,INSERT only).
func TestGrants_AppendOnly(t *testing.T) {
	l, _, ctx, customer, product, merchantID := testGrants(t)
	g, err := l.Grant(ctx, grants.GrantInput{
		Customer: customer, Product: &product, Kind: grants.Ownership, Source: grants.Purchase, SourceID: "pay_" + short(),
	})
	require.NoError(t, err)

	_, appDSN := dbtest.SharedRLSPostgres(t)
	conn, err := pgx.Connect(ctx, appDSN)
	require.NoError(t, err)
	defer func() { _ = conn.Close(ctx) }()
	_, err = conn.Exec(ctx, `SELECT set_config('app.merchant_id', $1, false)`, merchantID.String())
	require.NoError(t, err)

	_, err = conn.Exec(ctx, `UPDATE openrails.grants SET reason = 'x' WHERE id = $1`, g.ID)
	require.Error(t, err)
	require.Contains(t, strings.ToLower(err.Error()), "permission denied")
	_, err = conn.Exec(ctx, `DELETE FROM openrails.grants WHERE id = $1`, g.ID)
	require.Error(t, err)
	require.Contains(t, strings.ToLower(err.Error()), "permission denied")
}

// entWindows counts entitlement windows for a grant. revoked=false counts live
// windows; revoked=true counts recorded-revoked ones.
func entWindows(t *testing.T, ctx context.Context, pool *pgxpool.Pool, merchant, grantID uuid.UUID, feature string, revoked bool) int {
	t.Helper()
	cond := "revoked_at IS NULL"
	if revoked {
		cond = "revoked_at IS NOT NULL"
	}
	var n int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM openrails.entitlements WHERE merchant_id=$1 AND grant_id=$2 AND entitlement=$3 AND deleted_at IS NULL AND `+cond,
		merchant, grantID, feature).Scan(&n))
	return n
}

// requireConserved asserts the (merchant, currency) ledger nets to zero —
// double-entry conservation, read straight off the maintained counters.
func requireConserved(t *testing.T, ctx context.Context, pool *pgxpool.Pool, merchantID uuid.UUID, cur string) {
	t.Helper()
	var net int64
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT COALESCE(SUM(credits_posted - debits_posted), 0)::bigint FROM openrails.ledger_accounts WHERE merchant_id=$1 AND currency=$2`,
		merchantID, cur).Scan(&net))
	require.Equal(t, int64(0), net, "ledger conserves (Σ == 0)")
}

func liveOwnershipCount(t *testing.T, ctx context.Context, pool *pgxpool.Pool, merchant, customer, product uuid.UUID) int {
	t.Helper()
	var n int
	require.NoError(t, pool.QueryRow(ctx, `
SELECT count(*) FROM openrails.grants g
WHERE g.merchant_id = $1
  AND g.customer_id = $2
  AND g.product_id = $3
  AND g.kind = 'ownership'
  AND g.event = 'grant'
  AND NOT EXISTS (
      SELECT 1 FROM openrails.grants t
      WHERE t.merchant_id = g.merchant_id
        AND t.supersedes_id = g.id
        AND t.event IN ('revoke', 'expire', 'supersede')
  )`, merchant, customer, product).Scan(&n))
	return n
}

// TestGrants_RevokeBySource proves the #511 write-path unification: revoking an
// effect by source (RevokeBySourceAsOf) also terminates the matching live
// grants, so the ledger reflects the retraction (no "live grant, dead effect"
// residue). Scoped to the (kind, source_types, source_id) tuple; other grants
// are untouched; idempotent.
func TestGrants_RevokeBySource(t *testing.T) {
	l, pool, ctx, customer, _, merchantID := testGrants(t)
	q := gen.New(pool)
	now := time.Now().UTC()
	subID := uuid.New()
	otherSubID := uuid.New()

	// Two entitlement grants for subID (subscription + grace source) + one for a
	// different subscription + one credit grant for subID (different kind).
	gSub, err := l.Grant(ctx, grants.GrantInput{Customer: customer, Kind: grants.Entitlement, Source: grants.Subscription, SourceID: subID.String(), Spec: &grants.Spec{Entitlements: []string{"premium"}}, StartsAt: now})
	require.NoError(t, err)
	gGrace, err := l.Grant(ctx, grants.GrantInput{Customer: customer, Kind: grants.Entitlement, Source: grants.Grace, SourceID: subID.String(), Spec: &grants.Spec{Entitlements: []string{"premium"}}, StartsAt: now})
	require.NoError(t, err)
	gOther, err := l.Grant(ctx, grants.GrantInput{Customer: customer, Kind: grants.Entitlement, Source: grants.Subscription, SourceID: otherSubID.String(), Spec: &grants.Spec{Entitlements: []string{"premium"}}, StartsAt: now})
	require.NoError(t, err)

	require.NoError(t, l.RevokeBySourceAsOf(ctx, customer, grants.Entitlement,
		[]grants.SourceType{grants.Subscription, grants.Grace}, subID.String(), "subscription source revoked", time.Time{}))

	termd := func(id uuid.UUID) bool {
		v, e := q.IsGrantTerminated(ctx, gen.IsGrantTerminatedParams{MerchantID: merchantID, GrantID: id})
		require.NoError(t, e)
		return v
	}
	require.True(t, termd(gSub.ID), "subscription-source grant terminated")
	require.True(t, termd(gGrace.ID), "grace-source grant terminated")
	require.False(t, termd(gOther.ID), "other-subscription grant untouched")

	// Idempotent: a second call terminates nothing new (a grant terminates once).
	require.NoError(t, l.RevokeBySourceAsOf(ctx, customer, grants.Entitlement,
		[]grants.SourceType{grants.Subscription, grants.Grace}, subID.String(), "subscription source revoked", time.Time{}))
	require.True(t, termd(gSub.ID))
}
