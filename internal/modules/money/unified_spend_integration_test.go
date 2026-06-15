//go:build integration

package money_test

import (
	"testing"

	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/stretchr/testify/require"
)

func TestSpendCredits_PrepaidFloorsAtBalance(t *testing.T) {
	svc, _, payer, cur, ctx := moneyInEnv(t)
	_, err := svc.Deposit(ctx, money.DepositParams{CustomerID: &payer, Invoker: payer.UUID().String(), Currency: money.DefaultCurrency, Amount: 1000, Source: "seed"})
	require.NoError(t, err)

	// Prepay-only (no credit line): cannot exceed balance.
	err = svc.SpendCredits(ctx, money.SpendParams{Payer: &payer, Invoker: "u", Currency: money.DefaultCurrency, Amount: 1500, Source: "s", SourceID: "x1"})
	require.ErrorIs(t, err, money.ErrInsufficientCredits)
	bal, _ := svc.GetBalanceForCustomer(ctx, payer, cur)
	require.Equal(t, int64(1000), bal.Balance, "denied spend must not debit")

	err = svc.SpendCredits(ctx, money.SpendParams{Payer: &payer, Invoker: "u", Currency: money.DefaultCurrency, Amount: 600, Source: "s", SourceID: "x2"})
	require.NoError(t, err)
	bal, _ = svc.GetBalanceForCustomer(ctx, payer, cur)
	require.Equal(t, int64(400), bal.Balance)
}

func TestSpendCredits_ArrearsAccruesBeyondBalance(t *testing.T) {
	svc, _, payer, cur, ctx := moneyInEnv(t)
	_, err := svc.UpsertAccountSettings(ctx, payer, money.DefaultCurrency, money.AccountSettingsInput{BillingMode: strptr(money.BillingModeArrears)})
	require.NoError(t, err)
	_, err = svc.Deposit(ctx, money.DepositParams{CustomerID: &payer, Invoker: payer.UUID().String(), Currency: money.DefaultCurrency, Amount: 1000, Source: "seed"})
	require.NoError(t, err)

	err = svc.SpendCredits(ctx, money.SpendParams{Payer: &payer, Invoker: "u", Currency: money.DefaultCurrency, Amount: 1500, Source: "s", SourceID: "x1"})
	require.NoError(t, err)

	bal, _ := svc.GetBalanceForCustomer(ctx, payer, cur)
	require.Equal(t, int64(0), bal.Balance, "balance drawn first")
	owed, _ := svc.GetOutstandingOwed(ctx, payer, cur)
	require.Equal(t, int64(500), owed, "remainder accrued to owed")
}

func TestSpendCredits_HybridSpansBalanceThenOwed(t *testing.T) {
	svc, _, payer, cur, ctx := moneyInEnv(t)
	limit := int64(10000)
	_, err := svc.UpsertAccountSettings(ctx, payer, money.DefaultCurrency, money.AccountSettingsInput{BillingMode: strptr(money.BillingModeArrears), MaxOutstandingOwedAmount: &limit})
	require.NoError(t, err)
	_, err = svc.Deposit(ctx, money.DepositParams{CustomerID: &payer, Invoker: payer.UUID().String(), Currency: money.DefaultCurrency, Amount: 500, Source: "seed"})
	require.NoError(t, err)

	err = svc.SpendCredits(ctx, money.SpendParams{Payer: &payer, Invoker: "u", Currency: money.DefaultCurrency, Amount: 2000, Source: "s", SourceID: "x1"})
	require.NoError(t, err)
	bal, _ := svc.GetBalanceForCustomer(ctx, payer, cur)
	require.Equal(t, int64(0), bal.Balance)
	owed, _ := svc.GetOutstandingOwed(ctx, payer, cur)
	require.Equal(t, int64(1500), owed)
}

func TestSpendCredits_RespectsCreditLimit(t *testing.T) {
	svc, _, payer, cur, ctx := moneyInEnv(t)
	limit := int64(1000)
	_, err := svc.UpsertAccountSettings(ctx, payer, money.DefaultCurrency, money.AccountSettingsInput{BillingMode: strptr(money.BillingModeArrears), MaxOutstandingOwedAmount: &limit})
	require.NoError(t, err)
	// balance 0, credit line 1000.
	err = svc.SpendCredits(ctx, money.SpendParams{Payer: &payer, Invoker: "u", Currency: money.DefaultCurrency, Amount: 1500, Source: "s", SourceID: "x1"})
	require.ErrorIs(t, err, money.ErrInsufficientCredits, "exceeds credit line")

	err = svc.SpendCredits(ctx, money.SpendParams{Payer: &payer, Invoker: "u", Currency: money.DefaultCurrency, Amount: 1000, Source: "s", SourceID: "x2"})
	require.NoError(t, err)
	owed, _ := svc.GetOutstandingOwed(ctx, payer, cur)
	require.Equal(t, int64(1000), owed)

	err = svc.SpendCredits(ctx, money.SpendParams{Payer: &payer, Invoker: "u", Currency: money.DefaultCurrency, Amount: 1, Source: "s", SourceID: "x3"})
	require.ErrorIs(t, err, money.ErrInsufficientCredits, "would exceed line by 1")
}

func TestSpendCredits_Idempotent(t *testing.T) {
	svc, _, payer, cur, ctx := moneyInEnv(t)
	_, err := svc.Deposit(ctx, money.DepositParams{CustomerID: &payer, Invoker: payer.UUID().String(), Currency: money.DefaultCurrency, Amount: 1000, Source: "seed"})
	require.NoError(t, err)
	p := money.SpendParams{Payer: &payer, Invoker: "u", Currency: money.DefaultCurrency, Amount: 300, Source: "s", SourceID: "dup"}
	require.NoError(t, svc.SpendCredits(ctx, p))
	require.NoError(t, svc.SpendCredits(ctx, p)) // replay
	bal, _ := svc.GetBalanceForCustomer(ctx, payer, cur)
	require.Equal(t, int64(700), bal.Balance, "replay must not double-spend")
}

func TestRecordUsage_ArrearsDrawsThenAccrues(t *testing.T) {
	svc, pool, payer, cur, ctx := moneyInEnv(t)
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.usage_events WHERE customer_id = $1", payer.UUID())
	})
	_, err := svc.UpsertAccountSettings(ctx, payer, money.DefaultCurrency, money.AccountSettingsInput{BillingMode: strptr(money.BillingModeArrears)})
	require.NoError(t, err)
	_, err = svc.Deposit(ctx, money.DepositParams{CustomerID: &payer, Invoker: payer.UUID().String(), Currency: money.DefaultCurrency, Amount: 1000, Source: "seed"})
	require.NoError(t, err)

	_, err = svc.RecordUsage(ctx, money.RecordUsageParams{
		Payer: &payer, Invoker: "u", Currency: money.DefaultCurrency, EventType: "gpt-4o",
		Amount: 1500, Source: "req", SourceID: "r1",
	})
	require.NoError(t, err)
	bal, _ := svc.GetBalanceForCustomer(ctx, payer, cur)
	require.Equal(t, int64(0), bal.Balance)
	owed, _ := svc.GetOutstandingOwed(ctx, payer, cur)
	require.Equal(t, int64(500), owed)
}
