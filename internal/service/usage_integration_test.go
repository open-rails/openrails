//go:build integration

package service_test

import (
	"testing"
	"time"

	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/stretchr/testify/require"
)

// TestGetUsage_Breakdown proves the facade method backing GET /v1/me/usage
// returns the per-event_type rollup (summed amount, event count, summed
// dimensions) for a seeded payer over a window (issue #289). It mirrors the
// money-package usage rollup test but exercises the public service facade the
// HTTP handler calls.
func TestGetUsage_Breakdown(t *testing.T) {
	svc, ms, payer, ctx := authzEnv(t)

	// Separate pool used only to clean up the usage_events rows this test
	// writes, scoped by payer.
	pool := testPool(t)
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.usage_events WHERE customer_id = $1", payer.UUID())
	})

	_, err := ms.Deposit(ctx, money.DepositParams{
		CustomerID: &payer, Invoker: payer.UUID().String(), Currency: money.DefaultCurrency, Amount: 100_000, Source: "seed",
	})
	require.NoError(t, err)

	_, err = ms.RecordUsage(ctx, money.RecordUsageParams{
		Payer: &payer, Invoker: "user:a", Currency: money.DefaultCurrency, EventType: "gpt-4o",
		Dimensions: map[string]int64{"input_tokens": 100, "output_tokens": 50},
		Amount:     5_000, Key: money.MustIdempotencyKey(money.UsageOperation("gpt-4o"), "req", "u1"),
	})
	require.NoError(t, err)
	_, err = ms.RecordUsage(ctx, money.RecordUsageParams{
		Payer: &payer, Invoker: "user:a", Currency: money.DefaultCurrency, EventType: "gpt-4o",
		Dimensions: map[string]int64{"input_tokens": 60, "output_tokens": 30},
		Amount:     3_000, Key: money.MustIdempotencyKey(money.UsageOperation("gpt-4o"), "req", "u2"),
	})
	require.NoError(t, err)
	_, err = ms.RecordUsage(ctx, money.RecordUsageParams{
		Payer: &payer, Invoker: "user:a", Currency: money.DefaultCurrency, EventType: "embeddings",
		Dimensions: map[string]int64{"input_tokens": 200},
		Amount:     1_000, Key: money.MustIdempotencyKey(money.UsageOperation("embeddings"), "req", "u3"),
	})
	require.NoError(t, err)

	rows, err := svc.GetUsage(ctx, payer, money.DefaultCurrency, time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	require.NoError(t, err)
	require.Len(t, rows, 2)

	byType := map[string]int{}
	for i, r := range rows {
		byType[r.EventType] = i
	}

	g := rows[byType["gpt-4o"]]
	require.Equal(t, money.DefaultCurrency, g.Currency)
	require.Equal(t, int64(8_000), g.TotalAmount)
	require.Equal(t, int64(2), g.EventCount)
	require.Equal(t, int64(160), g.Dimensions["input_tokens"])
	require.Equal(t, int64(80), g.Dimensions["output_tokens"])

	e := rows[byType["embeddings"]]
	require.Equal(t, money.DefaultCurrency, e.Currency)
	require.Equal(t, int64(1_000), e.TotalAmount)
	require.Equal(t, int64(1), e.EventCount)
	require.Equal(t, int64(200), e.Dimensions["input_tokens"])

	// A window before any usage returns no rows.
	empty, err := svc.GetUsage(ctx, payer, money.DefaultCurrency, time.Now().Add(-72*time.Hour), time.Now().Add(-48*time.Hour))
	require.NoError(t, err)
	require.Empty(t, empty)
}
