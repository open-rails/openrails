//go:build integration

package credits_test

import (
	"testing"
	"time"

	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/credits"
	"github.com/stretchr/testify/require"
)

func findItem(items []models.InvoiceLineItem, eventType string) *models.InvoiceLineItem {
	for i := range items {
		if items[i].EventType == eventType {
			return &items[i]
		}
	}
	return nil
}

func TestFinalizeInvoice_PrepaidStatement(t *testing.T) {
	svc, bunDB, payer, ct, ctx := moneyInEnv(t)
	t.Cleanup(func() {
		_, _ = bunDB.NewDelete().Model((*models.UsageEvent)(nil)).Where("tenant_subject_id = ?", payer.UUID()).Exec(ctx)
		_, _ = bunDB.NewDelete().Model((*models.Invoice)(nil)).Where("tenant_subject_id = ?", payer.UUID()).Exec(ctx)
	})

	_, err := svc.Deposit(ctx, credits.CreditDepositParams{TenantSubjectID: &payer, Actor: payer.UUID().String(), CreditType: ct, Amount: 100_000, Source: "purchase"})
	require.NoError(t, err)
	rec := func(et string, amt int64, dims map[string]int64, sid string) {
		_, e := svc.RecordUsage(ctx, credits.RecordUsageParams{Payer: &payer, Actor: "u", CreditType: ct, EventType: et, Dimensions: dims, Amount: amt, Source: "req", SourceID: sid})
		require.NoError(t, e)
	}
	rec("gpt-4o", 5_000, map[string]int64{"input_tokens": 100, "output_tokens": 50}, "r1")
	rec("gpt-4o", 3_000, map[string]int64{"input_tokens": 60, "output_tokens": 30}, "r2")
	rec("dall-e-3", 2_000, map[string]int64{"images": 4}, "r3")

	from, to := time.Now().Add(-time.Hour), time.Now().Add(time.Hour)
	inv, err := svc.FinalizeInvoice(ctx, payer, ct, from, to)
	require.NoError(t, err)
	require.Equal(t, "finalized", inv.Status)
	require.NotNil(t, inv.FinalizedAt)
	require.Equal(t, int64(10_000), inv.UsageTotal)
	require.Equal(t, int64(100_000), inv.DepositsTotal)
	require.Equal(t, int64(90_000), inv.ClosingBalance)
	require.Len(t, inv.LineItems, 2)

	gpt := findItem(inv.LineItems, "gpt-4o")
	require.NotNil(t, gpt)
	require.Equal(t, int64(8_000), gpt.Amount)
	require.Equal(t, int64(2), gpt.Count)
	require.Equal(t, int64(160), gpt.Dimensions["input_tokens"])
	require.Equal(t, int64(80), gpt.Dimensions["output_tokens"])

	dalle := findItem(inv.LineItems, "dall-e-3")
	require.NotNil(t, dalle)
	require.Equal(t, int64(2_000), dalle.Amount)
	require.Equal(t, int64(4), dalle.Dimensions["images"])

	require.Equal(t, int64(-10_000), inv.MoneyMovements["withdrawal"])
	require.Equal(t, int64(100_000), inv.MoneyMovements["deposit"])

	// Idempotent.
	inv2, err := svc.FinalizeInvoice(ctx, payer, ct, from, to)
	require.NoError(t, err)
	require.Equal(t, inv.ID, inv2.ID)
}

func TestFinalizeInvoice_ArrearsOwed(t *testing.T) {
	svc, bunDB, payer, ct, ctx := moneyInEnv(t)
	t.Cleanup(func() {
		_, _ = bunDB.NewDelete().Model((*models.UsageEvent)(nil)).Where("tenant_subject_id = ?", payer.UUID()).Exec(ctx)
		_, _ = bunDB.NewDelete().Model((*models.Invoice)(nil)).Where("tenant_subject_id = ?", payer.UUID()).Exec(ctx)
	})
	_, err := svc.UpsertAccountSettings(ctx, payer, ct, credits.AccountSettingsInput{BillingMode: strptr(credits.BillingModeArrears)})
	require.NoError(t, err)
	_, err = svc.Deposit(ctx, credits.CreditDepositParams{TenantSubjectID: &payer, Actor: payer.UUID().String(), CreditType: ct, Amount: 1_000, Source: "seed"})
	require.NoError(t, err)
	_, err = svc.RecordUsage(ctx, credits.RecordUsageParams{Payer: &payer, Actor: "u", CreditType: ct, EventType: "gpt-4o", Amount: 1_500, Source: "req", SourceID: "r1"})
	require.NoError(t, err)

	from, to := time.Now().Add(-time.Hour), time.Now().Add(time.Hour)
	inv, err := svc.FinalizeInvoice(ctx, payer, ct, from, to)
	require.NoError(t, err)
	require.Equal(t, int64(1_500), inv.UsageTotal)
	require.Equal(t, int64(500), inv.OwedAccrued, "credit-line-funded usage shows as owed")
	require.Equal(t, int64(0), inv.ClosingBalance)
	require.Equal(t, int64(-1_000), inv.MoneyMovements["withdrawal"])
	require.Equal(t, int64(500), inv.MoneyMovements["owed_accrual"])
}

func TestFinalizeDueInvoices_EnumeratesAccount(t *testing.T) {
	svc, bunDB, payer, ct, ctx := moneyInEnv(t)
	t.Cleanup(func() {
		_, _ = bunDB.NewDelete().Model((*models.UsageEvent)(nil)).Where("tenant_subject_id = ?", payer.UUID()).Exec(ctx)
		_, _ = bunDB.NewDelete().Model((*models.Invoice)(nil)).Where("tenant_subject_id = ?", payer.UUID()).Exec(ctx)
	})
	_, err := svc.Deposit(ctx, credits.CreditDepositParams{TenantSubjectID: &payer, Actor: payer.UUID().String(), CreditType: ct, Amount: 5_000, Source: "seed"})
	require.NoError(t, err)
	_, err = svc.RecordUsage(ctx, credits.RecordUsageParams{Payer: &payer, Actor: "u", CreditType: ct, EventType: "gpt-4o", Amount: 2_000, Source: "req", SourceID: "r1"})
	require.NoError(t, err)

	from, to := time.Now().Add(-time.Hour), time.Now().Add(time.Hour)
	n, err := svc.FinalizeDueInvoices(ctx, from, to)
	require.NoError(t, err)
	require.GreaterOrEqual(t, n, 1)

	// This payer's invoice exists with the right usage total (bunDB bypasses RLS).
	inv := new(models.Invoice)
	require.NoError(t, bunDB.NewSelect().Model(inv).Where("tenant_subject_id = ?", payer.UUID()).Limit(1).Scan(ctx))
	require.Equal(t, int64(2_000), inv.UsageTotal)
	require.Equal(t, "finalized", inv.Status)
}
