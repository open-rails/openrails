//go:build integration

package money_test

import (
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/modules/money"
)

// or#891 — every claim below is INDUCED against the real ledger, never read off
// the code. Each test that exercises the owed leg installs a REAL credit line
// first: without one the arrears branch is unreachable and "no money moved" is
// true only because no money could move.

// --- item 1: the key is required; no empty-key path remains -------------------

func TestOr891_SpendCreditsRefusesAnEmptyKey(t *testing.T) {
	svc, _, payer, cur, ctx := moneyInEnv(t)
	_, err := svc.Deposit(ctx, money.DepositParams{
		CustomerID: &payer, Invoker: payer.UUID().String(), Currency: cur,
		Amount: 10_000, Source: "seed", SourceID: strptr("seed-" + payer.UUID().String()),
	})
	require.NoError(t, err)

	// or#892: a blank half is no longer representable — the constructor is the
	// only way to build a key and it refuses one, so an unkeyed spend cannot be
	// expressed at all, let alone posted.
	_, kerr := money.NewIdempotencyKey(money.OpSpend, "s", "")
	require.ErrorContains(t, kerr, "source and source_id required")
	_, kerr = money.NewIdempotencyKey(money.OpSpend, "", "k")
	require.ErrorContains(t, kerr, "source and source_id required")

	// The zero key — the only unkeyed value a caller can still hand in — is
	// refused at the entrypoint, and moves nothing.
	_, err = svc.SpendCredits(ctx, money.SpendParams{
		Payer: &payer, Invoker: "u", Currency: cur, Amount: 1_000,
	})
	require.ErrorContains(t, err, "idempotency key required")

	bal, berr := svc.GetBalanceForCustomer(ctx, payer, cur)
	require.NoError(t, berr)
	require.Equal(t, int64(10_000), bal.Balance, "a refused keyless spend must not debit")
}

func TestOr891_WithdrawRefusesAnEmptyKey(t *testing.T) {
	svc, _, payer, cur, ctx := moneyInEnv(t)
	_, err := svc.Deposit(ctx, money.DepositParams{
		CustomerID: &payer, Invoker: payer.UUID().String(), Currency: cur,
		Amount: 10_000, Source: "seed", SourceID: strptr("seed-" + payer.UUID().String()),
	})
	require.NoError(t, err)

	_, err = svc.Withdraw(ctx, money.WithdrawParams{
		CustomerID: &payer, Invoker: "u", Currency: cur, Amount: 1_000, Source: "s",
	})
	require.ErrorContains(t, err, "source_id required")

	bal, berr := svc.GetBalanceForCustomer(ctx, payer, cur)
	require.NoError(t, berr)
	require.Equal(t, int64(10_000), bal.Balance, "a refused keyless withdraw must not debit")
}

// A keyed spend replayed with the SAME body is a no-op — the property the empty
// key silently forfeited.
func TestOr891_KeyedSpendReplayMovesNoMoney(t *testing.T) {
	svc, _, payer, cur, ctx := moneyInEnv(t)
	_, err := svc.Deposit(ctx, money.DepositParams{
		CustomerID: &payer, Invoker: payer.UUID().String(), Currency: cur,
		Amount: 10_000, Source: "seed", SourceID: strptr("seed-" + payer.UUID().String()),
	})
	require.NoError(t, err)

	key := uuid.NewString()
	for i := 0; i < 3; i++ {
		require.NoError(t, spendErr(svc.SpendCredits(ctx, money.SpendParams{
			Payer: &payer, Invoker: "u", Currency: cur, Amount: 1_000, Key: money.MustIdempotencyKey(money.OpSpend, "invoke", key),
		})))
	}
	bal, berr := svc.GetBalanceForCustomer(ctx, payer, cur)
	require.NoError(t, berr)
	require.Equal(t, int64(9_000), bal.Balance, "three identical keyed spends debit once")
}

// --- item 2: the balance leg has a structural key ----------------------------

// The Go-side dedupe is a SELECT-then-INSERT under lockBalance. This drives it
// concurrently from independent connections: exactly one debit must land, and
// the loser must not be a silent second charge.
func TestOr891_ConcurrentKeyedSpendsDebitOnce(t *testing.T) {
	svc, _, payer, cur, ctx := moneyInEnv(t)
	_, err := svc.Deposit(ctx, money.DepositParams{
		CustomerID: &payer, Invoker: payer.UUID().String(), Currency: cur,
		Amount: 10_000, Source: "seed", SourceID: strptr("seed-" + payer.UUID().String()),
	})
	require.NoError(t, err)

	key := uuid.NewString()
	const racers = 6
	errs := make([]error, racers)
	var wg sync.WaitGroup
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = svc.SpendCredits(ctx, money.SpendParams{
				Payer: &payer, Invoker: "u", Currency: cur, Amount: 1_500, Key: money.MustIdempotencyKey(money.OpSpend, "invoke", key),
			})
		}(i)
	}
	wg.Wait()
	for i, e := range errs {
		require.NoError(t, e, "racer %d", i)
	}
	bal, berr := svc.GetBalanceForCustomer(ctx, payer, cur)
	require.NoError(t, berr)
	require.Equal(t, int64(8_500), bal.Balance, "%d concurrent identical spends debit exactly once", racers)
}

