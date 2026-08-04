//go:build integration

package money_test

import (
	"testing"
	"time"

	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/stretchr/testify/require"
)

// #676: a lapsed (unswept) credit lot inflates the raw ledger balance but is not
// drawable by CreditSpend — capture must not hard-fail ErrInsufficientCredits
// until the hourly expiry sweep runs. The balance leg is capped at the
// spendable-lot total; the shortfall spills to owed (capture never re-gates).
func TestCaptureAuthorized_LapsedLotNeverBlocksCapture(t *testing.T) {
	svc, _, _, payer, cur, ctx := moneyInEnvWithDB(t)
	fc := clockwork.NewFakeClockAt(time.Now())
	svc.SetClock(fc)

	// Lot A: 300, expires in 1h. Lot B: 200, no expiry.
	expA := fc.Now().Add(time.Hour)
	_, err := svc.Deposit(ctx, money.DepositParams{CustomerID: &payer, Invoker: payer.UUID().String(), Currency: cur, Amount: 300, Source: "seed-a", ExpiresAt: &expA})
	require.NoError(t, err)
	_, err = svc.Deposit(ctx, money.DepositParams{CustomerID: &payer, Invoker: payer.UUID().String(), Currency: cur, Amount: 200, Source: "seed-b"})
	require.NoError(t, err)

	// Lot A lapses but the expiry sweep has NOT run: raw balance still 500,
	// spendable only 200.
	fc.Advance(2 * time.Hour)

	trx, err := svc.CaptureAuthorized(ctx, money.SpendParams{
		Payer:    &payer,
		Invoker:  "user:a",
		Currency: cur,
		Amount:   300,
		Key:      money.MustIdempotencyKey(money.OpCapture, "req", "lapsed-1"),
	})
	require.NoError(t, err, "capture must not fail on an unswept lapsed lot")
	require.NotNil(t, trx)

	// 200 drawn from the spendable lot, 100 spilled to owed.
	owed, err := svc.GetOutstandingOwed(ctx, payer, cur)
	require.NoError(t, err)
	require.Equal(t, int64(100), owed, "shortfall past spendable lots spills to owed")

	// Idempotent retry returns the same durable spend, no double debit.
	_, err = svc.CaptureAuthorized(ctx, money.SpendParams{
		Payer: &payer, Invoker: "user:a", Currency: cur, Amount: 300, Key: money.MustIdempotencyKey(money.OpCapture, "req", "lapsed-1"),
	})
	require.NoError(t, err)
	owed2, err := svc.GetOutstandingOwed(ctx, payer, cur)
	require.NoError(t, err)
	require.Equal(t, owed, owed2)
}
