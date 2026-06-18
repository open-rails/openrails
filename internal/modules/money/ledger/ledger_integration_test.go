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

// testLedger provisions a Ledger over the shared test DB on a fresh customer and
// a unique currency (so each test owns an isolated (merchant, currency) ledger).
func testLedger(t *testing.T) (*ledger.Ledger, *pgxpool.Pool, context.Context, uuid.UUID, uuid.UUID, string) {
	t.Helper()
	ctx := context.Background()
	dbi := dbtest.OpenAppDB(t, dbtest.SharedPostgresDSN(t))
	pool := dbi.Pool()
	dbtest.EnsureTestMerchant(ctx, t, pool)
	merchantID := dbtest.TestMerchantID.UUID()
	customer := uuid.New()
	_, err := pool.Exec(ctx,
		`INSERT INTO openrails.customers (id, merchant_id) VALUES ($1, $2) ON CONFLICT DO NOTHING`,
		customer, merchantID)
	require.NoError(t, err)

	currency := "TC" + strings.ReplaceAll(uuid.NewString(), "-", "")[:10]
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM openrails.ledger_transfers WHERE merchant_id = $1 AND currency = $2`, merchantID, currency)
		_, _ = pool.Exec(ctx, `DELETE FROM openrails.ledger_accounts WHERE merchant_id = $1 AND currency = $2`, merchantID, currency)
	})
	return ledger.New(gen.New(pool), merchantID), pool, ctx, customer, merchantID, currency
}

// Deposit -> spend -> expire: balances move correctly and the ledger conserves.
func TestLedger_ConservationAndFlows(t *testing.T) {
	l, _, ctx, customer, _, cur := testLedger(t)

	_, err := l.Deposit(ctx, customer, cur, 1000, "grant", uuid.NewString(), uuid.New())
	require.NoError(t, err)
	_, err = l.Spend(ctx, customer, cur, 300, "invoker-1", "gpt", 0)
	require.NoError(t, err)
	_, err = l.Expire(ctx, customer, cur, 200, "grant", uuid.NewString())
	require.NoError(t, err)

	custAcc, err := l.EnsureCustomerBalance(ctx, customer, cur)
	require.NoError(t, err)
	clearing, err := l.EnsureSystemAccount(ctx, ledger.ProcessorClearing, cur)
	require.NoError(t, err)
	rev, err := l.EnsureSystemAccount(ctx, ledger.PlatformRevenue, cur)
	require.NoError(t, err)
	exp, err := l.EnsureSystemAccount(ctx, ledger.ExpiredCredits, cur)
	require.NoError(t, err)

	mustBalance(t, ctx, l, custAcc, 500)    // 1000 - 300 - 200
	mustBalance(t, ctx, l, clearing, -1000) // funded the deposit
	mustBalance(t, ctx, l, rev, 300)        // received the spend
	mustBalance(t, ctx, l, exp, 200)        // received the expiry

	net, err := l.Conservation(ctx, cur)
	require.NoError(t, err)
	require.Equal(t, int64(0), net, "every (merchant,currency) ledger must net to zero")
}

// Authorize holds funds; Capture posts the actual; the remainder is released;
// a pending may be resolved at most once.
func TestLedger_TwoPhase(t *testing.T) {
	l, _, ctx, customer, _, cur := testLedger(t)
	_, err := l.Deposit(ctx, customer, cur, 1000, "grant", uuid.NewString(), uuid.New())
	require.NoError(t, err)
	custAcc, err := l.EnsureCustomerBalance(ctx, customer, cur)
	require.NoError(t, err)

	hold, err := l.Authorize(ctx, customer, cur, 400, "invoker-1", "gpt")
	require.NoError(t, err)
	mustBalance(t, ctx, l, custAcc, 1000) // pending does not post
	mustHeld(t, ctx, l, custAcc, 400)
	mustAvailable(t, ctx, l, custAcc, 600)

	_, err = l.Capture(ctx, hold.ID, 250, 0) // actual < authorized
	require.NoError(t, err)
	mustBalance(t, ctx, l, custAcc, 750) // 1000 - 250
	mustHeld(t, ctx, l, custAcc, 0)      // hold resolved -> remainder freed

	// A resolved pending cannot be resolved again (unique resolution index).
	_, err = l.Capture(ctx, hold.ID, 100, 0)
	require.Error(t, err)
	_, err = l.Release(ctx, hold.ID)
	require.Error(t, err)

	// A released hold frees its reservation without touching the balance.
	hold2, err := l.Authorize(ctx, customer, cur, 100, "invoker-1", "gpt")
	require.NoError(t, err)
	mustHeld(t, ctx, l, custAcc, 100)
	_, err = l.Release(ctx, hold2.ID)
	require.NoError(t, err)
	mustHeld(t, ctx, l, custAcc, 0)
	mustBalance(t, ctx, l, custAcc, 750)

	net, err := l.Conservation(ctx, cur)
	require.NoError(t, err)
	require.Equal(t, int64(0), net)
}

// A customer_balance cannot be overdrawn past its arrears floor.
func TestLedger_SignConstraint(t *testing.T) {
	l, _, ctx, customer, _, cur := testLedger(t)
	_, err := l.Deposit(ctx, customer, cur, 100, "grant", uuid.NewString(), uuid.New())
	require.NoError(t, err)
	custAcc, err := l.EnsureCustomerBalance(ctx, customer, cur)
	require.NoError(t, err)

	// No credit line: overspend rejected, balance unchanged.
	_, err = l.Spend(ctx, customer, cur, 150, "invoker-1", "gpt", 0)
	require.ErrorIs(t, err, ledger.ErrInsufficientFunds)
	mustBalance(t, ctx, l, custAcc, 100)

	// With a 100 arrears floor, the same spend is allowed and goes negative.
	_, err = l.Spend(ctx, customer, cur, 150, "invoker-1", "gpt", 100)
	require.NoError(t, err)
	mustBalance(t, ctx, l, custAcc, -50)

	net, err := l.Conservation(ctx, cur)
	require.NoError(t, err)
	require.Equal(t, int64(0), net)
}

// A transfer may never cross ledgers (the currency-guard trigger).
func TestLedger_CurrencyGuard(t *testing.T) {
	l, pool, ctx, customer, merchantID, curA := testLedger(t)
	curB := "TC" + strings.ReplaceAll(uuid.NewString(), "-", "")[:10]
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM openrails.ledger_accounts WHERE merchant_id = $1 AND currency = $2`, merchantID, curB)
	})

	custA, err := l.EnsureCustomerBalance(ctx, customer, curA)
	require.NoError(t, err)
	clearingB, err := l.EnsureSystemAccount(ctx, ledger.ProcessorClearing, curB)
	require.NoError(t, err)

	// Debit account is in curB, transfer/credit in curA — the trigger rejects it.
	_, err = l.Apply(ctx, ledger.Transfer{
		Debit: clearingB, Credit: custA, Amount: 10, Currency: curA, Type: "deposit",
	})
	require.Error(t, err)
	require.Contains(t, strings.ToLower(err.Error()), "cross-currency")
}

// The app role (NOBYPASSRLS) is granted SELECT,INSERT only — UPDATE/DELETE on a
// transfer are denied, proving the ledger is append-only at the role level.
func TestLedger_AppendOnly(t *testing.T) {
	l, _, ctx, customer, merchantID, cur := testLedger(t)
	tr, err := l.Deposit(ctx, customer, cur, 100, "grant", uuid.NewString(), uuid.New())
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

func mustBalance(t *testing.T, ctx context.Context, l *ledger.Ledger, acc uuid.UUID, want int64) {
	t.Helper()
	got, err := l.Balance(ctx, acc)
	require.NoError(t, err)
	require.Equal(t, want, got, "balance of %s", acc)
}

func mustHeld(t *testing.T, ctx context.Context, l *ledger.Ledger, acc uuid.UUID, want int64) {
	t.Helper()
	got, err := l.Held(ctx, acc)
	require.NoError(t, err)
	require.Equal(t, want, got, "held of %s", acc)
}

func mustAvailable(t *testing.T, ctx context.Context, l *ledger.Ledger, acc uuid.UUID, want int64) {
	t.Helper()
	got, err := l.Available(ctx, acc)
	require.NoError(t, err)
	require.Equal(t, want, got, "available of %s", acc)
}
