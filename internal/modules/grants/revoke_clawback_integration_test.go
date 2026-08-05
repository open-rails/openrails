//go:build integration

package grants_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/modules/grants"
	"github.com/open-rails/openrails/internal/modules/money/ledger"
)

// Revoking a credit grant + re-deriving it (Revoke, then MaterializeGrant —
// the converge repair path) claws back its UNSPENT remainder to
// revoked_credits: the lot becomes unspendable, the derived balance drops by
// the unspent amount, the money is frozen (not refunded), conservation holds,
// and re-deriving is idempotent. (#514, docs/consistency-invariants.md §11
// decision 4.)
func TestGrants_RevokeClawback(t *testing.T) {
	l, pool, ctx, customer, product, merchantID := testGrants(t)
	cur := "TC" + strings.ToUpper(short())
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM openrails.ledger_accounts WHERE merchant_id=$1 AND currency=$2`, merchantID, cur)
	})
	ml := ledger.New(gen.New(pool), merchantID)
	custAcc, err := ml.EnsureCustomerBalance(ctx, customer, cur)
	require.NoError(t, err)

	lot := mustCreditLot(t, ctx, l, customer, product, cur, 100, time.Now().UTC(), nil)
	_, csErr := l.CreditSpend(ctx, customer, cur, 30, "inv", "gpt", ledger.Coord{Operation: ledger.OpSpend, Source: "spend", SourceID: "req-1"})
	require.NoError(t, csErr)
	mustBal(t, ctx, ml, custAcc, 70)

	_, err = l.Revoke(ctx, lot.ID, "admin removed")
	require.NoError(t, err)
	require.NoError(t, l.MaterializeGrant(ctx, lot)) // derive-2 retracts: clawback

	// The revoked lot is no longer spendable…
	require.False(t, lotSpendable(t, ctx, pool, merchantID, customer, cur, lot.ID))
	// …the derived balance drops to 0 (only the unspent 70 retracted)…
	mustBal(t, ctx, ml, custAcc, 0)
	// …the money is frozen in revoked_credits (NOT refunded)…
	require.Equal(t, int64(70), sysBal(t, ctx, ml, ledger.RevokedCredits, cur))
	// …the spent 30 stays as revenue…
	require.Equal(t, int64(30), sysBal(t, ctx, ml, ledger.PlatformRevenue, cur))
	requireConserved(t, ctx, pool, merchantID, cur)

	// Idempotent: re-deriving the revoked grant claws nothing more.
	require.NoError(t, l.MaterializeGrant(ctx, lot))
	mustBal(t, ctx, ml, custAcc, 0)
	require.Equal(t, int64(70), sysBal(t, ctx, ml, ledger.RevokedCredits, cur))
	requireConserved(t, ctx, pool, merchantID, cur)
}

// The clawback is a pure reversing transfer, so it is reversible at the ledger
// level: returning the frozen remainder to customer_balance restores the balance
// and conservation — no value was destroyed.
func TestGrants_RevokeClawbackReversible(t *testing.T) {
	l, pool, ctx, customer, product, merchantID := testGrants(t)
	cur := "TC" + strings.ToUpper(short())
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM openrails.ledger_accounts WHERE merchant_id=$1 AND currency=$2`, merchantID, cur)
	})
	ml := ledger.New(gen.New(pool), merchantID)
	custAcc, err := ml.EnsureCustomerBalance(ctx, customer, cur)
	require.NoError(t, err)

	lot := mustCreditLot(t, ctx, l, customer, product, cur, 100, time.Now().UTC(), nil)
	_, err = l.Revoke(ctx, lot.ID, "mistake")
	require.NoError(t, err)
	require.NoError(t, l.MaterializeGrant(ctx, lot))
	mustBal(t, ctx, ml, custAcc, 0)
	require.Equal(t, int64(100), sysBal(t, ctx, ml, ledger.RevokedCredits, cur))

	// Reverse the clawback: revoked_credits -> customer_balance.
	revAcc, err := ml.EnsureSystemAccount(ctx, ledger.RevokedCredits, cur)
	require.NoError(t, err)
	c := customer
	_, err = ml.Apply(ctx, ledger.Transfer{
		Debit: revAcc, Credit: custAcc, Amount: 100, Currency: cur, Type: ledger.CreditReinstate,
		Coord:   ledger.Coord{Operation: ledger.OpCreditReinstate, Source: "grant_reinstate", SourceID: lot.ID.String()},
		GrantID: &lot.ID, Customer: &c,
	})
	require.NoError(t, err)
	mustBal(t, ctx, ml, custAcc, 100) // restored
	require.Equal(t, int64(0), sysBal(t, ctx, ml, ledger.RevokedCredits, cur))
	requireConserved(t, ctx, pool, merchantID, cur)
}

