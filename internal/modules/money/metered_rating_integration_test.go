//go:build integration

package money_test

import (
	"testing"
	"time"

	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/stretchr/testify/require"
)

func TestAccrueMeteredAggregate_WritesInvoiceableOwedRows(t *testing.T) {
	svc, pool, payer, cur, ctx := moneyInEnv(t)
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.invoice_items WHERE customer_id = $1", payer.UUID())
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.invoices WHERE customer_id = $1", payer.UUID())
	})

	_, err := svc.UpsertAccountSettings(ctx, payer, money.DefaultCurrency, money.AccountSettingsInput{
		BillingMode: strptr(money.BillingModeArrears),
	})
	require.NoError(t, err)

	rate := money.MeteredRate{
		Kind:       money.MeterKindCounter,
		RateMicros: 100_000,
		PerUnits:   100,
	}
	got, err := svc.AccrueMeteredAggregate(ctx, payer, cur, "api-calls", "period-2026-06", 1250, rate)
	require.NoError(t, err)
	require.Equal(t, int64(1_250_000), got)

	var pendingCount int
	var pendingTotal int64
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT count(*), COALESCE(sum(amount), 0)::bigint
		FROM openrails.invoice_items
		WHERE customer_id = $1
		  AND currency = $2
		  AND source_type = 'owed_accrual'
		  AND source_id = 'metered:api-calls:period-2026-06'
		  AND invoice_id IS NULL
		  AND status = 'pending'
	`, payer.UUID(), cur).Scan(&pendingCount, &pendingTotal))
	require.Equal(t, 1, pendingCount)
	require.Equal(t, int64(1_250_000), pendingTotal)

	// Same meter/source period is idempotent in the durable DB rows.
	got, err = svc.AccrueMeteredAggregate(ctx, payer, cur, "api-calls", "period-2026-06", 1250, rate)
	require.NoError(t, err)
	require.Equal(t, int64(1_250_000), got)
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT count(*), COALESCE(sum(amount), 0)::bigint
		FROM openrails.invoice_items
		WHERE customer_id = $1
		  AND source_type = 'owed_accrual'
		  AND source_id = 'metered:api-calls:period-2026-06'
	`, payer.UUID()).Scan(&pendingCount, &pendingTotal))
	require.Equal(t, 1, pendingCount)
	require.Equal(t, int64(1_250_000), pendingTotal)

	inv, err := svc.FinalizeInvoice(ctx, payer, cur, time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	require.NoError(t, err)
	require.Equal(t, "open", inv.Status)
	require.Equal(t, int64(1_250_000), inv.OwedAccrued)
	require.Equal(t, int64(1_250_000), inv.AmountDue)

	var invoicedCount int
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT count(*)
		FROM openrails.invoice_items
		WHERE customer_id = $1
		  AND invoice_id = $2
		  AND status = 'invoiced'
		  AND source_id = 'metered:api-calls:period-2026-06'
	`, payer.UUID(), inv.ID).Scan(&invoicedCount))
	require.Equal(t, 1, invoicedCount)
}
