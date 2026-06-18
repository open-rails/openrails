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
	dbi := dbtest.OpenAppDB(t, dbtest.SharedPostgresDSN(t))
	pool := dbi.Pool()
	dbtest.EnsureTestMerchant(ctx, t, pool)
	merchantID := dbtest.TestMerchantID.UUID()
	customer := uuid.New()
	product := uuid.New()
	_, err := pool.Exec(ctx, `INSERT INTO openrails.customers (id, merchant_id) VALUES ($1, $2) ON CONFLICT DO NOTHING`, customer, merchantID)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `INSERT INTO openrails.products (id, slug, display_name, merchant_id) VALUES ($1, $2, $3, $4) ON CONFLICT DO NOTHING`,
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

// A credit grant materializes as a #512 ledger deposit; idempotent.
func TestGrants_CreditDepositSeam(t *testing.T) {
	l, pool, ctx, customer, product, merchantID := testGrants(t)
	cur := "TC" + short()
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

	net, err := ml.Conservation(ctx, cur)
	require.NoError(t, err)
	require.Equal(t, int64(0), net)

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
	l, _, ctx, customer, product, _ := testGrants(t)
	g, err := l.Grant(ctx, grants.GrantInput{
		Customer: customer, Product: &product, Kind: grants.Ownership, Source: grants.Purchase, SourceID: "pay_" + short(),
	})
	require.NoError(t, err)
	require.NoError(t, l.MaterializeGrant(ctx, g)) // no-op

	live, err := l.LiveGrants(ctx, customer)
	require.NoError(t, err)
	require.True(t, containsGrant(live, g.ID), "owned before revoke")

	_, err = l.Revoke(ctx, g.ID, "refund")
	require.NoError(t, err)
	live, err = l.LiveGrants(ctx, customer)
	require.NoError(t, err)
	require.False(t, containsGrant(live, g.ID), "not owned after revoke")
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

func containsGrant(gs []gen.OpenrailsGrant, id uuid.UUID) bool {
	for i := range gs {
		if gs[i].ID == id {
			return true
		}
	}
	return false
}
