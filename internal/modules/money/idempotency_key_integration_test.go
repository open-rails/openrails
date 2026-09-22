//go:build integration

package money_test

import (
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
		`SELECT count(*) FROM billing.invoice_items WHERE merchant_id = $1 AND customer_id = $2`,
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
		`SELECT count(*) FROM billing.invoice_items WHERE merchant_id = $1 AND customer_id = $2`,
		dbtest.TestMerchantID.UUID(), payer.UUID()).Scan(&items))
	require.Equal(t, 0, items, "no minted-key invoice items")
}
