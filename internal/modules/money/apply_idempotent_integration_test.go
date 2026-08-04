//go:build integration

package money_test

import (
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/internal/modules/money/ledger"
)

// or#892: once-only is now a DATABASE fact. These prove the two claims that
// distinguish this from or#891's Go-side guards:
//
//  1. The unique index refuses a duplicate even when the check-then-insert in Go
//     is BYPASSED entirely — so a future spend path that forgets lockBalance
//     cannot double-post.
//  2. Every durable write reports applied-vs-replayed, which is what consumers
//     were rebuilding claim tables to obtain.

// The DATABASE is the enforcement. This bypasses EVERY Go-side guard —
// ApplyIdempotent's read, lockBalance, the SumLedgerSpendByCoords pre-check —
// and drives the generated INSERT twice at one coordinate. The second must
// return zero rows (ON CONFLICT DO NOTHING fired), which is the only evidence
// that a future spend path forgetting the lock still cannot double-post.
//
// Measured without idx_ledger_transfers_operation_once, the same two inserts
// leave 2 rows and 10,000 micros at one coordinate instead of 5,000.
func TestOr892_TheDatabaseRefusesADuplicateCoordinate(t *testing.T) {
	_, _, pool, payer, cur, ctx := moneyInEnvWithDB(t)
	merchantID := dbtest.TestMerchantID.UUID()
	customer := payer.UUID()
	dbtest.EnsureCustomerIDPgx(ctx, t, pool, customer.String())

	q := gen.New(pool)
	l := ledger.New(q, merchantID)
	clearing, err := l.EnsureSystemAccount(ctx, ledger.RailClearing, cur)
	require.NoError(t, err)
	balance, err := l.EnsureCustomerBalance(ctx, customer, cur)
	require.NoError(t, err)

	sourceID := uuid.NewString()
	params := gen.InsertLedgerTransferParams{
		MerchantID: merchantID, DebitAccountID: clearing, CreditAccountID: balance,
		Amount: 5_000, Currency: cur, TransferType: string(ledger.Deposit),
		Operation: string(ledger.OpDeposit), Source: "or892-raw", SourceID: sourceID,
		CustomerID: &customer,
	}

	_, err = q.InsertLedgerTransfer(ctx, params)
	require.NoError(t, err, "the first insert at a coordinate lands")

	_, err = q.InsertLedgerTransfer(ctx, params)
	require.ErrorIs(t, err, pgx.ErrNoRows,
		"ON CONFLICT DO NOTHING must swallow the duplicate and return zero rows")

	var rows int
	var moved int64
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT count(*), COALESCE(sum(amount), 0)::bigint FROM openrails.ledger_transfers
		 WHERE merchant_id = $1 AND source = 'or892-raw' AND source_id = $2
	`, merchantID, sourceID).Scan(&rows, &moved))
	require.Equal(t, 1, rows, "one coordinate, one row — enforced by the index alone")
	require.Equal(t, int64(5_000), moved)
}

// ApplyIdempotent turns that refusal into a usable answer rather than an error:
// the replay returns the transfer that DID land, and says it did not apply.
func TestOr892_ApplyIdempotentReportsReplayInsteadOfErroring(t *testing.T) {
	_, _, pool, payer, cur, ctx := moneyInEnvWithDB(t)
	merchantID := dbtest.TestMerchantID.UUID()
	customer := payer.UUID()
	// These drive the ledger DIRECTLY, below the money service that would
	// normally materialize the customers row, so seed it for the account FK.
	dbtest.EnsureCustomerIDPgx(ctx, t, pool, customer.String())

	l := ledger.New(gen.New(pool), merchantID)
	clearing, err := l.EnsureSystemAccount(ctx, ledger.RailClearing, cur)
	require.NoError(t, err)
	balance, err := l.EnsureCustomerBalance(ctx, customer, cur)
	require.NoError(t, err)

	coord := ledger.Coord{Operation: ledger.OpDeposit, Source: "or892", SourceID: uuid.NewString()}
	transfer := ledger.Transfer{
		Debit: clearing, Credit: balance, Amount: 4_000, Currency: cur,
		Type: ledger.Deposit, Coord: coord, Customer: &customer,
	}

	first, applied, err := l.ApplyIdempotent(ctx, transfer)
	require.NoError(t, err)
	require.True(t, applied, "the first write at a coordinate applies")

	second, applied, err := l.ApplyIdempotent(ctx, transfer)
	require.NoError(t, err, "a replay is not an error — it is a replay")
	require.False(t, applied, "the SECOND write at the same coordinate must not apply")
	require.Equal(t, first.ID, second.ID, "a replay returns the transfer that actually landed")

	var rows int
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT count(*) FROM openrails.ledger_transfers
		 WHERE merchant_id = $1 AND customer_id = $2 AND source = 'or892' AND source_id = $3
	`, merchantID, customer, coord.SourceID).Scan(&rows))
	require.Equal(t, 1, rows, "the database holds exactly one row for one coordinate")

	bal, err := l.Balance(ctx, balance)
	require.NoError(t, err)
	require.Equal(t, int64(4_000), bal, "the replay must not move the balance counters a second time")
}

