//go:build integration

package tests

import (
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	identity "github.com/open-rails/openrails/internal/billingidentity"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/permissions"
	"github.com/open-rails/openrails/pkg/merchant"
)

func checkTreasuryUsageInvoice(t *testing.T, f treasuryWorkflow) {
	ctx := merchant.WithID(t.Context(), f.merchant.MerchantID)
	captured, _ := f.actor(t, nil)
	payer, token := f.actor(t, []string{permissions.CustomerAll})
	for _, who := range []openrails.CustomerID{captured, payer} {
		_, err := f.client.DepositCredits(ctx, openrails.DepositCreditsRequest{CustomerID: &who, Invoker: who.String(), Currency: "USD", Amount: 100_000, Source: "usage-workflow", SourceID: uuid.NewString()})
		require.NoError(t, err)
	}
	for _, row := range []struct {
		resource, tier string
		amount         int64
	}{{"alpha", "standard", 5}, {"alpha", "standard", 3}, {"beta", "fast", 4}} {
		deadline := time.Now().Add(time.Hour)
		key := uuid.NewString()
		decision, err := f.client.Admit(ctx, openrails.AdmitRequest{CustomerID: captured, Invoker: captured.String(), InvokerType: openrails.InvokerTypePayer, Currency: "USD", RequestID: key, EstimatedAmount: row.amount, ExpiresAt: &deadline})
		require.NoError(t, err)
		require.True(t, decision.Allowed)
		usage := &openrails.CaptureUsage{EventType: "owner/" + row.resource, Resource: row.resource,
			Metadata: map[string]any{"function_name": "gen", "availability_tier": row.tier}, Source: "invoke", SourceID: key}
		first, err := f.client.Capture(ctx, key, row.amount, usage)
		require.NoError(t, err)
		replay, err := f.client.Capture(ctx, key, row.amount, usage)
		require.NoError(t, err)
		first.Replayed = true
		require.Equal(t, first, replay)
	}
	account, err := f.client.Balance(ctx, (captured).String())
	require.NoError(t, err)
	require.EqualValues(t, 99_988, account.BalanceAmount, "three captures plus retries debit once each")
	from, to := time.Now().Add(-time.Hour), time.Now().Add(time.Hour)
	for _, group := range []struct {
		key      string
		expected []openrails.UsageRollupRow
	}{
		{"resource", []openrails.UsageRollupRow{{Key: "alpha", Currency: "USD", EventCount: 2, TotalAmount: 8}, {Key: "beta", Currency: "USD", EventCount: 1, TotalAmount: 4}}},
		{"tier", []openrails.UsageRollupRow{{Key: "standard", Currency: "USD", EventCount: 2, TotalAmount: 8}, {Key: "fast", Currency: "USD", EventCount: 1, TotalAmount: 4}}},
	} {
		rows, err := f.client.UsageRollup(ctx, (captured).String(), "USD", from, to, group.key)
		require.NoError(t, err)
		require.ElementsMatch(t, group.expected, rows)
	}
	_, err = f.client.UsageRollup(ctx, (captured).String(), "USD", from, to, "bogus")
	require.ErrorIs(t, err, openrails.ErrInvalid)
	for _, row := range []struct {
		event                 string
		amount, input, output int64
	}{{"gpt-4o", 5_000, 100, 50}, {"gpt-4o", 3_000, 60, 30}, {"embeddings", 1_000, 200, 0}} {
		dimensions := map[string]int64{"input_tokens": row.input}
		if row.output != 0 {
			dimensions["output_tokens"] = row.output
		}
		report := openrails.UsageReport{CustomerID: payer, Invoker: "user:a", Currency: "USD", EventType: row.event, Dimensions: dimensions, Amount: row.amount, Source: "req", SourceID: uuid.NewString()}
		require.NoError(t, f.client.RecordUsage(ctx, report))
		require.NoError(t, f.client.RecordUsage(ctx, report))
	}
	base := f.hostURL + "/v1/customers/" + payer.String()
	read := func(path string) map[string]any {
		t.Helper()
		status, raw := requestWorkflowJSON(t, http.MethodGet, base+path, token, nil)
		require.Equal(t, http.StatusOK, status, string(raw))
		return workflowObject(t, raw)
	}
	query := url.Values{"currency": {"USD"}, "from": {from.UTC().Format(time.RFC3339Nano)}, "to": {to.UTC().Format(time.RFC3339Nano)}}
	usage := read("/usage?" + query.Encode())["usage"].([]any)
	require.Len(t, usage, 2)
	byEvent := func(rows []any) map[string]map[string]any {
		out := map[string]map[string]any{}
		for _, item := range rows {
			row := item.(map[string]any)
			out[row["event_type"].(string)] = row
		}
		return out
	}
	assertGroups := func(rows map[string]map[string]any, amount, count string) {
		t.Helper()
		for _, expected := range []struct {
			event, cost          string
			count, input, output int64
		}{{"gpt-4o", "8000", 2, 160, 80}, {"embeddings", "1000", 1, 200, 0}} {
			row := rows[expected.event]
			require.NotNil(t, row)
			require.Equal(t, expected.cost, row[amount])
			require.EqualValues(t, expected.count, row[count])
			dimensions := row["dimensions"].(map[string]any)
			require.EqualValues(t, expected.input, dimensions["input_tokens"])
			if expected.output != 0 {
				require.EqualValues(t, expected.output, dimensions["output_tokens"])
			}
		}
	}
	assertGroups(byEvent(usage), "total_amount", "event_count")
	for _, item := range usage {
		require.Equal(t, "USD", item.(map[string]any)["currency"])
	}
	query.Set("from", from.Add(-72*time.Hour).UTC().Format(time.RFC3339Nano))
	query.Set("to", to.Add(-48*time.Hour).UTC().Format(time.RFC3339Nano))
	require.Empty(t, read("/usage?" + query.Encode())["usage"])
	ledger := money.NewMoneyService(dbtest.OpenMerchantDB(t, f.merchant.MerchantID.UUID()))
	invoice, err := ledger.FinalizeInvoice(ctx, identity.CustomerID(payer), "USD", from, to)
	require.NoError(t, err)
	require.Equal(t, "paid", invoice.Status, "prepaid statement needs no provider collection")
	page := read("/invoices?limit=50&offset=0")
	require.EqualValues(t, 1, page["total"])
	invoices := page["invoices"].([]any)
	require.Len(t, invoices, 1)
	statement := invoices[0].(map[string]any)
	require.Equal(t, invoice.ID.String(), statement["id"])
	require.Equal(t, "9000", statement["usage_total"])
	require.Equal(t, "100000", statement["deposits_total"])
	full := read("/invoices/" + invoice.ID.String())
	require.Equal(t, invoice.ID.String(), full["id"])
	lines := full["line_items"].([]any)
	require.Len(t, lines, 2)
	assertGroups(byEvent(lines), "amount", "count")
	page = read("/invoices?limit=50&offset=1")
	require.EqualValues(t, 1, page["total"])
	require.Empty(t, page["invoices"])
}
