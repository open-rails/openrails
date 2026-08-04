//go:build integration

package money_test

import (
	"testing"
	"time"

	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/stretchr/testify/require"
)

// #302: a hold placed against an arrears credit line must capture even when the
// actual exceeds the prepaid balance — drawing balance first, then spilling to
// owed (previously this errored in withdrawBalanceAndBlocks).
func TestCaptureHold_ArrearsSpillsToOwed(t *testing.T) {
	svc, _, payer, cur, ctx := moneyInEnv(t)
	bm := money.BillingModeArrears
	_, err := svc.UpsertAccountSettings(ctx, payer, money.DefaultCurrency, money.AccountSettingsInput{BillingMode: &bm})
	require.NoError(t, err)
	require.NoError(t, svc.SetCreditLimit(ctx, payer, money.DefaultCurrency, 1000))
	_, err = svc.Deposit(ctx, money.DepositParams{CustomerID: &payer, Invoker: payer.UUID().String(), Currency: money.DefaultCurrency, Amount: 300, Source: "seed"})
	require.NoError(t, err)

	_, err = svc.AuthorizeAndHold(ctx, money.AuthorizeHoldInput{
		Payer:           payer,
		Invoker:         "user:a",
		Currency:        money.DefaultCurrency,
		EstimatedAmount: 800,
		Key:             money.MustIdempotencyKey(money.OpCapture, "req", "h1"),
		ExpiresAt:       time.Now().Add(time.Hour),
	})
	require.NoError(t, err)

	_, err = svc.CaptureAuthorized(ctx, money.SpendParams{
		Payer:    &payer,
		Invoker:  "user:a",
		Currency: money.DefaultCurrency,
		Amount:   800,
		Key:      money.MustIdempotencyKey(money.OpCapture, "req", "h1"),
	})
	require.NoError(t, err, "arrears capture past balance must not error")

	bal, _ := svc.GetBalanceForCustomer(ctx, payer, cur)
	require.Equal(t, int64(0), bal.Balance, "balance drawn first")
	owed, _ := svc.GetOutstandingOwed(ctx, payer, cur)
	require.Equal(t, int64(500), owed, "remainder (800-300) spilled to owed")
}
