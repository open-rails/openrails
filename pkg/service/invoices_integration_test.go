//go:build integration

package service_test

import (
	"testing"
	"time"

	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/stretchr/testify/require"
)

// TestListInvoices_Statement proves the facade methods backing
// GET /v1/me/invoices and GET /v1/me/invoices/:id (issue #303): after
// FinalizeInvoice rolls up a window into a statement, ListInvoices returns it for
// the payer (paginated, newest first) and GetInvoice returns it with its line
// items. Mirrors TestGetUsage_Breakdown — it exercises the public service facade
// the HTTP handlers call, not the money package directly.
func TestListInvoices_Statement(t *testing.T) {
	svc, ms, payer, ctx := authzEnv(t)

	// Separate pool used only to clean up the usage_events and invoices rows
	// this test writes, scoped by payer. authzEnv cleans up the ledger/balance
	// rows but not these.
	pool := testPool(t)
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.usage_events WHERE customer_id = $1", payer.UUID())
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.invoices WHERE customer_id = $1", payer.UUID())
	})

	_, err := ms.Deposit(ctx, money.DepositParams{
		CustomerID: &payer, Invoker: payer.UUID().String(), Currency: money.DefaultCurrency, Amount: 100_000, Source: "purchase",
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

	from, to := time.Now().Add(-time.Hour), time.Now().Add(time.Hour)
	finalized, err := ms.FinalizeInvoice(ctx, payer, money.DefaultCurrency, from, to)
	require.NoError(t, err)
	require.Equal(t, "paid", finalized.Status)

	// ListInvoices returns the payer's invoice (newest first, paginated).
	list, total, err := svc.ListInvoices(ctx, payer, 50, 0)
	require.NoError(t, err)
	require.Equal(t, 1, total)
	require.Len(t, list, 1)
	require.Equal(t, finalized.ID, list[0].ID)
	require.Equal(t, int64(9_000), list[0].UsageTotal)
	require.Equal(t, int64(100_000), list[0].DepositsTotal)

	// GetInvoiceByID returns the same invoice WITH its snapshotted line items.
	got, err := svc.GetInvoice(ctx, payer, finalized.ID)
	require.NoError(t, err)
	require.Equal(t, finalized.ID, got.ID)
	require.Len(t, got.LineItems, 2)

	byType := map[string]int{}
	for i, li := range got.LineItems {
		byType[li.EventType] = i
	}
	gpt := got.LineItems[byType["gpt-4o"]]
	require.Equal(t, int64(8_000), gpt.Amount)
	require.Equal(t, int64(2), gpt.Count)
	require.Equal(t, int64(160), gpt.Dimensions["input_tokens"])
	require.Equal(t, int64(80), gpt.Dimensions["output_tokens"])

	emb := got.LineItems[byType["embeddings"]]
	require.Equal(t, int64(1_000), emb.Amount)
	require.Equal(t, int64(1), emb.Count)
	require.Equal(t, int64(200), emb.Dimensions["input_tokens"])

	// Pagination: offset past the only row yields an empty page but the same total.
	page2, total2, err := svc.ListInvoices(ctx, payer, 50, 1)
	require.NoError(t, err)
	require.Equal(t, 1, total2)
	require.Empty(t, page2)
}
