//go:build integration

package service_test

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/modules/money"
	billingservice "github.com/open-rails/openrails/pkg/service"
	"github.com/stretchr/testify/require"
)

// TestGetUsage_Breakdown proves the facade method backing GET /v1/self/usage
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
		Amount:     5_000, Source: "req", SourceID: "u1",
	})
	require.NoError(t, err)
	_, err = ms.RecordUsage(ctx, money.RecordUsageParams{
		Payer: &payer, Invoker: "user:a", Currency: money.DefaultCurrency, EventType: "gpt-4o",
		Dimensions: map[string]int64{"input_tokens": 60, "output_tokens": 30},
		Amount:     3_000, Source: "req", SourceID: "u2",
	})
	require.NoError(t, err)
	_, err = ms.RecordUsage(ctx, money.RecordUsageParams{
		Payer: &payer, Invoker: "user:a", Currency: money.DefaultCurrency, EventType: "embeddings",
		Dimensions: map[string]int64{"input_tokens": 200},
		Amount:     1_000, Source: "req", SourceID: "u3",
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

func TestCaptureHold_WritesIdempotentUsageEventAndServiceRollup(t *testing.T) {
	svc, ms, payer, ctx := authzEnv(t)

	pool := testPool(t)
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.usage_events WHERE customer_id = $1", payer.UUID())
	})

	_, err := ms.Deposit(ctx, money.DepositParams{
		CustomerID: &payer,
		Invoker:    payer.UUID().String(),
		Currency:   money.DefaultCurrency,
		Amount:     10_000,
		Source:     "seed",
	})
	require.NoError(t, err)

	capture := func(amount int64, holdSource, usageSourceID, invoker, resource string, metadata map[string]any) {
		t.Helper()
		hold, err := svc.HoldCredits(ctx, billingservice.HoldCreditsRequest{
			CustomerID: &payer,
			Invoker:    invoker,
			Amount:     amount,
			Source:     "hold",
			SourceID:   holdSource,
			ExpiresAt:  time.Now().Add(time.Hour).UTC(),
		})
		require.NoError(t, err)
		_, err = svc.CaptureHold(ctx, billingservice.CaptureHoldRequest{
			HoldID:    hold.ID,
			Amount:    amount,
			EventType: "endpoint.capture",
			Resource:  resource,
			Metadata:  metadata,
			Source:    "capture",
			SourceID:  usageSourceID,
		})
		require.NoError(t, err)
	}

	capture(1_000, uuid.NewString(), "usage-replay", "u1", "ep-a", map[string]any{
		"function_name":     "txt2img",
		"availability_tier": "fast",
	})
	capture(2_000, uuid.NewString(), "usage-replay", "u1", "ep-a", map[string]any{
		"function_name":     "txt2img",
		"availability_tier": "fast",
	})
	capture(3_000, uuid.NewString(), "usage-unique", "u2", "ep-b", map[string]any{
		"function_name":     "img2img",
		"availability_tier": "batch",
	})

	account, err := svc.GetCreditAccount(ctx, payer, money.DefaultCurrency)
	require.NoError(t, err)
	require.Equal(t, int64(4_000), account.BalanceAmount, "three captures debit exactly their captured amounts")

	var usageEventCount int
	require.NoError(t, pool.QueryRow(ctx,
		"SELECT count(*) FROM openrails.usage_events WHERE customer_id = $1 AND event_type = $2",
		payer.UUID(), "endpoint.capture",
	).Scan(&usageEventCount))
	require.Equal(t, 2, usageEventCount, "duplicate usage source_id is idempotent")

	var captureCount int
	require.NoError(t, pool.QueryRow(ctx,
		"SELECT count(*) FROM openrails.money_transactions WHERE customer_id = $1 AND transaction_type = $2 AND status = $3",
		payer.UUID(), "hold", "captured",
	).Scan(&captureCount))
	require.Equal(t, 3, captureCount)

	var ledgerCount int
	require.NoError(t, pool.QueryRow(ctx,
		"SELECT count(*) FROM openrails.money_transactions WHERE customer_id = $1",
		payer.UUID(),
	).Scan(&ledgerCount))
	require.Equal(t, 4, ledgerCount, "usage analytics must not create extra ledger debits")

	assertRollup := func(groupBy string, want map[string]int64) {
		t.Helper()
		rows, err := svc.ServiceUsageRollup(ctx, billingservice.ServiceUsageRollupRequest{
			CustomerID: &payer,
			From:       time.Now().Add(-time.Hour),
			To:         time.Now().Add(time.Hour),
			GroupBy:    groupBy,
		})
		require.NoError(t, err)
		got := make(map[string]int64, len(rows))
		for _, row := range rows {
			got[row.Key] = row.TotalAmount
		}
		require.Equal(t, want, got)
	}

	assertRollup("resource", map[string]int64{"ep-a": 1_000, "ep-b": 3_000})
	assertRollup("function", map[string]int64{"txt2img": 1_000, "img2img": 3_000})
	assertRollup("tier", map[string]int64{"fast": 1_000, "batch": 3_000})
	assertRollup("invoker", map[string]int64{"u1": 1_000, "u2": 3_000})
}
