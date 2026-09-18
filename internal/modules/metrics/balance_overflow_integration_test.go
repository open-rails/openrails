//go:build integration

package metrics_test

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/modules/metrics"
	"github.com/open-rails/openrails/pkg/merchant"
)

// Each account and each SQL time bucket fits int64, but their running merchant
// liability need not. A time series must refuse the overflow just as the
// point-in-time SQL aggregate does; wrapping it into negative money is wrong.
func TestMetrics_BalanceSeriesRefusesOverflow(t *testing.T) {
	ctx := context.Background()
	dbi := dbtest.OpenAppDB(t, dbtest.SharedPostgresDSN(t))
	pool := dbtest.SharedSuperuserPGXPool(t)
	merchantID := uuid.New()
	exec(ctx, t, pool, `INSERT INTO openrails.merchants (id, slug, status) VALUES ($1, $2, 'active')`, merchantID, "metrics-overflow-"+merchantID.String())
	for i, source := range []string{"processor_clearing", "world"} {
		customer, debit, credit := uuid.New(), uuid.New(), uuid.New()
		exec(ctx, t, pool, `INSERT INTO openrails.customers (id, merchant_id) VALUES ($1, $2)`, customer, merchantID)
		exec(ctx, t, pool, `INSERT INTO openrails.ledger_accounts (id, merchant_id, account_type, currency) VALUES ($1, $2, $3, 'USD')`, debit, merchantID, source)
		exec(ctx, t, pool, `INSERT INTO openrails.ledger_accounts (id, merchant_id, customer_id, account_type, currency, debits_must_not_exceed_credits) VALUES ($1, $2, $3, 'customer_balance', 'USD', true)`, credit, merchantID, customer)
		id := uuid.New()
		exec(ctx, t, pool, `INSERT INTO openrails.ledger_transfers (id, merchant_id, debit_account_id, credit_account_id, amount, currency, transfer_type, customer_id, created_at, operation, source, source_id) VALUES ($1, $2, $3, $4, $5, 'USD', 'deposit', $6, $7, 'deposit', 'metrics-overflow', $1::uuid::text)`, id, merchantID, debit, credit, int64(math.MaxInt64/2+1), customer, time.Date(2026, time.Month(5+i), 15, 0, 0, 0, 0, time.UTC))
	}
	svc := metrics.NewService(dbi)
	ctx = merchant.WithID(ctx, merchant.ID(merchantID))
	query := &metrics.Query{Measures: []string{"outstanding_credit_liability"}, By: []string{"time"}, Grain: "month", Range: &metrics.QueryRange{From: "2026-06-01", To: "2026-08-01"}, Filters: map[string][]string{"currency": {"USD"}}}
	plan, validationErr := metrics.Validate(query)
	require.Nil(t, validationErr)
	result, err := svc.Execute(ctx, plan)
	require.ErrorContains(t, err, "does not fit int64", "out-of-range running liability must not succeed: %+v", result)
	require.Nil(t, result)

	// The June start is still representable. Its later June delta does not
	// contribute to that snapshot, so it must not cause a spurious refusal.
	query.Range.To = "2026-07-01T00:00:00Z"
	plan, validationErr = metrics.Validate(query)
	require.Nil(t, validationErr)
	result, err = svc.Execute(ctx, plan)
	require.NoError(t, err)
	require.Len(t, result.Rows, 1)
	require.Equal(t, metrics.MoneyCell(math.MaxInt64/2+1), result.Rows[0][1])
}
