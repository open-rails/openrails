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

	q := dbtest.Queries(pool)
	l := ledger.New(q, merchantID)
	clearing, err := l.EnsureSystemAccount(ctx, ledger.RailClearing, cur)
	require.NoError(t, err)
	clearingBefore, err := l.Balance(ctx, clearing)
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
		SELECT count(*), COALESCE(sum(amount), 0)::bigint FROM billing.ledger_transfers
		 WHERE merchant_id = $1 AND source = 'or892-raw' AND source_id = $2
	`, merchantID, sourceID).Scan(&rows, &moved))
	require.Equal(t, 1, rows, "one coordinate, one row — enforced by the index alone")
	require.Equal(t, int64(5_000), moved)
	posted, err := l.Balance(ctx, balance)
	require.NoError(t, err)
	require.Equal(t, moved, posted, "a discarded insert must not increment account counters")
	posted, err = l.Balance(ctx, clearing)
	require.NoError(t, err)
	require.Equal(t, clearingBefore-moved, posted, "a discarded insert must not debit shared clearing again")
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
	const n = 6
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		applies int
		errs    []error
	)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
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
	close(start)
	wg.Wait()
	require.Empty(t, errs, "a concurrent replay is a replay, never an error")
	require.Equal(t, 1, applies, "exactly one of the concurrent spends may apply")

	bal, err := svc.GetBalanceForCustomer(ctx, payer, cur)
	require.NoError(t, err)
	require.Equal(t, int64(7_500), bal.Balance, "one spend's worth of money leaves the balance")
}

// Each money entrypoint rejects changed financial terms, then still replays the
// accepted write. One funded payer makes every intermediate balance observable.
func TestMoneyWritesPreserveAcceptedAmountAndReplay(t *testing.T) {
	svc, _, payer, cur, ctx := moneyInEnv(t)
	_, err := svc.Deposit(ctx, money.DepositParams{
		CustomerID: &payer, Invoker: payer.UUID().String(), Currency: cur,
		Amount: 10_000, Source: "seed", SourceID: strptr("seed-" + payer.UUID().String()),
	})
	require.NoError(t, err)

	spend := money.SpendParams{Payer: &payer, Invoker: "u", Currency: cur, Amount: 1_000,
		Key: money.MustIdempotencyKey(money.OpSpend, "invoke", uuid.NewString())}
	spent, err := svc.SpendCredits(ctx, spend)
	require.NoError(t, err)
	require.False(t, spent.Replayed)
	spend.Amount = 4_000
	_, err = svc.SpendCredits(ctx, spend)
	require.ErrorIs(t, err, money.ErrIdempotencyKeyReused)
	var conflict *money.IdempotencyConflict
	require.ErrorAs(t, err, &conflict)
	require.Equal(t, int64(1_000), conflict.Committed)
	require.Equal(t, int64(4_000), conflict.Retried)
	require.Equal(t, "amount", conflict.Field)
	bal, err := svc.GetBalanceForCustomer(ctx, payer, cur)
	require.NoError(t, err)
	require.Equal(t, int64(9_000), bal.Balance, "a rejected spend moves nothing")
	spend.Amount = 1_000
	respent, err := svc.SpendCredits(ctx, spend)
	require.NoError(t, err)
	require.True(t, respent.Replayed)
	bal, err = svc.GetBalanceForCustomer(ctx, payer, cur)
	require.NoError(t, err)
	require.Equal(t, int64(9_000), bal.Balance)

	capture := money.SpendParams{Payer: &payer, Invoker: "u", Currency: cur, Amount: 400,
		Key: money.MustIdempotencyKey(money.OpCapture, "admit", uuid.NewString())}
	captured, err := svc.CaptureAuthorized(ctx, capture)
	require.NoError(t, err)
	require.False(t, captured.Replayed)
	capture.Amount = 4_000
	_, err = svc.CaptureAuthorized(ctx, capture)
	require.ErrorIs(t, err, money.ErrIdempotencyKeyReused)
	bal, err = svc.GetBalanceForCustomer(ctx, payer, cur)
	require.NoError(t, err)
	require.Equal(t, int64(8_600), bal.Balance, "a rejected capture moves nothing")
	capture.Amount = 400
	recaptured, err := svc.CaptureAuthorized(ctx, capture)
	require.NoError(t, err)
	require.NotNil(t, recaptured)
	require.True(t, recaptured.Replayed)
	bal, err = svc.GetBalanceForCustomer(ctx, payer, cur)
	require.NoError(t, err)
	require.Equal(t, int64(8_600), bal.Balance)

	withdraw := money.WithdrawParams{CustomerID: &payer, Invoker: "u", Currency: cur,
		Amount: 1_000, Source: "payout", SourceID: new(uuid.New())}
	withdrawn, err := svc.Withdraw(ctx, withdraw)
	require.NoError(t, err)
	require.False(t, withdrawn.Replayed)
	withdraw.Amount = 2_500
	_, err = svc.Withdraw(ctx, withdraw)
	require.ErrorIs(t, err, money.ErrIdempotencyKeyReused)
	bal, err = svc.GetBalanceForCustomer(ctx, payer, cur)
	require.NoError(t, err)
	require.Equal(t, int64(7_600), bal.Balance, "a rejected withdraw moves nothing")
	withdraw.Amount = 1_000
	rewithdrawn, err := svc.Withdraw(ctx, withdraw)
	require.NoError(t, err)
	require.True(t, rewithdrawn.Replayed)
	bal, err = svc.GetBalanceForCustomer(ctx, payer, cur)
	require.NoError(t, err)
	require.Equal(t, int64(7_600), bal.Balance)

	usage := money.RecordUsageParams{Payer: &payer, Invoker: "u", Currency: cur,
		EventType: "invoke", Amount: 700, Key: money.MustIdempotencyKey(money.UsageOperation("invoke"), "invoke", uuid.NewString())}
	recorded, err := svc.RecordUsage(ctx, usage)
	require.NoError(t, err)
	require.False(t, recorded.Replayed)
	usage.Amount = 7_000
	_, err = svc.RecordUsage(ctx, usage)
	require.ErrorIs(t, err, money.ErrIdempotencyKeyReused)
	bal, err = svc.GetBalanceForCustomer(ctx, payer, cur)
	require.NoError(t, err)
	require.Equal(t, int64(6_900), bal.Balance, "a rejected usage moves nothing")
	usage.Amount = 700
	rerecorded, err := svc.RecordUsage(ctx, usage)
	require.NoError(t, err)
	require.True(t, rerecorded.Replayed)
	bal, err = svc.GetBalanceForCustomer(ctx, payer, cur)
	require.NoError(t, err)
	require.Equal(t, int64(6_900), bal.Balance, "only the four accepted writes moved money")
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

	l := ledger.New(dbtest.Queries(pool), merchantID)
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
		SELECT count(*) FROM billing.ledger_transfers
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

	for range 2 {
		replay, err := svc.SpendCredits(ctx, spend)
		require.NoError(t, err, "a prepaid payer replaying a spend must not be denied for insufficient credits")
		require.True(t, replay.Replayed)
		require.Equal(t, first.ID, replay.ID)
	}

	bal, err := svc.GetBalanceForCustomer(ctx, payer, cur)
	require.NoError(t, err)
	require.Equal(t, int64(200), bal.Balance, "the replay moved nothing")
}