// #677: two overlapping repairs of the same terminated credit grant (each the
// converge Repair body — a tx taking the per-customer spend lock, then
// MaterializeGrant) produce exactly ONE clawback: the loser blocks on the lock,
// re-reads remaining=0 and no-ops. The 057 partial unique index
// (idx_ledger_transfers_lot_once) backstops the same invariant in the DB.
func TestGrants_ConcurrentClawback_SingleClawback(t *testing.T) {
	l, pool, ctx, customer, product, merchantID := testGrants(t)
	cur := "TC" + strings.ToUpper(short())
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM openrails.ledger_accounts WHERE merchant_id=$1 AND currency=$2`, merchantID, cur)
	})
	ml := ledger.New(gen.New(pool), merchantID)
	custAcc, err := ml.EnsureCustomerBalance(ctx, customer, cur)
	require.NoError(t, err)

	lot := mustCreditLot(t, ctx, l, customer, product, cur, 100, time.Now().UTC(), nil)
	_, err = l.Revoke(ctx, lot.ID, "admin removed")
	require.NoError(t, err)

	claw := func() error {
		tx, err := pool.Begin(ctx)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback(ctx) }()
		gl := grants.New(gen.New(tx), merchantID)
		if err := gl.LockCustomer(ctx, customer); err != nil {
			return err
		}
		if err := gl.MaterializeGrant(ctx, lot); err != nil {
			return err
		}
		return tx.Commit(ctx)
	}
	errs := make(chan error, 2)
	go func() { errs <- claw() }()
	go func() { errs <- claw() }()
	require.NoError(t, <-errs)
	require.NoError(t, <-errs)

	var n int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM openrails.ledger_transfers WHERE merchant_id=$1 AND grant_id=$2 AND transfer_type='credit_revoke'`,
		merchantID, lot.ID).Scan(&n))
	require.Equal(t, 1, n, "exactly one clawback transfer")
	mustBal(t, ctx, ml, custAcc, 0)
	require.Equal(t, int64(100), sysBal(t, ctx, ml, ledger.RevokedCredits, cur))
	requireConserved(t, ctx, pool, merchantID, cur)
}

// --- helpers ---

func sysBal(t *testing.T, ctx context.Context, ml *ledger.Ledger, at ledger.AccountType, cur string) int64 {
	t.Helper()
	acc, err := ml.EnsureSystemAccount(ctx, at, cur)
	require.NoError(t, err)
	b, err := ml.Balance(ctx, acc)
	require.NoError(t, err)
	return b
}

func lotSpendable(t *testing.T, ctx context.Context, pool *pgxpool.Pool, merchantID, customer uuid.UUID, cur string, lotID uuid.UUID) bool {
	t.Helper()
	lots, err := gen.New(pool).ListSpendableCreditLots(ctx, gen.ListSpendableCreditLotsParams{
		MerchantID: merchantID, CustomerID: customer, Currency: cur, AsOf: time.Now().UTC(),
	})
	require.NoError(t, err)
	for _, lot := range lots {
		if lot.ID == lotID {
			return true
		}
	}
	return false
}
