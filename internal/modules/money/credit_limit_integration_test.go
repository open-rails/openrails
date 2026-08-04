//go:build integration

package money_test

import (
	"testing"
	"time"

	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/stretchr/testify/require"
)

// #489: under arrears with an admin-set credit line, AuthorizeAndHold allows the
// balance to go NEGATIVE up to credit_limit_amount and denies insufficient_credit
// when a new hold would exceed it.
func TestAuthorizeAndHold_ArrearsCreditLine(t *testing.T) {
	svc, _, payer, _, ctx := moneyInEnv(t)
	_, err := svc.UpsertAccountSettings(ctx, payer, money.DefaultCurrency, money.AccountSettingsInput{BillingMode: strptr(money.BillingModeArrears)})
	require.NoError(t, err)
	// Operator-set credit line of 1000 (NOT via self-serve settings).
	require.NoError(t, svc.SetCreditLimit(ctx, payer, money.DefaultCurrency, 1000))

	got, err := svc.GetCreditLimit(ctx, payer, money.DefaultCurrency)
	require.NoError(t, err)
	require.Equal(t, int64(1000), got)

	// Balance 0, credit line 1000. An estimate up to the line is allowed.
	res, err := svc.AuthorizeAndHold(ctx, money.AuthorizeHoldInput{
		Payer: payer, Invoker: "u", Currency: money.DefaultCurrency, EstimatedAmount: 1000,
		Key: money.MustIdempotencyKey(money.OpCapture, "usage", "h1"), ExpiresAt: time.Now().Add(time.Hour),
	})
	require.NoError(t, err)
	require.True(t, res.Decision.Allowed)
	require.Equal(t, int64(1000), res.AccountCapacityAmount)

	// Above the line is denied. Active Redis hold subtraction is tracked by #505.
	res, err = svc.AuthorizeAndHold(ctx, money.AuthorizeHoldInput{
		Payer: payer, Invoker: "u", Currency: money.DefaultCurrency, EstimatedAmount: 1001,
		Key: money.MustIdempotencyKey(money.OpCapture, "usage", "h2"), ExpiresAt: time.Now().Add(time.Hour),
	})
	require.NoError(t, err)
	require.False(t, res.Decision.Allowed)
	require.Equal(t, money.DenyInsufficientCredit, res.Decision.DenyCode)
}

func TestAuthorizeAndHold_ArrearsCreditLineSubtractsOwedExposure(t *testing.T) {
	svc, _, payer, _, ctx := moneyInEnv(t)
	_, err := svc.UpsertAccountSettings(ctx, payer, money.DefaultCurrency, money.AccountSettingsInput{BillingMode: strptr(money.BillingModeArrears)})
	require.NoError(t, err)
	require.NoError(t, svc.SetCreditLimit(ctx, payer, money.DefaultCurrency, 1000))
	_, err = svc.AccrueOwed(ctx, payer, money.DefaultCurrency, "usage", "owed-before-hold", 900)
	require.NoError(t, err)

	res, err := svc.AuthorizeAndHold(ctx, money.AuthorizeHoldInput{
		Payer: payer, Invoker: "u", Currency: money.DefaultCurrency, EstimatedAmount: 200,
		Key: money.MustIdempotencyKey(money.OpCapture, "usage", "over-line"), ExpiresAt: time.Now().Add(time.Hour),
	})
	require.NoError(t, err)
	require.False(t, res.Decision.Allowed)
	require.Equal(t, money.DenyInsufficientCredit, res.Decision.DenyCode)

	res, err = svc.AuthorizeAndHold(ctx, money.AuthorizeHoldInput{
		Payer: payer, Invoker: "u", Currency: money.DefaultCurrency, EstimatedAmount: 100,
		Key: money.MustIdempotencyKey(money.OpCapture, "usage", "at-line"), ExpiresAt: time.Now().Add(time.Hour),
	})
	require.NoError(t, err)
	require.True(t, res.Decision.Allowed)
	require.Equal(t, int64(100), res.AccountCapacityAmount)
}

// #506: credit_limit=0 means no arrears capacity. Prepaid balance can still
// admit work, but a hold beyond prepaid availability is denied.
func TestAuthorizeAndHold_ArrearsZeroCreditLinePrepaidOnly(t *testing.T) {
	svc, _, payer, _, ctx := moneyInEnv(t)
	_, err := svc.UpsertAccountSettings(ctx, payer, money.DefaultCurrency, money.AccountSettingsInput{BillingMode: strptr(money.BillingModeArrears)})
	require.NoError(t, err)
	_, err = svc.Deposit(ctx, money.DepositParams{CustomerID: &payer, Invoker: payer.UUID().String(), Currency: money.DefaultCurrency, Amount: 500, Source: "seed"})
	require.NoError(t, err)

	res, err := svc.AuthorizeAndHold(ctx, money.AuthorizeHoldInput{
		Payer: payer, Invoker: "u", Currency: money.DefaultCurrency, EstimatedAmount: 200,
		Key: money.MustIdempotencyKey(money.OpCapture, "usage", "prepaid-ok"), ExpiresAt: time.Now().Add(time.Hour),
	})
	require.NoError(t, err)
	require.True(t, res.Decision.Allowed)
	require.Equal(t, int64(500), res.AccountCapacityAmount)

	res, err = svc.AuthorizeAndHold(ctx, money.AuthorizeHoldInput{
		Payer: payer, Invoker: "u", Currency: money.DefaultCurrency, EstimatedAmount: 600,
		Key: money.MustIdempotencyKey(money.OpCapture, "usage", "prepaid-over"), ExpiresAt: time.Now().Add(time.Hour),
	})
	require.NoError(t, err)
	require.False(t, res.Decision.Allowed)
	require.Equal(t, money.DenyInsufficientCredit, res.Decision.DenyCode)
}

// #489: a PREPAID account is unaffected by the credit-line change — a hold beyond
// available balance is denied insufficient_balance.
func TestAuthorizeAndHold_PrepaidUnaffected(t *testing.T) {
	svc, _, payer, _, ctx := moneyInEnv(t)
	_, err := svc.Deposit(ctx, money.DepositParams{CustomerID: &payer, Invoker: payer.UUID().String(), Currency: money.DefaultCurrency, Amount: 500, Source: "seed"})
	require.NoError(t, err)

	res, err := svc.AuthorizeAndHold(ctx, money.AuthorizeHoldInput{
		Payer: payer, Invoker: "u", Currency: money.DefaultCurrency, EstimatedAmount: 600,
		Key: money.MustIdempotencyKey(money.OpCapture, "usage", "over"), ExpiresAt: time.Now().Add(time.Hour),
	})
	require.NoError(t, err)
	require.False(t, res.Decision.Allowed)
	require.Equal(t, money.DenyInsufficientBalance, res.Decision.DenyCode)
}
