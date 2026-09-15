//go:build integration

package ledger_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/modules/money/ledger"
)

// Pause after the first real database result. This forces a second writer to
// finish before the first helper continues, without replacing queries/results.
type pausedResultDB struct {
	gen.DBTX
	afterResult func()
}

func (d pausedResultDB) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	return pausedResultRow{Row: d.DBTX.QueryRow(ctx, sql, args...), afterResult: d.afterResult}
}

type pausedResultRow struct {
	pgx.Row
	afterResult func()
}

func (r pausedResultRow) Scan(dest ...any) error {
	err := r.Row.Scan(dest...)
	r.afterResult()
	return err
}

func TestLedger_ReplayPreservesCountersAndDepletedBalance(t *testing.T) {
	for _, spend := range []bool{false, true} {
		name := "deposit"
		if spend {
			name = "depleting_spend"
		}
		t.Run(name, func(t *testing.T) {
			l, pool, baseCtx, customer, merchantID, cur := testLedger(t)
			ctx, cancel := context.WithTimeout(baseCtx, 10*time.Second)
			defer cancel()
			balance, err := l.EnsureCustomerBalance(ctx, customer, cur)
			require.NoError(t, err)
			clearing, err := l.EnsureSystemAccount(ctx, ledger.RailClearing, cur)
			require.NoError(t, err)
			transfer := ledger.Transfer{
				Debit: clearing, Credit: balance, Amount: 1000, Currency: cur,
				Type: ledger.Deposit, Customer: &customer,
				Coord: ledger.Coord{Operation: ledger.OpDeposit, Source: "replay-test", SourceID: uuid.NewString()},
			}
			wantBalance := int64(1000)
			if spend {
				_, err := l.Deposit(ctx, customer, cur, 1000, ledger.Coord{Operation: ledger.OpDeposit, Source: "funding", SourceID: uuid.NewString()}, uuid.Nil)
				require.NoError(t, err)
				revenue, err := l.EnsureSystemAccount(ctx, ledger.PlatformRevenue, cur)
				require.NoError(t, err)
				transfer.Debit, transfer.Credit = balance, revenue
				transfer.Type, transfer.Coord.Operation = ledger.CreditSpend, ledger.OpSpend
				wantBalance = 0
			}

			paused, resume := make(chan struct{}), make(chan struct{})
			release := sync.OnceFunc(func() { close(resume) })
			defer release()
			queries := gen.New(pausedResultDB{DBTX: pool, afterResult: sync.OnceFunc(func() {
				close(paused)
				select {
				case <-resume:
				case <-ctx.Done():
				}
			})})
			type result struct {
				row     gen.OpenrailsLedgerTransfer
				applied bool
				err     error
			}
			first := make(chan result, 1)
			go func() {
				row, applied, err := ledger.New(queries, merchantID).ApplyIdempotent(ctx, transfer)
				first <- result{row, applied, err}
			}()
			select {
			case <-paused:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			secondRow, secondApplied, secondErr := l.ApplyIdempotent(ctx, transfer)
			release()
			firstResult := <-first
			require.NoError(t, firstResult.err)
			require.NoError(t, secondErr)
			require.NotEqual(t, firstResult.applied, secondApplied, "one writer applies; the other replays")
			require.Equal(t, firstResult.row.ID, secondRow.ID)
			// Replaying again after all balance changes must retain the receipt.
			row, applied, err := l.ApplyIdempotent(ctx, transfer)
			require.NoError(t, err)
			require.False(t, applied)
			require.Equal(t, secondRow.ID, row.ID)
			mustBalance(t, ctx, l, balance, wantBalance)
			requireNoCounterDrift(t, ctx, pool, merchantID, cur)
			requireLedgerNetZero(t, ctx, pool, merchantID, cur)
		})
	}
}

func TestLedger_RollbackAndRejectedInsertPreserveCounters(t *testing.T) {
	l, pool, ctx, customer, merchantID, cur := testLedger(t)
	_, err := l.Deposit(ctx, customer, cur, 1000, ledger.Coord{Operation: ledger.OpDeposit, Source: "funding", SourceID: uuid.NewString()}, uuid.Nil)
	require.NoError(t, err)
	balance, err := l.EnsureCustomerBalance(ctx, customer, cur)
	require.NoError(t, err)
	revenue, err := l.EnsureSystemAccount(ctx, ledger.PlatformRevenue, cur)
	require.NoError(t, err)
	transfer := ledger.Transfer{
		Debit: balance, Credit: revenue, Amount: 1000, Currency: cur,
		Type: ledger.CreditSpend, Customer: &customer,
		Coord: ledger.Coord{Operation: ledger.OpSpend, Source: "rollback-test", SourceID: uuid.NewString()},
	}
	tx, err := pool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(ctx) }()
	txLedger := ledger.New(gen.New(tx), merchantID)
	rolledBack, applied, err := txLedger.ApplyIdempotent(ctx, transfer)
	require.NoError(t, err)
	require.True(t, applied)
	mustBalance(t, ctx, txLedger, balance, 0)
	require.NoError(t, tx.Rollback(ctx))
	var retained bool
	require.NoError(t, pool.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM openrails.ledger_transfers WHERE id=$1)", rolledBack.ID).Scan(&retained))
	require.False(t, retained)

	transfer.Amount++
	_, applied, err = l.ApplyIdempotent(ctx, transfer)
	require.ErrorIs(t, err, ledger.ErrInsufficientFunds)
	require.False(t, applied)
	mustBalance(t, ctx, l, balance, 1000)
	mustBalance(t, ctx, l, revenue, 0)
	requireNoCounterDrift(t, ctx, pool, merchantID, cur)
	requireLedgerNetZero(t, ctx, pool, merchantID, cur)
}
