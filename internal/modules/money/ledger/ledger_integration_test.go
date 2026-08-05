//go:build integration

package ledger_test

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/modules/money/ledger"
)

func TestMain(m *testing.M) { dbtest.RunMain(m) }

// ledgerOwnerPool is the PRIVILEGED handle. The ledger is append-only BY DESIGN
// — openrails_app holds SELECT,INSERT and nothing else on ledger_transfers /
// ledger_accounts (0001_schema) — so a fixture that deletes its rows, rewrites a
// counter, or disables the counter trigger is doing something no production role
// can do, and must say so by asking for the owner rather than by widening the
// grant.
func ledgerOwnerPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	return dbtest.SharedSuperuserPGXPool(t)
}

// testLedger provisions a Ledger over the shared test DB on a fresh customer and
// a unique currency (so each test owns an isolated (merchant, currency) ledger).
func testLedger(t *testing.T) (*ledger.Ledger, *pgxpool.Pool, context.Context, uuid.UUID, uuid.UUID, string) {
	t.Helper()
	ctx := context.Background()
	dbi := dbtest.OpenMerchantDB(t, dbtest.TestMerchantID.UUID())
	pool := dbi.Pool()
	dbtest.EnsureTestMerchant(ctx, t, pool)
	merchantID := dbtest.TestMerchantID.UUID()
	customer := uuid.New()
	_, err := pool.Exec(ctx,
		`INSERT INTO openrails.customers (id, merchant_id) VALUES ($1, $2) ON CONFLICT DO NOTHING`,
		customer, merchantID)
	require.NoError(t, err)

	currency := "TC" + strings.ToUpper(strings.ReplaceAll(uuid.NewString(), "-", "")[:10])
	t.Cleanup(func() {
		owner := ledgerOwnerPool(t)
		_, _ = owner.Exec(ctx, `DELETE FROM openrails.ledger_transfers WHERE merchant_id = $1 AND currency = $2`, merchantID, currency)
		_, _ = owner.Exec(ctx, `DELETE FROM openrails.ledger_accounts WHERE merchant_id = $1 AND currency = $2`, merchantID, currency)
	})
	return ledger.New(gen.New(pool), merchantID), pool, ctx, customer, merchantID, currency
}

// Deposit -> spend -> expire transfers: balances move correctly and the ledger
// conserves. (The spend/expire flows post through Apply — the module-level
// wrappers live in grants.CreditSpend/ExpireLapsed.)
func TestLedger_ConservationAndFlows(t *testing.T) {
	l, pool, ctx, customer, merchantID, cur := testLedger(t)

	_, err := l.Deposit(ctx, customer, cur, 1000, ledger.Coord{Operation: ledger.OpDeposit, Source: "grant", SourceID: uuid.NewString()}, uuid.New())
	require.NoError(t, err)

	custAcc, err := l.EnsureCustomerBalance(ctx, customer, cur)
	require.NoError(t, err)
	clearing, err := l.EnsureSystemAccount(ctx, ledger.RailClearing, cur)
	require.NoError(t, err)
	rev, err := l.EnsureSystemAccount(ctx, ledger.PlatformRevenue, cur)
	require.NoError(t, err)
	exp, err := l.EnsureSystemAccount(ctx, ledger.ExpiredCredits, cur)
	require.NoError(t, err)

	c := customer
	_, err = l.Apply(ctx, ledger.Transfer{
		Debit: custAcc, Credit: rev, Amount: 300, Currency: cur, Type: "credit_spend", Customer: &c,
		Coord: ledger.Coord{Operation: ledger.OpSpend, Source: "spend", SourceID: uuid.NewString()},
	})
	require.NoError(t, err)
	_, err = l.Apply(ctx, ledger.Transfer{
		Debit: custAcc, Credit: exp, Amount: 200, Currency: cur, Type: "credit_expire", Customer: &c,
		Coord: ledger.Coord{Operation: ledger.OpCreditExpire, Source: "credit_expiry", SourceID: uuid.NewString()},
	})
	require.NoError(t, err)

	mustBalance(t, ctx, l, custAcc, 500)    // 1000 - 300 - 200
	mustBalance(t, ctx, l, clearing, -1000) // funded the deposit
	mustBalance(t, ctx, l, rev, 300)        // received the spend
	mustBalance(t, ctx, l, exp, 200)        // received the expiry

	requireLedgerNetZero(t, ctx, pool, merchantID, cur)
	requireNoCounterDrift(t, ctx, pool, merchantID, cur)
}

// A customer_balance cannot be overdrawn past its arrears floor.
func TestLedger_SignConstraint(t *testing.T) {
	l, pool, ctx, customer, merchantID, cur := testLedger(t)
	_, err := l.Deposit(ctx, customer, cur, 100, ledger.Coord{Operation: ledger.OpDeposit, Source: "grant", SourceID: uuid.NewString()}, uuid.New())
	require.NoError(t, err)
	custAcc, err := l.EnsureCustomerBalance(ctx, customer, cur)
	require.NoError(t, err)
	rev, err := l.EnsureSystemAccount(ctx, ledger.PlatformRevenue, cur)
	require.NoError(t, err)
	c := customer

	// No credit line: overspend rejected, balance unchanged.
	_, err = l.Apply(ctx, ledger.Transfer{
		Debit: custAcc, Credit: rev, Amount: 150, Currency: cur, Type: "credit_spend", Customer: &c,
		Coord: ledger.Coord{Operation: ledger.OpSpend, Source: "spend", SourceID: uuid.NewString()},
	})
	require.ErrorIs(t, err, ledger.ErrInsufficientFunds)
	mustBalance(t, ctx, l, custAcc, 100)

	// With a 100 arrears floor, the same spend is allowed and goes negative.
	_, err = l.Apply(ctx, ledger.Transfer{
		Debit: custAcc, Credit: rev, Amount: 150, Currency: cur, Type: "credit_spend", Customer: &c,
		Coord:                  ledger.Coord{Operation: ledger.OpSpend, Source: "spend", SourceID: uuid.NewString()},
		AllowDebitNegativeUpTo: 100,
	})
	require.NoError(t, err)
	mustBalance(t, ctx, l, custAcc, -50)

	requireLedgerNetZero(t, ctx, pool, merchantID, cur)
	requireNoCounterDrift(t, ctx, pool, merchantID, cur)
}

