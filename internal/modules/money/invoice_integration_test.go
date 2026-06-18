//go:build integration

package money_test

import (
	"testing"
	"time"

	"github.com/google/uuid"
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
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.invoice_items WHERE customer_id = $1", payer.UUID())
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.usage_events WHERE customer_id = $1", payer.UUID())
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.invoices WHERE customer_id = $1", payer.UUID())
	})

	_, err := svc.Deposit(ctx, money.DepositParams{CustomerID: &payer, Invoker: payer.UUID().String(), Currency: money.DefaultCurrency, Amount: 100_000, Source: "purchase"})
	require.NoError(t, err)
	rec := func(et string, amt int64, dims map[string]int64, sid string) {
		_, e := svc.RecordUsage(ctx, money.RecordUsageParams{Payer: &payer, Invoker: "u", Currency: money.DefaultCurrency, EventType: et, Dimensions: dims, Amount: amt, Source: "req", SourceID: sid})
		require.NoError(t, e)
	}
	rec("gpt-4o", 5_000, map[string]int64{"input_tokens": 100, "output_tokens": 50}, "r1")
	rec("gpt-4o", 3_000, map[string]int64{"input_tokens": 60, "output_tokens": 30}, "r2")
	rec("dall-e-3", 2_000, map[string]int64{"images": 4}, "r3")

	from, to := time.Now().Add(-time.Hour), time.Now().Add(time.Hour)
	inv, err := svc.FinalizeInvoice(ctx, payer, money.DefaultCurrency, from, to)
	require.NoError(t, err)
	require.Equal(t, "paid", inv.Status)
	require.NotNil(t, inv.FinalizedAt)
	require.NotNil(t, inv.PaidAt)
	require.Equal(t, int64(10_000), inv.UsageTotal)
	require.Equal(t, int64(10_000), inv.TotalAmount)
	require.Equal(t, int64(0), inv.AmountDue)
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
	inv2, err := svc.FinalizeInvoice(ctx, payer, money.DefaultCurrency, from, to)
	require.NoError(t, err)
	require.Equal(t, inv.ID, inv2.ID)
}

