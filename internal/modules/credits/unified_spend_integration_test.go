//go:build integration

package credits_test

import (
	"testing"

	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/credits"
	"github.com/stretchr/testify/require"
)

func TestSpendCredits_PrepaidFloorsAtBalance(t *testing.T) {
	svc, _, payer, ct, ctx := moneyInEnv(t)
	_, err := svc.Deposit(ctx, credits.CreditDepositParams{TenantSubjectID: &payer, InvokerID: payer.UUID().String(), CreditType: ct, Amount: 1000, Source: "seed"})
	require.NoError(t, err)

	// Prepay-only (no credit line): cannot exceed balance.
	err = svc.SpendCredits(ctx, credits.SpendParams{Payer: &payer, InvokerID: "u", CreditType: ct, Amount: 1500, Source: "s", SourceID: "x1"})
	require.ErrorIs(t, err, credits.ErrInsufficientCredits)
	bal, _ := svc.GetBalanceForTenantSubject(ctx, payer, ct)
	require.Equal(t, int64(1000), bal.Balance, "denied spend must not debit")

	err = svc.SpendCredits(ctx, credits.SpendParams{Payer: &payer, InvokerID: "u", CreditType: ct, Amount: 600, Source: "s", SourceID: "x2"})
	require.NoError(t, err)
	bal, _ = svc.GetBalanceForTenantSubject(ctx, payer, ct)
	require.Equal(t, int64(400), bal.Balance)
}

func TestSpendCredits_ArrearsAccruesBeyondBalance(t *testing.T) {
	svc, _, payer, ct, ctx := moneyInEnv(t)
	_, err := svc.UpsertAccountSettings(ctx, payer, ct, credits.AccountSettingsInput{BillingMode: strptr(credits.BillingModeArrears)})
	require.NoError(t, err)
	_, err = svc.Deposit(ctx, credits.CreditDepositParams{TenantSubjectID: &payer, InvokerID: payer.UUID().String(), CreditType: ct, Amount: 1000, Source: "seed"})
	require.NoError(t, err)

	err = svc.SpendCredits(ctx, credits.SpendParams{Payer: &payer, InvokerID: "u", CreditType: ct, Amount: 1500, Source: "s", SourceID: "x1"})
	require.NoError(t, err)

	bal, _ := svc.GetBalanceForTenantSubject(ctx, payer, ct)
	require.Equal(t, int64(0), bal.Balance, "balance drawn first")
	owed, _ := svc.GetOutstandingOwed(ctx, payer, ct)
	require.Equal(t, int64(500), owed, "remainder accrued to owed")
}

func TestSpendCredits_HybridSpansBalanceThenOwed(t *testing.T) {
	svc, _, payer, ct, ctx := moneyInEnv(t)
	limit := int64(10000)
	_, err := svc.UpsertAccountSettings(ctx, payer, ct, credits.AccountSettingsInput{BillingMode: strptr(credits.BillingModeArrears), MaxOutstandingOwedCents: &limit})
	require.NoError(t, err)
	_, err = svc.Deposit(ctx, credits.CreditDepositParams{TenantSubjectID: &payer, InvokerID: payer.UUID().String(), CreditType: ct, Amount: 500, Source: "seed"})
	require.NoError(t, err)

	err = svc.SpendCredits(ctx, credits.SpendParams{Payer: &payer, InvokerID: "u", CreditType: ct, Amount: 2000, Source: "s", SourceID: "x1"})
	require.NoError(t, err)
	bal, _ := svc.GetBalanceForTenantSubject(ctx, payer, ct)
	require.Equal(t, int64(0), bal.Balance)
	owed, _ := svc.GetOutstandingOwed(ctx, payer, ct)
	require.Equal(t, int64(1500), owed)
}

func TestSpendCredits_RespectsCreditLimit(t *testing.T) {
	svc, _, payer, ct, ctx := moneyInEnv(t)
	limit := int64(1000)
	_, err := svc.UpsertAccountSettings(ctx, payer, ct, credits.AccountSettingsInput{BillingMode: strptr(credits.BillingModeArrears), MaxOutstandingOwedCents: &limit})
	require.NoError(t, err)
	// balance 0, credit line 1000.
	err = svc.SpendCredits(ctx, credits.SpendParams{Payer: &payer, InvokerID: "u", CreditType: ct, Amount: 1500, Source: "s", SourceID: "x1"})
	require.ErrorIs(t, err, credits.ErrInsufficientCredits, "exceeds credit line")

	err = svc.SpendCredits(ctx, credits.SpendParams{Payer: &payer, InvokerID: "u", CreditType: ct, Amount: 1000, Source: "s", SourceID: "x2"})
	require.NoError(t, err)
	owed, _ := svc.GetOutstandingOwed(ctx, payer, ct)
	require.Equal(t, int64(1000), owed)

	err = svc.SpendCredits(ctx, credits.SpendParams{Payer: &payer, InvokerID: "u", CreditType: ct, Amount: 1, Source: "s", SourceID: "x3"})
	require.ErrorIs(t, err, credits.ErrInsufficientCredits, "would exceed line by 1")
}

func TestSpendCredits_Idempotent(t *testing.T) {
	svc, _, payer, ct, ctx := moneyInEnv(t)
	_, err := svc.Deposit(ctx, credits.CreditDepositParams{TenantSubjectID: &payer, InvokerID: payer.UUID().String(), CreditType: ct, Amount: 1000, Source: "seed"})
	require.NoError(t, err)
	p := credits.SpendParams{Payer: &payer, InvokerID: "u", CreditType: ct, Amount: 300, Source: "s", SourceID: "dup"}
	require.NoError(t, svc.SpendCredits(ctx, p))
	require.NoError(t, svc.SpendCredits(ctx, p)) // replay
	bal, _ := svc.GetBalanceForTenantSubject(ctx, payer, ct)
	require.Equal(t, int64(700), bal.Balance, "replay must not double-spend")
}

func TestRecordUsage_ArrearsDrawsThenAccrues(t *testing.T) {
	svc, bunDB, payer, ct, ctx := moneyInEnv(t)
	t.Cleanup(func() {
		_, _ = bunDB.NewDelete().Model((*models.UsageEvent)(nil)).Where("tenant_subject_id = ?", payer.UUID()).Exec(ctx)
	})
	_, err := svc.UpsertAccountSettings(ctx, payer, ct, credits.AccountSettingsInput{BillingMode: strptr(credits.BillingModeArrears)})
	require.NoError(t, err)
	_, err = svc.Deposit(ctx, credits.CreditDepositParams{TenantSubjectID: &payer, InvokerID: payer.UUID().String(), CreditType: ct, Amount: 1000, Source: "seed"})
	require.NoError(t, err)

	_, err = svc.RecordUsage(ctx, credits.RecordUsageParams{
		Payer: &payer, InvokerID: "u", CreditType: ct, EventType: "gpt-4o",
		Amount: 1500, Source: "req", SourceID: "r1",
	})
	require.NoError(t, err)
	bal, _ := svc.GetBalanceForTenantSubject(ctx, payer, ct)
	require.Equal(t, int64(0), bal.Balance)
	owed, _ := svc.GetOutstandingOwed(ctx, payer, ct)
	require.Equal(t, int64(500), owed)
}
