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

	// Balance 0, credit line 1000. A hold of 800 is allowed (balance goes to -800).
	res, err := svc.AuthorizeAndHold(ctx, money.AuthorizeHoldInput{
		Payer: payer, Invoker: "u", EstimatedAmount: 800,
		Source: "usage", SourceID: "h1", ExpiresAt: time.Now().Add(time.Hour),
	})
	require.NoError(t, err)
	require.True(t, res.Decision.Allowed)
	require.NotNil(t, res.Hold)

	// A second hold of 300 would push exposure to 1100 > 1000 -> insufficient_credit.
	res, err = svc.AuthorizeAndHold(ctx, money.AuthorizeHoldInput{
		Payer: payer, Invoker: "u", EstimatedAmount: 300,
		Source: "usage", SourceID: "h2", ExpiresAt: time.Now().Add(time.Hour),
	})
	require.NoError(t, err)
	require.False(t, res.Decision.Allowed)
	require.Equal(t, money.DenyInsufficientCredit, res.Decision.DenyCode)
	require.Nil(t, res.Hold)

	// Exactly at the line (another 200, total held 1000) -> allowed.
	res, err = svc.AuthorizeAndHold(ctx, money.AuthorizeHoldInput{
		Payer: payer, Invoker: "u", EstimatedAmount: 200,
		Source: "usage", SourceID: "h3", ExpiresAt: time.Now().Add(time.Hour),
	})
	require.NoError(t, err)
	require.True(t, res.Decision.Allowed)
}

// #489: credit_limit=0 (default OFF) preserves the EXISTING arrears behavior
// (#302) — a hold may exceed the balance (bounded by the outstanding ceiling, not
// the balance) — rather than the literal "prepaid" reading, which would regress
// arrears-spill-past-balance. Prepaid accounts are unaffected (gated on balance).
func TestAuthorizeAndHold_ArrearsNoCreditLine_ExistingBehavior(t *testing.T) {
	svc, _, payer, _, ctx := moneyInEnv(t)
	_, err := svc.UpsertAccountSettings(ctx, payer, money.DefaultCurrency, money.AccountSettingsInput{BillingMode: strptr(money.BillingModeArrears)})
	require.NoError(t, err)
	// No credit line set (default 0). Balance 500.
	_, err = svc.Deposit(ctx, money.DepositParams{CustomerID: &payer, Invoker: payer.UUID().String(), Amount: 500, Source: "seed"})
	require.NoError(t, err)

	// A hold of 2000 (4x balance) is allowed under arrears with no credit line
	// (existing #302 behavior — bounded by MaxOutstandingOwed, not the balance).
	res, err := svc.AuthorizeAndHold(ctx, money.AuthorizeHoldInput{
		Payer: payer, Invoker: "u", EstimatedAmount: 2000,
		Source: "usage", SourceID: "spill", ExpiresAt: time.Now().Add(time.Hour),
	})
	require.NoError(t, err)
	require.True(t, res.Decision.Allowed)
	require.NotNil(t, res.Hold)
}

// #489: a PREPAID account is unaffected by the credit-line change — a hold beyond
// available balance is denied insufficient_balance.
func TestAuthorizeAndHold_PrepaidUnaffected(t *testing.T) {
	svc, _, payer, _, ctx := moneyInEnv(t)
	_, err := svc.Deposit(ctx, money.DepositParams{CustomerID: &payer, Invoker: payer.UUID().String(), Amount: 500, Source: "seed"})
	require.NoError(t, err)

	res, err := svc.AuthorizeAndHold(ctx, money.AuthorizeHoldInput{
		Payer: payer, Invoker: "u", EstimatedAmount: 600,
		Source: "usage", SourceID: "over", ExpiresAt: time.Now().Add(time.Hour),
	})
	require.NoError(t, err)
	require.False(t, res.Decision.Allowed)
	require.Equal(t, money.DenyInsufficientBalance, res.Decision.DenyCode)
}