func TestFinalizeInvoice_ArrearsOwed(t *testing.T) {
	svc, pool, payer, _, ctx := moneyInEnv(t)
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.invoice_payments WHERE customer_id = $1", payer.UUID())
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.invoice_items WHERE customer_id = $1", payer.UUID())
		_, _ = pool.Exec(ctx, "UPDATE openrails.money_transactions SET invoice_id = NULL WHERE customer_id = $1", payer.UUID())
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.usage_events WHERE customer_id = $1", payer.UUID())
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.invoices WHERE customer_id = $1", payer.UUID())
	})
	pm := uuid.New()
	_, err := svc.UpsertAccountSettings(ctx, payer, money.DefaultCurrency, money.AccountSettingsInput{
		BillingMode: strptr(money.BillingModeArrears), AutoTopupPaymentMethod: &pm,
	})
	require.NoError(t, err)
	require.NoError(t, svc.SetCreditLimit(ctx, payer, money.DefaultCurrency, 1_000))
	_, err = svc.Deposit(ctx, money.DepositParams{CustomerID: &payer, Invoker: payer.UUID().String(), Currency: money.DefaultCurrency, Amount: 1_000, Source: "seed"})
	require.NoError(t, err)
	_, err = svc.RecordUsage(ctx, money.RecordUsageParams{Payer: &payer, Invoker: "u", Currency: money.DefaultCurrency, EventType: "gpt-4o", Amount: 1_500, Source: "req", SourceID: "r1"})
	require.NoError(t, err)

	var pendingBefore int
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT count(*)
		FROM openrails.invoice_items
		WHERE customer_id = $1 AND invoice_id IS NULL AND status = 'pending'
	`, payer.UUID()).Scan(&pendingBefore))
	require.Equal(t, 1, pendingBefore, "arrears spill creates a pending invoice item before invoice finalization")

	from, to := time.Now().Add(-time.Hour), time.Now().Add(time.Hour)
	inv, err := svc.FinalizeInvoice(ctx, payer, money.DefaultCurrency, from, to)
	require.NoError(t, err)
	require.Equal(t, int64(1_500), inv.UsageTotal)
	require.Equal(t, int64(500), inv.OwedAccrued, "credit-line-funded usage shows as owed")
	require.Equal(t, int64(500), inv.TotalAmount)
	require.Equal(t, int64(500), inv.AmountDue)
	require.Equal(t, "open", inv.Status)
	require.Equal(t, int64(0), inv.ClosingBalance)
	require.Equal(t, int64(-1_000), inv.MoneyMovements["withdrawal"])
	require.Equal(t, int64(500), inv.MoneyMovements["owed_accrual"])

	var itemCount int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM openrails.invoice_items WHERE invoice_id = $1`, inv.ID).Scan(&itemCount))
	require.Equal(t, 1, itemCount)
	var pendingAfter int
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT count(*)
		FROM openrails.invoice_items
		WHERE customer_id = $1 AND invoice_id IS NULL AND status = 'pending'
	`, payer.UUID()).Scan(&pendingAfter))
	require.Equal(t, 0, pendingAfter, "finalization attaches pending invoice items")

	ch := &fakeCharger{}
	n, err := svc.ChargeOutstanding(ctx, ch, 0)
	require.NoError(t, err)
	require.Equal(t, 1, n)
	require.Len(t, ch.charges, 1)

	paid, err := svc.GetInvoiceByID(ctx, payer, inv.ID)
	require.NoError(t, err)
	require.Equal(t, "paid", paid.Status)
	require.Equal(t, int64(500), paid.AmountPaid)
	require.Equal(t, int64(0), paid.AmountDue)
	require.NotNil(t, paid.PaidAt)
}

func TestInvoiceCollectionDeclineLeavesInvoiceOpenAndBlocksArrears(t *testing.T) {
	svc, pool, payer, _, ctx := moneyInEnv(t)
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.invoice_payments WHERE customer_id = $1", payer.UUID())
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.invoice_items WHERE customer_id = $1", payer.UUID())
		_, _ = pool.Exec(ctx, "UPDATE openrails.money_transactions SET invoice_id = NULL WHERE customer_id = $1", payer.UUID())
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.usage_events WHERE customer_id = $1", payer.UUID())
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.invoices WHERE customer_id = $1", payer.UUID())
	})
	pm := uuid.New()
	_, err := svc.UpsertAccountSettings(ctx, payer, money.DefaultCurrency, money.AccountSettingsInput{
		BillingMode: strptr(money.BillingModeArrears), AutoTopupPaymentMethod: &pm,
	})
	require.NoError(t, err)
	require.NoError(t, svc.SetCreditLimit(ctx, payer, money.DefaultCurrency, 500))
	_, err = svc.AccrueOwed(ctx, payer, money.DefaultCurrency, "usage", "declined-invoice", 500)
	require.NoError(t, err)

	from, to := time.Now().Add(-time.Hour), time.Now().Add(time.Hour)
	inv, err := svc.FinalizeInvoice(ctx, payer, money.DefaultCurrency, from, to)
	require.NoError(t, err)
	require.Equal(t, "open", inv.Status)
	require.Equal(t, int64(500), inv.AmountDue)

	ch := &fakeCharger{declineAll: true}
	n, err := svc.ChargeOutstanding(ctx, ch, 0)
	require.NoError(t, err)
	require.Equal(t, 0, n)
	require.Len(t, ch.charges, 1)

	stillOpen, err := svc.GetInvoiceByID(ctx, payer, inv.ID)
	require.NoError(t, err)
	require.Equal(t, "open", stillOpen.Status)
	require.Equal(t, int64(500), stillOpen.AmountDue)
	require.Equal(t, int64(0), stillOpen.AmountPaid)
	var failedAttempts int
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT count(*)
		FROM openrails.invoice_payments
		WHERE invoice_id = $1 AND status = 'failed'
	`, inv.ID).Scan(&failedAttempts))
	require.Equal(t, 1, failedAttempts)

	res, err := svc.AuthorizeAndHold(ctx, money.AuthorizeHoldInput{
		Payer: payer, Invoker: "u", Currency: money.DefaultCurrency, EstimatedAmount: 1,
		Source: "usage", SourceID: "blocked-after-decline", ExpiresAt: time.Now().Add(time.Hour),
	})
	require.NoError(t, err)
	require.False(t, res.Decision.Allowed)
	require.Equal(t, money.DenyInsufficientCredit, res.Decision.DenyCode)
}