// CONCURRENT identical spends: with the index in place, exactly one applies and
// exactly one amount leaves the balance, no matter how the two transactions
// interleave.
func TestOr892_ConcurrentIdenticalSpendsApplyExactlyOnce(t *testing.T) {
	svc, _, payer, cur, ctx := moneyInEnv(t)
	_, err := svc.Deposit(ctx, money.DepositParams{
		CustomerID: &payer, Invoker: payer.UUID().String(), Currency: cur,
		Amount: 10_000, Source: "seed", SourceID: strptr("seed-" + payer.UUID().String()),
	})
	require.NoError(t, err)

	key := money.MustIdempotencyKey(money.OpSpend, "invoke", uuid.NewString())
	const n = 4
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		applies int
		errs    []error
	)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			trx, serr := svc.SpendCredits(ctx, money.SpendParams{
				Payer: &payer, Invoker: "u", Currency: cur, Amount: 2_500, Key: key,
			})
			mu.Lock()
			defer mu.Unlock()
			if serr != nil {
				errs = append(errs, serr)
				return
			}
			if !trx.Replayed {
				applies++
			}
		}()
	}
	wg.Wait()
	require.Empty(t, errs, "a concurrent replay is a replay, never an error")
	require.Equal(t, 1, applies, "exactly one of the concurrent spends may apply")

	bal, err := svc.GetBalanceForCustomer(ctx, payer, cur)
	require.NoError(t, err)
	require.Equal(t, int64(7_500), bal.Balance, "one spend's worth of money leaves the balance")
}

// The applied-vs-replayed signal is what consumers were rebuilding claim tables
// for. Pin it on the surfaces that carry money.
func TestOr892_WritesReportAppliedVersusReplayed(t *testing.T) {
	svc, _, payer, cur, ctx := moneyInEnv(t)
	seed := "seed-" + payer.UUID().String()

	first, err := svc.Deposit(ctx, money.DepositParams{
		CustomerID: &payer, Invoker: payer.UUID().String(), Currency: cur,
		Amount: 10_000, Source: "seed", SourceID: &seed,
	})
	require.NoError(t, err)
	require.False(t, first.Replayed, "a first deposit applies")

	again, err := svc.Deposit(ctx, money.DepositParams{
		CustomerID: &payer, Invoker: payer.UUID().String(), Currency: cur,
		Amount: 10_000, Source: "seed", SourceID: &seed,
	})
	require.NoError(t, err)
	require.True(t, again.Replayed, "a re-deposit under the same key is a replay")

	spendKey := money.MustIdempotencyKey(money.OpSpend, "invoke", uuid.NewString())
	spent, err := svc.SpendCredits(ctx, money.SpendParams{
		Payer: &payer, Invoker: "u", Currency: cur, Amount: 1_000, Key: spendKey,
	})
	require.NoError(t, err)
	require.False(t, spent.Replayed)

	respent, err := svc.SpendCredits(ctx, money.SpendParams{
		Payer: &payer, Invoker: "u", Currency: cur, Amount: 1_000, Key: spendKey,
	})
	require.NoError(t, err)
	require.True(t, respent.Replayed, "the replay must say so instead of looking like a fresh charge")

	captureKey := money.MustIdempotencyKey(money.OpCapture, "admit", uuid.NewString())
	cap1, err := svc.CaptureAuthorized(ctx, money.SpendParams{
		Payer: &payer, Invoker: "u", Currency: cur, Amount: 900, Key: captureKey,
	})
	require.NoError(t, err)
	require.False(t, cap1.Replayed)

	cap2, err := svc.CaptureAuthorized(ctx, money.SpendParams{
		Payer: &payer, Invoker: "u", Currency: cur, Amount: 900, Key: captureKey,
	})
	require.NoError(t, err)
	require.True(t, cap2.Replayed)

	bal, err := svc.GetBalanceForCustomer(ctx, payer, cur)
	require.NoError(t, err)
	require.Equal(t, int64(10_000-1_000-900), bal.Balance,
		"only the applied writes moved money")
}

