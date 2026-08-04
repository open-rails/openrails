//go:build integration

package money_test

import (
	"testing"

	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/stretchr/testify/require"
)

// #513 decision 8: the admit/serve/capture split can bounded-over-admit under
// concurrency, because durable capture RECORDS REALITY UNCONDITIONALLY and is
// the system of record — it is NOT a second gate. CaptureAuthorized must
// therefore draw the prepaid balance first and record any remainder as
// owed/overdraft, even for a prepaid payer with no credit line, instead of
// returning ErrInsufficientCredits ("served but can't charge").
func TestCaptureAuthorized_PrepaidOverAdmitRecordsOverdraft(t *testing.T) {
	svc, _, payer, cur, ctx := moneyInEnv(t)
	// Prepaid payer (no settings row, no credit line), balance 100.
	_, err := svc.Deposit(ctx, money.DepositParams{
		CustomerID: &payer, Invoker: payer.UUID().String(),
		Currency: money.DefaultCurrency, Amount: 100, Source: "seed",
	})
	require.NoError(t, err)

	// Over-admit: the gate let through 150 against 100 of spendable balance. Capture
	// must record the truth, not reject.
	trx, err := svc.CaptureAuthorized(ctx, money.SpendParams{
		Payer:    &payer,
		Invoker:  "user:a",
		Currency: money.DefaultCurrency,
		Amount:   150,
		Key:      money.MustIdempotencyKey(money.OpCapture, "req", "over-admit-1"),
	})
	require.NoError(t, err, "pre-authorized capture must not re-gate the credit line")
	require.NotErrorIs(t, err, money.ErrInsufficientCredits)
	require.NotNil(t, trx, "capture returns the durable money transaction")

	// Prepaid balance drawn to 0, remainder recorded as owed (involuntary overdraft).
	bal, _ := svc.GetBalanceForCustomer(ctx, payer, cur)
	require.Equal(t, int64(0), bal.Balance, "prepaid balance drawn first")
	owed, _ := svc.GetOutstandingOwed(ctx, payer, cur)
	require.Equal(t, int64(50), owed, "remainder (150-100) recorded as owed/overdraft")
}

// Idempotency survives the non-gating path: replaying the same capture
// coordinates is a no-op that returns the original transaction and never
// double-charges.
func TestCaptureAuthorized_OverAdmitIsIdempotent(t *testing.T) {
	svc, _, payer, cur, ctx := moneyInEnv(t)
	_, err := svc.Deposit(ctx, money.DepositParams{
		CustomerID: &payer, Invoker: payer.UUID().String(),
		Currency: money.DefaultCurrency, Amount: 100, Source: "seed",
	})
	require.NoError(t, err)

	p := money.SpendParams{
		Payer: &payer, Invoker: "user:a", Currency: money.DefaultCurrency,
		Amount: 150, Key: money.MustIdempotencyKey(money.OpCapture, "req", "over-admit-dup"),
	}
	first, err := svc.CaptureAuthorized(ctx, p)
	require.NoError(t, err)
	require.NotNil(t, first)

	second, err := svc.CaptureAuthorized(ctx, p) // replay
	require.NoError(t, err)
	require.NotNil(t, second)
	require.Equal(t, first.ID, second.ID, "replay returns the same durable transaction")

	bal, _ := svc.GetBalanceForCustomer(ctx, payer, cur)
	require.Equal(t, int64(0), bal.Balance, "replay must not double-spend")
	owed, _ := svc.GetOutstandingOwed(ctx, payer, cur)
	require.Equal(t, int64(50), owed, "replay must not double-accrue owed")
}

// Arrears variant: a pre-authorized capture that exceeds BOTH the balance and
// the admin-set credit line still captures (records reality) instead of being
// rejected for breaching the line — that gate lives only at admit time (#513).
func TestCaptureAuthorized_ArrearsOverLineRecordsOverdraft(t *testing.T) {
	svc, _, payer, cur, ctx := moneyInEnv(t)
	bm := money.BillingModeArrears
	_, err := svc.UpsertAccountSettings(ctx, payer, money.DefaultCurrency, money.AccountSettingsInput{BillingMode: &bm})
	require.NoError(t, err)
	require.NoError(t, svc.SetCreditLimit(ctx, payer, money.DefaultCurrency, 200))
	_, err = svc.Deposit(ctx, money.DepositParams{
		CustomerID: &payer, Invoker: payer.UUID().String(),
		Currency: money.DefaultCurrency, Amount: 100, Source: "seed",
	})
	require.NoError(t, err)

	// 500 > balance(100) + credit line(200). The immediate SpendCredits path would
	// reject; the pre-authorized capture path records the full charge.
	trx, err := svc.CaptureAuthorized(ctx, money.SpendParams{
		Payer:    &payer,
		Invoker:  "user:a",
		Currency: money.DefaultCurrency,
		Amount:   500,
		Key:      money.MustIdempotencyKey(money.OpCapture, "req", "arrears-over-line"),
	})
	require.NoError(t, err, "pre-authorized capture must not re-gate the credit line")
	require.NotNil(t, trx)

	bal, _ := svc.GetBalanceForCustomer(ctx, payer, cur)
	require.Equal(t, int64(0), bal.Balance, "balance drawn first")
	owed, _ := svc.GetOutstandingOwed(ctx, payer, cur)
	require.Equal(t, int64(400), owed, "remainder (500-100) recorded as owed, past the 200 line")
}

// Guard: the IMMEDIATE spend path (SpendCredits) must STILL re-gate — only the
// capture-of-an-authorized-hold path became non-gating.
func TestSpendCredits_PrepaidStillGatesAfterFix(t *testing.T) {
	svc, _, payer, cur, ctx := moneyInEnv(t)
	_, err := svc.Deposit(ctx, money.DepositParams{
		CustomerID: &payer, Invoker: payer.UUID().String(),
		Currency: money.DefaultCurrency, Amount: 100, Source: "seed",
	})
	require.NoError(t, err)

	err = svc.SpendCredits(ctx, money.SpendParams{
		Payer: &payer, Invoker: "u", Currency: money.DefaultCurrency,
		Amount: 150, Key: money.MustIdempotencyKey(money.OpSpend, "s", "x1"),
	})
	require.ErrorIs(t, err, money.ErrInsufficientCredits, "immediate prepaid spend must still floor at balance")
	bal, _ := svc.GetBalanceForCustomer(ctx, payer, cur)
	require.Equal(t, int64(100), bal.Balance, "denied immediate spend must not debit")
}