// A transfer may never cross ledgers (the currency-guard trigger).
func TestLedger_CurrencyGuard(t *testing.T) {
	l, pool, ctx, customer, merchantID, curA := testLedger(t)
	curB := "TC" + strings.ToUpper(strings.ReplaceAll(uuid.NewString(), "-", "")[:10])
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM openrails.ledger_accounts WHERE merchant_id = $1 AND currency = $2`, merchantID, curB)
	})

	custA, err := l.EnsureCustomerBalance(ctx, customer, curA)
	require.NoError(t, err)
	clearingB, err := l.EnsureSystemAccount(ctx, ledger.RailClearing, curB)
	require.NoError(t, err)

	// Debit account is in curB, transfer/credit in curA — the trigger rejects it.
	_, err = l.Apply(ctx, ledger.Transfer{
		Debit: clearingB, Credit: custA, Amount: 10, Currency: curA, Type: "deposit",
		Coord: ledger.Coord{Operation: ledger.OpDeposit, Source: "grant", SourceID: uuid.NewString()},
	})
	require.Error(t, err)
	require.Contains(t, strings.ToLower(err.Error()), "cross-currency")
}

// The app role (NOBYPASSRLS) is granted SELECT,INSERT only — UPDATE/DELETE on a
// transfer are denied, proving the ledger is append-only at the role level.
func TestLedger_AppendOnly(t *testing.T) {
	l, _, ctx, customer, merchantID, cur := testLedger(t)
	tr, err := l.Deposit(ctx, customer, cur, 100, ledger.Coord{Operation: ledger.OpDeposit, Source: "grant", SourceID: uuid.NewString()}, uuid.New())
	require.NoError(t, err)

	_, appDSN := dbtest.SharedRLSPostgres(t)
	conn, err := pgx.Connect(ctx, appDSN)
	require.NoError(t, err)
	defer func() { _ = conn.Close(ctx) }()
	_, err = conn.Exec(ctx, `SELECT set_config('app.merchant_id', $1, false)`, merchantID.String())
	require.NoError(t, err)

	// Visible (SELECT granted, same merchant under RLS)...
	var seen int
	require.NoError(t, conn.QueryRow(ctx, `SELECT count(*) FROM openrails.ledger_transfers WHERE id = $1`, tr.ID).Scan(&seen))
	require.Equal(t, 1, seen)

	// ...but not mutable.
	_, err = conn.Exec(ctx, `UPDATE openrails.ledger_transfers SET amount = amount + 1 WHERE id = $1`, tr.ID)
	require.Error(t, err)
	require.Contains(t, strings.ToLower(err.Error()), "permission denied")

	_, err = conn.Exec(ctx, `DELETE FROM openrails.ledger_transfers WHERE id = $1`, tr.ID)
	require.Error(t, err)
	require.Contains(t, strings.ToLower(err.Error()), "permission denied")
}

// requireLedgerNetZero asserts double-entry conservation for the (merchant,
// currency) ledger, read straight off the maintained counters.
func requireLedgerNetZero(t *testing.T, ctx context.Context, pool *pgxpool.Pool, merchantID uuid.UUID, cur string) {
	t.Helper()
	var net int64
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT COALESCE(SUM(credits_posted - debits_posted), 0)::bigint FROM openrails.ledger_accounts WHERE merchant_id=$1 AND currency=$2`,
		merchantID, cur).Scan(&net))
	require.Equal(t, int64(0), net, "every (merchant,currency) ledger must net to zero")
}

func mustBalance(t *testing.T, ctx context.Context, l *ledger.Ledger, acc uuid.UUID, want int64) {
	t.Helper()
	got, err := l.Balance(ctx, acc)
	require.NoError(t, err)
	require.Equal(t, want, got, "balance of %s", acc)
}

func requireNoCounterDrift(t *testing.T, ctx context.Context, pool *pgxpool.Pool, merchantID uuid.UUID, currency string) {
	t.Helper()
	rows, err := pool.Query(ctx, `
WITH actual AS (
    SELECT
        a.id,
        COALESCE(SUM(t.amount) FILTER (WHERE t.credit_account_id = a.id), 0)::bigint AS credits_posted,
        COALESCE(SUM(t.amount) FILTER (WHERE t.debit_account_id = a.id), 0)::bigint AS debits_posted
    FROM openrails.ledger_accounts a
    LEFT JOIN openrails.ledger_transfers t
      ON t.merchant_id = a.merchant_id
     AND (t.debit_account_id = a.id OR t.credit_account_id = a.id)
    WHERE a.merchant_id = $1 AND a.currency = $2
    GROUP BY a.id
)
SELECT a.id
FROM openrails.ledger_accounts a
JOIN actual ON actual.id = a.id
WHERE a.merchant_id = $1
  AND a.currency = $2
  AND (
      a.credits_posted <> actual.credits_posted
      OR a.debits_posted <> actual.debits_posted
  )`, merchantID, currency)
	require.NoError(t, err)
	defer rows.Close()
	var drift []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		require.NoError(t, rows.Scan(&id))
		drift = append(drift, id)
	}
	require.NoError(t, rows.Err())
	require.Empty(t, drift, "ledger account counters must match transfer re-sum")
}
