//go:build integration

package money_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/modules/merchantconfig"
	"github.com/open-rails/openrails/internal/modules/money"
	riverjobs "github.com/open-rails/openrails/internal/river"
	"github.com/open-rails/openrails/pkg/identity"
	"github.com/open-rails/openrails/pkg/pricing"
	"github.com/riverqueue/river"
	"github.com/stretchr/testify/require"
)

// A usage-only payer — the OpenRails-SaaS platform fee shape: zero-amount
// catalog usage priced by a rate card, never a deposit, spend or accrual before
// rating — has no ledger row until FinalizeInvoice rates it. The production
// monthly sweep must still enumerate that payer and issue the rated invoice.
func TestInvoiceWorker_SweepInvoicesUsageOnlyPayer(t *testing.T) {
	svc, dbi, pool, payer, cur, ctx := moneyInEnvWithDB(t)
	merchantID := dbtest.TestMerchantID.UUID()
	ledgerPayer := identity.CustomerIDFromString(uuid.NewString())
	productID := uuid.New()
	meter := "payment-volume-" + uuid.NewString()[:8]
	eventType := "platform.payment_volume." + uuid.NewString()[:8]
	t.Cleanup(func() {
		for _, p := range []uuid.UUID{payer.UUID(), ledgerPayer.UUID()} {
			_, _ = pool.Exec(ctx, "DELETE FROM openrails.invoice_items WHERE customer_id = $1", p)
			_, _ = pool.Exec(ctx, "DELETE FROM openrails.invoices WHERE customer_id = $1", p)
			_, _ = pool.Exec(ctx, "DELETE FROM openrails.usage_events WHERE customer_id = $1", p)
			_, _ = pool.Exec(ctx, "DELETE FROM openrails.metered_rating_watermarks WHERE customer_id = $1", p)
			_, _ = pool.Exec(ctx, "DELETE FROM openrails.money_settings WHERE customer_id = $1", p)
		}
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.catalog_rate_cards WHERE merchant_id = $1 AND meter_key = $2", merchantID, meter)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.catalog_meters WHERE merchant_id = $1 AND key = $2", merchantID, meter)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.products WHERE id = $1", productID)
	})

	// Calendar-month periods so the window under test is exact; restore the
	// merchant's own configuration afterwards.
	store := merchantconfig.NewStore(dbi)
	previousCfg, _, err := store.Get(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Upsert(ctx, previousCfg) })
	cfg := previousCfg
	cfg.InvoiceBillingBoundary = money.InvoiceBoundaryCalendarMonth
	require.NoError(t, store.Upsert(ctx, cfg))

	_, err = pool.Exec(ctx, `INSERT INTO openrails.products (id, key, display_name, merchant_id) VALUES ($1, $2, 'Payment volume', $3)`,
		productID, "volume-"+uuid.NewString()[:8], merchantID)
	require.NoError(t, err)
	require.NoError(t, svc.EnsureUsageMeter(ctx, money.UsageMeterSpec{
		Key: meter, EventType: eventType, ValueProperty: "amount_micros", Aggregation: "sum", Unit: "currency_micros",
	}))
	// 100 bps of settled volume, the SaaS take-rate shape.
	require.NoError(t, svc.SetUsageRateCard(ctx, money.UsageRateCardInput{
		ProductID: &productID, MeterKey: meter,
		Price: pricing.RatePrice{Model: pricing.ModelPerUnit, Currency: cur,
			PerUnit: &pricing.PerUnitPrice{UnitAmount: 100, DivideBy: 10_000}},
	}))

	now := time.Date(2026, time.September, 16, 12, 0, 0, 0, time.UTC)
	periodFrom := time.Date(2026, time.August, 1, 0, 0, 0, 0, time.UTC)
	periodTo := time.Date(2026, time.September, 1, 0, 0, 0, 0, time.UTC)
	const settled = int64(17_000_000)
	for _, occurred := range []time.Time{periodFrom.Add(3 * 24 * time.Hour), periodTo.Add(-time.Hour)} {
		_, err = svc.RecordUsage(ctx, money.RecordUsageParams{
			Payer: &payer, Invoker: payer.String(), Currency: cur, EventType: eventType,
			Dimensions: map[string]int64{"amount_micros": settled},
			Amount:     0, Key: money.MustIdempotencyKey(money.UsageOperation(eventType), "saas:payment-settlement", uuid.NewString()),
			OccurredAt: occurred,
		})
		require.NoError(t, err)
	}
	// A ledger-only payer proves the money-activity enumeration is unchanged.
	depositID := uuid.NewString()
	_, err = svc.Deposit(ctx, money.DepositParams{
		CustomerID: &ledgerPayer, Invoker: ledgerPayer.String(), Currency: cur, Amount: 1_000_000,
		Source: "sweep-test", SourceID: &depositID,
	})
	require.NoError(t, err)

	var ledgerRows int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM openrails.ledger_transfers WHERE customer_id = $1`, payer.UUID()).Scan(&ledgerRows))
	require.Zero(t, ledgerRows, "precondition: a usage-only payer has no ledger row before rating")

	worker := riverjobs.InvoiceWorker{DB: dbi, Money: svc, Clock: clockwork.NewFakeClockAt(now)}
	sweep := func() {
		t.Helper()
		require.NoError(t, worker.Work(context.Background(), &river.Job[riverjobs.InvoiceArgs]{
			Args: riverjobs.InvoiceArgs{FinalizePreviousMonth: true},
		}))
	}
	sweep()

	invoices, total, err := svc.ListInvoices(ctx, payer, 10, 0)
	require.NoError(t, err)
	require.Equal(t, 1, total, "the sweep must invoice a payer whose only activity is unrated catalog usage")
	inv := invoices[0]
	require.True(t, inv.PeriodFrom.Equal(periodFrom) && inv.PeriodTo.Equal(periodTo), "period %v..%v", inv.PeriodFrom, inv.PeriodTo)
	// 2 x 17_000_000 micros x 100 / 10_000 = 340_000 micros, exact.
	const fee = int64(340_000)
	require.Equal(t, "open", inv.Status)
	require.Equal(t, fee, inv.AmountDue)
	require.Equal(t, fee, inv.TotalAmount)
	require.Equal(t, fee, inv.SubtotalAmount)
	require.Equal(t, models.InvoiceLineItem{EventType: "metered:" + meter, Amount: fee, Count: 1}, ratedLine(t, inv, "metered:"+meter))
	usage := ratedLine(t, inv, eventType)
	require.Equal(t, int64(2), usage.Count)
	require.Equal(t, 2*settled, usage.Dimensions["amount_micros"])

	_, ledgerTotal, err := svc.ListInvoices(ctx, ledgerPayer, 10, 0)
	require.NoError(t, err)
	require.Equal(t, 1, ledgerTotal, "ledger-activity payers are still enumerated")

	// Re-sweep: same invoice, nothing rated or accrued twice.
	sweep()
	_, total, err = svc.ListInvoices(ctx, payer, 10, 0)
	require.NoError(t, err)
	require.Equal(t, 1, total)
	var accrued int64
	require.NoError(t, pool.QueryRow(ctx, `SELECT COALESCE(SUM(amount), 0) FROM openrails.ledger_transfers
		WHERE customer_id = $1 AND transfer_type = 'owed_accrual'`, payer.UUID()).Scan(&accrued))
	require.Equal(t, fee, accrued, "exactly-once rating across sweeps")
	var pending int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM openrails.invoice_items
		WHERE customer_id = $1 AND invoice_id IS NULL`, payer.UUID()).Scan(&pending))
	require.Zero(t, pending, "the rated accrual is attached to the invoice")
}

func ratedLine(t *testing.T, inv models.Invoice, eventType string) models.InvoiceLineItem {
	t.Helper()
	for _, li := range inv.LineItems {
		if li.EventType == eventType {
			return li
		}
	}
	t.Fatalf("invoice %s has no %q line item: %+v", inv.ID, eventType, inv.LineItems)
	return models.InvoiceLineItem{}
}