// --- item 3: a reused key with a changed body is refused ---------------------

func TestOr891_SpendReusedKeyChangedAmountIsRefused(t *testing.T) {
	svc, _, payer, cur, ctx := moneyInEnv(t)
	_, err := svc.Deposit(ctx, money.DepositParams{
		CustomerID: &payer, Invoker: payer.UUID().String(), Currency: cur,
		Amount: 10_000, Source: "seed", SourceID: strptr("seed-" + payer.UUID().String()),
	})
	require.NoError(t, err)

	key := uuid.NewString()
	require.NoError(t, spendErr(svc.SpendCredits(ctx, money.SpendParams{
		Payer: &payer, Invoker: "u", Currency: cur, Amount: 1_000, Key: money.MustIdempotencyKey(money.OpSpend, "invoke", key),
	})))

	_, err = svc.SpendCredits(ctx, money.SpendParams{
		Payer: &payer, Invoker: "u", Currency: cur, Amount: 4_000, Key: money.MustIdempotencyKey(money.OpSpend, "invoke", key),
	})
	require.ErrorIs(t, err, money.ErrIdempotencyKeyReused,
		"a corrected amount under a reused key must be refused, not answered with the first charge")

	var conflict *money.IdempotencyConflict
	require.ErrorAs(t, err, &conflict)
	require.Equal(t, int64(1_000), conflict.Committed)
	require.Equal(t, int64(4_000), conflict.Retried)
	require.Equal(t, "amount", conflict.Field)

	bal, berr := svc.GetBalanceForCustomer(ctx, payer, cur)
	require.NoError(t, berr)
	require.Equal(t, int64(9_000), bal.Balance, "the refused retry must not move money either way")
}

func TestOr891_CaptureReusedKeyChangedAmountIsRefused(t *testing.T) {
	svc, _, payer, cur, ctx := moneyInEnv(t)
	_, err := svc.Deposit(ctx, money.DepositParams{
		CustomerID: &payer, Invoker: payer.UUID().String(), Currency: cur,
		Amount: 10_000, Source: "seed", SourceID: strptr("seed-" + payer.UUID().String()),
	})
	require.NoError(t, err)

	key := uuid.NewString()
	_, err = svc.CaptureAuthorized(ctx, money.SpendParams{
		Payer: &payer, Invoker: "u", Currency: cur, Amount: 400, Key: money.MustIdempotencyKey(money.OpCapture, "admit", key),
	})
	require.NoError(t, err)

	// Same coordinates, corrected amount.
	_, err = svc.CaptureAuthorized(ctx, money.SpendParams{
		Payer: &payer, Invoker: "u", Currency: cur, Amount: 4_000, Key: money.MustIdempotencyKey(money.OpCapture, "admit", key),
	})
	require.ErrorIs(t, err, money.ErrIdempotencyKeyReused)

	// Same coordinates, same amount: still an idempotent replay.
	trx, err := svc.CaptureAuthorized(ctx, money.SpendParams{
		Payer: &payer, Invoker: "u", Currency: cur, Amount: 400, Key: money.MustIdempotencyKey(money.OpCapture, "admit", key),
	})
	require.NoError(t, err)
	require.NotNil(t, trx)

	bal, berr := svc.GetBalanceForCustomer(ctx, payer, cur)
	require.NoError(t, berr)
	require.Equal(t, int64(9_600), bal.Balance)
}

func TestOr891_WithdrawReusedKeyChangedAmountIsRefused(t *testing.T) {
	svc, _, payer, cur, ctx := moneyInEnv(t)
	_, err := svc.Deposit(ctx, money.DepositParams{
		CustomerID: &payer, Invoker: payer.UUID().String(), Currency: cur,
		Amount: 10_000, Source: "seed", SourceID: strptr("seed-" + payer.UUID().String()),
	})
	require.NoError(t, err)

	key := uuid.New()
	_, err = svc.Withdraw(ctx, money.WithdrawParams{
		CustomerID: &payer, Invoker: "u", Currency: cur, Amount: 1_000, Source: "payout", SourceID: &key,
	})
	require.NoError(t, err)

	_, err = svc.Withdraw(ctx, money.WithdrawParams{
		CustomerID: &payer, Invoker: "u", Currency: cur, Amount: 2_500, Source: "payout", SourceID: &key,
	})
	require.ErrorIs(t, err, money.ErrIdempotencyKeyReused)

	bal, berr := svc.GetBalanceForCustomer(ctx, payer, cur)
	require.NoError(t, berr)
	require.Equal(t, int64(9_000), bal.Balance)
}