// The coordinate is NOT NULL end to end now: a blank half cannot reach the
// table even if a caller hand-builds the transfer past money.IdempotencyKey.
func TestOr892_TheLedgerRefusesABlankCoordinate(t *testing.T) {
	_, _, pool, payer, cur, ctx := moneyInEnvWithDB(t)
	merchantID := dbtest.TestMerchantID.UUID()
	customer := payer.UUID()
	// These drive the ledger DIRECTLY, below the money service that would
	// normally materialize the customers row, so seed it for the account FK.
	dbtest.EnsureCustomerIDPgx(ctx, t, pool, customer.String())

	l := ledger.New(gen.New(pool), merchantID)
	clearing, err := l.EnsureSystemAccount(ctx, ledger.RailClearing, cur)
	require.NoError(t, err)
	balance, err := l.EnsureCustomerBalance(ctx, customer, cur)
	require.NoError(t, err)

	for _, c := range []ledger.Coord{
		{Operation: "", Source: "s", SourceID: "x"},
		{Operation: ledger.OpDeposit, Source: "", SourceID: "x"},
		{Operation: ledger.OpDeposit, Source: "s", SourceID: ""},
	} {
		_, _, err := l.ApplyIdempotent(ctx, ledger.Transfer{
			Debit: clearing, Credit: balance, Amount: 10, Currency: cur,
			Type: ledger.Deposit, Coord: c, Customer: &customer,
		})
		require.Error(t, err, "a partial coordinate must never reach the table: %+v", c)
	}

	var blank int
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT count(*) FROM openrails.ledger_transfers
		 WHERE merchant_id = $1 AND (operation = '' OR source = '' OR source_id = '')
	`, merchantID).Scan(&blank))
	require.Zero(t, blank, "chk_ledger_transfers_coordinate_not_blank holds")
}

// A REPLAY must never re-run the spend arithmetic. Regression pin: threading
// applied-vs-replayed through SpendCredits initially dropped the early return
// on an already-committed key, so the replay re-derived fromBalance/fromOwed
// against the ALREADY-DEBITED balance — splitting one charge into a short
// balance leg plus an owed remainder, and denying a prepaid payer with
// ErrInsufficientCredits. A replay answered by a hard failure is the exact
// shape #513 decision 8 forbids, and it only shows up when the replayed amount
// exceeds the REMAINING balance.
func TestOr892_AReplayDoesNotRecomputeTheSpendAgainstTheDebitedBalance(t *testing.T) {
	svc, _, payer, cur, ctx := moneyInEnv(t)
	_, err := svc.Deposit(ctx, money.DepositParams{
		CustomerID: &payer, Invoker: payer.UUID().String(), Currency: cur,
		Amount: 500, Source: "seed", SourceID: strptr("seed-" + payer.UUID().String()),
	})
	require.NoError(t, err)

	// 300 of 500 leaves 200 — strictly LESS than the replayed amount, which is
	// what makes the recompute observable rather than silently identical.
	key := money.MustIdempotencyKey(money.OpSpend, "api", "replay-recompute")
	spend := money.SpendParams{Payer: &payer, Invoker: "u", Currency: cur, Amount: 300, Key: key}

	first, err := svc.SpendCredits(ctx, spend)
	require.NoError(t, err)
	require.False(t, first.Replayed)

	replay, err := svc.SpendCredits(ctx, spend)
	require.NoError(t, err, "a prepaid payer replaying a spend must not be denied for insufficient credits")
	require.True(t, replay.Replayed)

	bal, err := svc.GetBalanceForCustomer(ctx, payer, cur)
	require.NoError(t, err)
	require.Equal(t, int64(200), bal.Balance, "the replay moved nothing")
}
