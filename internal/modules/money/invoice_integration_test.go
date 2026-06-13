//go:build integration

package money_test

import (
	"testing"
	"time"

	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/money"
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
	svc, pool, payer, _, ctx := moneyInEnv(t)
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.usage_events WHERE tenant_subject_id = $1", payer.UUID())
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.invoices WHERE tenant_subject_id = $1", payer.UUID())
	})

	_, err := svc.Deposit(ctx, money.DepositParams{TenantSubjectID: &payer, Actor: payer.UUID().String(), Amount: 100_000, Source: "purchase"})
	require.NoError(t, err)
	rec := func(et string, amt int64, dims map[string]int64, sid string) {
		_, e := svc.RecordUsage(ctx, money.RecordUsageParams{Payer: &payer, Actor: "u", EventType: et, Dimensions: dims, Amount: amt, Source: "req", SourceID: sid})
		require.NoError(t, e)
	}
	rec("gpt-4o", 5_000, map[string]int64{"input_tokens": 100, "output_tokens": 50}, "r1")
	rec("gpt-4o", 3_000, map[string]int64{"input_tokens": 60, "output_tokens": 30}, "r2")
	rec("dall-e-3", 2_000, map[string]int64{"images": 4}, "r3")

	from, to := time.Now().Add(-time.Hour), time.Now().Add(time.Hour)
	inv, err := svc.FinalizeInvoice(ctx, payer, "", from, to)
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
	inv2, err := svc.FinalizeInvoice(ctx, payer, "", from, to)
	require.NoError(t, err)
	require.Equal(t, inv.ID, inv2.ID)
}

func TestFinalizeInvoice_ArrearsOwed(t *testing.T) {
	svc, pool, payer, _, ctx := moneyInEnv(t)
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.usage_events WHERE tenant_subject_id = $1", payer.UUID())
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.invoices WHERE tenant_subject_id = $1", payer.UUID())
	})
	_, err := svc.UpsertAccountSettings(ctx, payer, money.AccountSettingsInput{BillingMode: strptr(money.BillingModeArrears)})
	require.NoError(t, err)
	_, err = svc.Deposit(ctx, money.DepositParams{TenantSubjectID: &payer, Actor: payer.UUID().String(), Amount: 1_000, Source: "seed"})
	require.NoError(t, err)
	_, err = svc.RecordUsage(ctx, money.RecordUsageParams{Payer: &payer, Actor: "u", EventType: "gpt-4o", Amount: 1_500, Source: "req", SourceID: "r1"})
	require.NoError(t, err)

	from, to := time.Now().Add(-time.Hour), time.Now().Add(time.Hour)
	inv, err := svc.FinalizeInvoice(ctx, payer, "", from, to)
	require.NoError(t, err)
	require.Equal(t, int64(1_500), inv.UsageTotal)
	require.Equal(t, int64(500), inv.OwedAccrued, "credit-line-funded usage shows as owed")
	require.Equal(t, int64(0), inv.ClosingBalance)
	require.Equal(t, int64(-1_000), inv.MoneyMovements["withdrawal"])
	require.Equal(t, int64(500), inv.MoneyMovements["owed_accrual"])
}

// FinalizeDueInvoices enumerates the tenant's payers and finalizes each one's
// invoice for the period (#472). The deposit above creates the payer's
// money_accounts row, so enumeration must find and finalize it.
func TestFinalizeDueInvoices_EnumeratesAccount(t *testing.T) {
	svc, pool, payer, _, ctx := moneyInEnv(t)
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.usage_events WHERE tenant_subject_id = $1", payer.UUID())
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.invoices WHERE tenant_subject_id = $1", payer.UUID())
	})
	_, err := svc.Deposit(ctx, money.DepositParams{TenantSubjectID: &payer, Actor: payer.UUID().String(), Amount: 5_000, Source: "seed"})
	require.NoError(t, err)
	_, err = svc.RecordUsage(ctx, money.RecordUsageParams{Payer: &payer, Actor: "u", EventType: "gpt-4o", Amount: 2_000, Source: "req", SourceID: "r1"})
	require.NoError(t, err)

	from, to := time.Now().Add(-time.Hour), time.Now().Add(time.Hour)
	n, err := svc.FinalizeDueInvoices(ctx, from, to)
	require.NoError(t, err)
	require.GreaterOrEqual(t, n, 1, "the depositing payer must be enumerated and finalized")

	// the payer's invoice for the period is now persisted
	inv, err := svc.FinalizeInvoice(ctx, payer, "", from, to)
	require.NoError(t, err)
	require.Equal(t, int64(2_000), inv.UsageTotal)
	require.Equal(t, "finalized", inv.Status)
}