func TestOr891_RecordUsageReusedKeyChangedAmountIsRefused(t *testing.T) {
	svc, _, payer, cur, ctx := moneyInEnv(t)
	_, err := svc.Deposit(ctx, money.DepositParams{
		CustomerID: &payer, Invoker: payer.UUID().String(), Currency: cur,
		Amount: 10_000, Source: "seed", SourceID: strptr("seed-" + payer.UUID().String()),
	})
	require.NoError(t, err)

	key := uuid.NewString()
	_, err = svc.RecordUsage(ctx, money.RecordUsageParams{
		Payer: &payer, Invoker: "u", Currency: cur, EventType: "invoke",
		Amount: 700, Key: money.MustIdempotencyKey(money.UsageOperation("invoke"), "invoke", key),
	})
	require.NoError(t, err)

	_, err = svc.RecordUsage(ctx, money.RecordUsageParams{
		Payer: &payer, Invoker: "u", Currency: cur, EventType: "invoke",
		Amount: 7_000, Key: money.MustIdempotencyKey(money.UsageOperation("invoke"), "invoke", key),
	})
	require.ErrorIs(t, err, money.ErrIdempotencyKeyReused)

	bal, berr := svc.GetBalanceForCustomer(ctx, payer, cur)
	require.NoError(t, berr)
	require.Equal(t, int64(9_300), bal.Balance)
}

// --- item 4: no minted key behind uq_invoice_items_source --------------------

// With a REAL credit line installed the owed leg is reachable, so the pending
// invoice item is actually written. Every replay used to mint a fresh uuidv7
// for that item's key, defeating uq_invoice_items_source by construction and
// accruing a NEW item each time. The key is now the caller's, so replays
// collapse onto one item.
func TestOr891_OwedLegAccruesOneInvoiceItemPerKey(t *testing.T) {
	svc, pool, payer, cur, ctx := moneyInEnv(t)
	_, err := svc.UpsertAccountSettings(ctx, payer, cur, money.AccountSettingsInput{
		BillingMode: strptr(money.BillingModeArrears),
	})
	require.NoError(t, err)
	require.NoError(t, svc.SetCreditLimit(ctx, payer, cur, 10_000))
	_, err = svc.Deposit(ctx, money.DepositParams{
		CustomerID: &payer, Invoker: payer.UUID().String(), Currency: cur,
		Amount: 1_000, Source: "seed", SourceID: strptr("seed-" + payer.UUID().String()),
	})
	require.NoError(t, err)

	key := uuid.NewString()
	// 1000 from balance, 2000 to owed.
	require.NoError(t, spendErr(svc.SpendCredits(ctx, money.SpendParams{
		Payer: &payer, Invoker: "u", Currency: cur, Amount: 3_000, Key: money.MustIdempotencyKey(money.OpSpend, "invoke", key),
	})))
	require.NoError(t, spendErr(svc.SpendCredits(ctx, money.SpendParams{
		Payer: &payer, Invoker: "u", Currency: cur, Amount: 3_000, Key: money.MustIdempotencyKey(money.OpSpend, "invoke", key),
	})))
	require.NoError(t, spendErr(svc.SpendCredits(ctx, money.SpendParams{
		Payer: &payer, Invoker: "u", Currency: cur, Amount: 3_000, Key: money.MustIdempotencyKey(money.OpSpend, "invoke", key),
	})))

	owed, oerr := svc.GetOutstandingOwed(ctx, payer, cur)
	require.NoError(t, oerr)
	require.Equal(t, int64(2_000), owed, "three replays accrue owed once")

	var items int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM openrails.invoice_items WHERE merchant_id = $1 AND customer_id = $2`,
		dbtest.TestMerchantID.UUID(), payer.UUID()).Scan(&items))
	require.Equal(t, 1, items, "one pending invoice item per key, not one per replay")
}

// A keyless spend can no longer reach the invoice-item write at all.
func TestOr891_KeylessSpendCannotReachTheOwedLeg(t *testing.T) {
	svc, pool, payer, cur, ctx := moneyInEnv(t)
	_, err := svc.UpsertAccountSettings(ctx, payer, cur, money.AccountSettingsInput{
		BillingMode: strptr(money.BillingModeArrears),
	})
	require.NoError(t, err)
	require.NoError(t, svc.SetCreditLimit(ctx, payer, cur, 10_000))

	// or#891 item 4: a blank key used to reach spendBalanceThenOwedTx and be
	// papered over with a freshly minted uuidv7, so every replay accrued a NEW
	// invoice item past uq_invoice_items_source. or#892 makes the blank key
	// unconstructable; the zero key is what a caller can still hand in, and it
	// is refused before any leg posts.
	_, kerr := money.NewIdempotencyKey(money.OpSpend, "invoke", "")
	require.ErrorContains(t, kerr, "source and source_id required")
	for i := 0; i < 3; i++ {
		_, err = svc.SpendCredits(ctx, money.SpendParams{
			Payer: &payer, Invoker: "u", Currency: cur, Amount: 3_000,
		})
		require.ErrorContains(t, err, "idempotency key required")
	}
	var items int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM openrails.invoice_items WHERE merchant_id = $1 AND customer_id = $2`,
		dbtest.TestMerchantID.UUID(), payer.UUID()).Scan(&items))
	require.Equal(t, 0, items, "no minted-key invoice items")
}