func TestFinalizeThresholdInvoices_CapHitCreatesCollectableInvoice(t *testing.T) {
	svc, pool, payer, _, ctx := moneyInEnv(t)
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.invoice_payments WHERE customer_id = $1", payer.UUID())
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.invoice_items WHERE customer_id = $1", payer.UUID())
		_, _ = pool.Exec(ctx, "UPDATE openrails.money_transactions SET invoice_id = NULL WHERE customer_id = $1", payer.UUID())
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.usage_events WHERE customer_id = $1", payer.UUID())
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.invoices WHERE customer_id = $1", payer.UUID())
	})
	pm := uuid.New()
	_, err := svc.UpsertAccountSettings(ctx, payer, money.DefaultCurrency, money.AccountSettingsInput{
		BillingMode: strptr(money.BillingModeArrears), AutoTopupPaymentMethod: &pm,
	})
	require.NoError(t, err)
	require.NoError(t, svc.SetCreditLimit(ctx, payer, money.DefaultCurrency, 500))
	_, err = svc.AccrueOwed(ctx, payer, money.DefaultCurrency, "usage", "threshold-invoice", 500)
	require.NoError(t, err)

	var rawCounter int64
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT outstanding_owed_amount
		FROM openrails.money_settings
		WHERE customer_id = $1 AND currency = $2
	`, payer.UUID(), money.DefaultCurrency).Scan(&rawCounter))
	require.Equal(t, int64(0), rawCounter, "runtime exposure is invoice-derived, not the old settings counter")

	n, err := svc.FinalizeThresholdInvoices(ctx, time.Now().Add(time.Hour))
	require.NoError(t, err)
	require.Equal(t, 1, n)

	invoices, total, err := svc.ListInvoices(ctx, payer, 10, 0)
	require.NoError(t, err)
	require.Equal(t, 1, total)
	require.Len(t, invoices, 1)
	require.Equal(t, "open", invoices[0].Status)
	require.Equal(t, int64(500), invoices[0].AmountDue)

	ch := &fakeCharger{}
	n, err = svc.ChargeOutstanding(ctx, ch, 0)
	require.NoError(t, err)
	require.Equal(t, 1, n)
	require.Len(t, ch.charges, 1)

	paid, err := svc.GetInvoiceByID(ctx, payer, invoices[0].ID)
	require.NoError(t, err)
	require.Equal(t, "paid", paid.Status)
	require.Equal(t, int64(0), paid.AmountDue)
}

func TestRecordOutOfBandInvoicePayment_PartialThenPaid(t *testing.T) {
	svc, pool, payer, _, ctx := moneyInEnv(t)
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.invoice_payments WHERE customer_id = $1", payer.UUID())
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.invoice_items WHERE customer_id = $1", payer.UUID())
		_, _ = pool.Exec(ctx, "UPDATE openrails.money_transactions SET invoice_id = NULL WHERE customer_id = $1", payer.UUID())
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.usage_events WHERE customer_id = $1", payer.UUID())
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.invoices WHERE customer_id = $1", payer.UUID())
	})
	_, err := svc.UpsertAccountSettings(ctx, payer, money.DefaultCurrency, money.AccountSettingsInput{BillingMode: strptr(money.BillingModeArrears)})
	require.NoError(t, err)
	_, err = svc.AccrueOwed(ctx, payer, money.DefaultCurrency, "usage", "manual-payment-invoice", 500)
	require.NoError(t, err)
	inv, err := svc.FinalizeInvoice(ctx, payer, money.DefaultCurrency, time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	require.NoError(t, err)
	require.Equal(t, "open", inv.Status)
	require.Equal(t, int64(500), inv.AmountDue)

	partial, err := svc.RecordOutOfBandInvoicePayment(ctx, payer, inv.ID, 300, "bank-transfer-1")
	require.NoError(t, err)
	require.Equal(t, "open", partial.Status)
	require.Equal(t, int64(300), partial.AmountPaid)
	require.Equal(t, int64(200), partial.AmountDue)

	_, err = svc.RecordOutOfBandInvoicePayment(ctx, payer, inv.ID, 1, "bank-transfer-1")
	require.Error(t, err, "duplicate manual payment reference must not apply twice")
	afterDup, err := svc.GetInvoiceByID(ctx, payer, inv.ID)
	require.NoError(t, err)
	require.Equal(t, int64(300), afterDup.AmountPaid)
	require.Equal(t, int64(200), afterDup.AmountDue)

	paid, err := svc.RecordOutOfBandInvoicePayment(ctx, payer, inv.ID, 200, "bank-transfer-2")
	require.NoError(t, err)
	require.Equal(t, "paid", paid.Status)
	require.Equal(t, int64(500), paid.AmountPaid)
	require.Equal(t, int64(0), paid.AmountDue)
	require.NotNil(t, paid.PaidAt)

	var paymentCount int
	var paymentSum int64
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT count(*), COALESCE(SUM(amount), 0)
		FROM openrails.invoice_payments
		WHERE invoice_id = $1 AND status = 'settled' AND processor = 'manual'
	`, inv.ID).Scan(&paymentCount, &paymentSum))
	require.Equal(t, 2, paymentCount)
	require.Equal(t, int64(500), paymentSum)

	var ledgerCount int
	var ledgerSum int64
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT count(*), COALESCE(SUM(amount), 0)
		FROM openrails.ledger_transfers
		WHERE invoice_id = $1 AND transfer_type = 'owed_payment'
	`, inv.ID).Scan(&ledgerCount, &ledgerSum))
	require.Equal(t, 2, ledgerCount)
	require.Equal(t, int64(500), ledgerSum) // #512 ledger amounts are positive
}

func TestInvoiceVoidAndUncollectibleLifecycle(t *testing.T) {
	svc, pool, payer, _, ctx := moneyInEnv(t)
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.invoice_payments WHERE customer_id = $1", payer.UUID())
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.invoice_items WHERE customer_id = $1", payer.UUID())
		_, _ = pool.Exec(ctx, "UPDATE openrails.money_transactions SET invoice_id = NULL WHERE customer_id = $1", payer.UUID())
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.usage_events WHERE customer_id = $1", payer.UUID())
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.invoices WHERE customer_id = $1", payer.UUID())
	})
	_, err := svc.UpsertAccountSettings(ctx, payer, money.DefaultCurrency, money.AccountSettingsInput{BillingMode: strptr(money.BillingModeArrears)})
	require.NoError(t, err)
	require.NoError(t, svc.SetCreditLimit(ctx, payer, money.DefaultCurrency, 500))
	_, err = svc.AccrueOwed(ctx, payer, money.DefaultCurrency, "usage", "voided-invoice", 500)
	require.NoError(t, err)
	inv, err := svc.FinalizeInvoice(ctx, payer, money.DefaultCurrency, time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	require.NoError(t, err)

	voided, err := svc.VoidInvoice(ctx, payer, inv.ID)
	require.NoError(t, err)
	require.Equal(t, "voided", voided.Status)
	require.Equal(t, int64(0), voided.AmountDue)
	require.NotNil(t, voided.VoidedAt)
	owed, err := svc.GetOutstandingOwed(ctx, payer, money.DefaultCurrency)
	require.NoError(t, err)
	require.Equal(t, int64(0), owed, "void neutralizes the transition owed projection")

	svc2, pool2, payer2, _, ctx2 := moneyInEnv(t)
	t.Cleanup(func() {
		_, _ = pool2.Exec(ctx2, "DELETE FROM openrails.invoice_payments WHERE customer_id = $1", payer2.UUID())
		_, _ = pool2.Exec(ctx2, "DELETE FROM openrails.invoice_items WHERE customer_id = $1", payer2.UUID())
		_, _ = pool2.Exec(ctx2, "UPDATE openrails.money_transactions SET invoice_id = NULL WHERE customer_id = $1", payer2.UUID())
		_, _ = pool2.Exec(ctx2, "DELETE FROM openrails.usage_events WHERE customer_id = $1", payer2.UUID())
		_, _ = pool2.Exec(ctx2, "DELETE FROM openrails.invoices WHERE customer_id = $1", payer2.UUID())
	})
	_, err = svc2.UpsertAccountSettings(ctx2, payer2, money.DefaultCurrency, money.AccountSettingsInput{BillingMode: strptr(money.BillingModeArrears)})
	require.NoError(t, err)
	_, err = svc2.AccrueOwed(ctx2, payer2, money.DefaultCurrency, "usage", "uncollectible-invoice", 400)
	require.NoError(t, err)
	inv2, err := svc2.FinalizeInvoice(ctx2, payer2, money.DefaultCurrency, time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	require.NoError(t, err)
	uncollectible, err := svc2.MarkInvoiceUncollectible(ctx2, payer2, inv2.ID)
	require.NoError(t, err)
	require.Equal(t, "uncollectible", uncollectible.Status)
	require.Equal(t, int64(400), uncollectible.AmountDue)
	require.NotNil(t, uncollectible.UncollectibleAt)
}

// FinalizeDueInvoices enumerates the merchant's payers and finalizes each one's
// invoice for the period (#472). The deposit above creates the payer's
// money_accounts row, so enumeration must find and finalize it.
func TestFinalizeDueInvoices_EnumeratesAccount(t *testing.T) {
	svc, pool, payer, _, ctx := moneyInEnv(t)
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.invoice_items WHERE customer_id = $1", payer.UUID())
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.usage_events WHERE customer_id = $1", payer.UUID())
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.invoices WHERE customer_id = $1", payer.UUID())
	})
	_, err := svc.Deposit(ctx, money.DepositParams{CustomerID: &payer, Invoker: payer.UUID().String(), Currency: money.DefaultCurrency, Amount: 5_000, Source: "seed"})
	require.NoError(t, err)
	_, err = svc.RecordUsage(ctx, money.RecordUsageParams{Payer: &payer, Invoker: "u", Currency: money.DefaultCurrency, EventType: "gpt-4o", Amount: 2_000, Source: "req", SourceID: "r1"})
	require.NoError(t, err)

	from, to := time.Now().Add(-time.Hour), time.Now().Add(time.Hour)
	n, err := svc.FinalizeDueInvoices(ctx, from, to)
	require.NoError(t, err)
	require.GreaterOrEqual(t, n, 1, "the depositing payer must be enumerated and finalized")

	// the payer's invoice for the period is now persisted
	inv, err := svc.FinalizeInvoice(ctx, payer, money.DefaultCurrency, from, to)
	require.NoError(t, err)
	require.Equal(t, int64(2_000), inv.UsageTotal)
	require.Equal(t, "paid", inv.Status)
}
