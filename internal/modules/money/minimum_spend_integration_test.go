//go:build integration

package money_test

import (
	"testing"
	"time"

	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/stretchr/testify/require"
)

// A full-period close trues-up a short-usage customer to their committed
// minimum_spend (#643): 3_000 of rated usage against a 10_000 commitment yields a
// 7_000 true-up line, amount_due 10_000, and a consistent owed ledger. Idempotent
// across re-finalize.
func TestFinalizeInvoice_MinimumSpendTrueUp(t *testing.T) {
	svc, pool, payer, _, ctx := moneyInEnv(t)
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.invoice_items WHERE customer_id = $1", payer.UUID())
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.invoices WHERE customer_id = $1", payer.UUID())
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.customer_minimum_spend WHERE customer_id = $1", payer.UUID())
	})

	_, err := svc.AccrueOwed(ctx, payer, money.DefaultCurrency, "metered:test", "r1", 3_000)
	require.NoError(t, err)
	require.NoError(t, svc.SetCustomerMinimumSpend(ctx, payer, money.DefaultCurrency, 10_000))

	got, err := svc.GetCustomerMinimumSpend(ctx, payer, money.DefaultCurrency)
	require.NoError(t, err)
	require.Equal(t, int64(10_000), got)

	from, to := time.Now().Add(-time.Hour), time.Now().Add(time.Hour)
	inv, err := svc.FinalizeInvoice(ctx, payer, money.DefaultCurrency, from, to, money.WithMinimumSpendTrueUp())
	require.NoError(t, err)
	require.Equal(t, int64(10_000), inv.TotalAmount)
	require.Equal(t, int64(10_000), inv.AmountDue, "short usage trues-up to the minimum")
	require.Equal(t, int64(10_000), inv.OwedAccrued, "true-up is a real owed accrual")
	require.Equal(t, "open", inv.Status)

	// #726: the true-up is a distinct line of 7_000 (10_000 - 3_000) on the
	// frozen statement (invoices.line_items), not an invoice_items row.
	trueUpLine := findItem(inv.LineItems, "minimum_spend_trueup")
	require.NotNil(t, trueUpLine)
	require.Equal(t, int64(7_000), trueUpLine.Amount)

	// The workspace only holds the attached accrual — no invoiced-copy rows.
	var attachedCount int
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT count(*) FROM openrails.invoice_items WHERE invoice_id = $1`, inv.ID).Scan(&attachedCount))
	require.Equal(t, 1, attachedCount, "#726: only the pending accrual is attached at close")

	// The owed ledger reflects the full 10_000 liability (usage + true-up).
	outstanding, err := svc.GetOutstandingOwed(ctx, payer, money.DefaultCurrency)
	require.NoError(t, err)
	require.Equal(t, int64(10_000), outstanding)

	// Idempotent: re-finalize returns the same frozen statement, no second true-up.
	inv2, err := svc.FinalizeInvoice(ctx, payer, money.DefaultCurrency, from, to, money.WithMinimumSpendTrueUp())
	require.NoError(t, err)
	require.Equal(t, inv.ID, inv2.ID)
	trueUps := 0
	for _, li := range inv2.LineItems {
		if li.EventType == "minimum_spend_trueup" {
			trueUps++
		}
	}
	require.Equal(t, 1, trueUps, "re-finalize does not double the true-up")
}

// Without WithMinimumSpendTrueUp (e.g. a threshold mid-period close) the invoice
// bills actual usage — a commitment true-up is meaningless mid-period (#643).
func TestFinalizeInvoice_MinimumSpendSkippedWithoutOption(t *testing.T) {
	svc, pool, payer, _, ctx := moneyInEnv(t)
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.invoice_items WHERE customer_id = $1", payer.UUID())
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.invoices WHERE customer_id = $1", payer.UUID())
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.customer_minimum_spend WHERE customer_id = $1", payer.UUID())
	})

	_, err := svc.AccrueOwed(ctx, payer, money.DefaultCurrency, "metered:test", "r1", 3_000)
	require.NoError(t, err)
	require.NoError(t, svc.SetCustomerMinimumSpend(ctx, payer, money.DefaultCurrency, 10_000))

	from, to := time.Now().Add(-time.Hour), time.Now().Add(time.Hour)
	inv, err := svc.FinalizeInvoice(ctx, payer, money.DefaultCurrency, from, to)
	require.NoError(t, err)
	require.Equal(t, int64(3_000), inv.AmountDue, "no option -> actual usage, no true-up")
	require.Nil(t, findItem(inv.LineItems, "minimum_spend_trueup"))
}

// When rated usage already meets or exceeds the commitment, the true-up is a
// no-op even with the option on (#643).
func TestFinalizeInvoice_MinimumSpendNoTrueUpWhenUsageMeetsMinimum(t *testing.T) {
	svc, pool, payer, _, ctx := moneyInEnv(t)
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.invoice_items WHERE customer_id = $1", payer.UUID())
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.invoices WHERE customer_id = $1", payer.UUID())
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.customer_minimum_spend WHERE customer_id = $1", payer.UUID())
	})

	_, err := svc.AccrueOwed(ctx, payer, money.DefaultCurrency, "metered:test", "r1", 8_000)
	require.NoError(t, err)
	require.NoError(t, svc.SetCustomerMinimumSpend(ctx, payer, money.DefaultCurrency, 5_000))

	from, to := time.Now().Add(-time.Hour), time.Now().Add(time.Hour)
	inv, err := svc.FinalizeInvoice(ctx, payer, money.DefaultCurrency, from, to, money.WithMinimumSpendTrueUp())
	require.NoError(t, err)
	require.Equal(t, int64(8_000), inv.AmountDue, "usage above the minimum is billed as-is")
	require.Nil(t, findItem(inv.LineItems, "minimum_spend_trueup"))
}
