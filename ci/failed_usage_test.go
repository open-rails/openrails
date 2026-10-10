//go:build e2e && integration

package ci_test

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/merchant"
)

// Failed usage is a usage event with outcome failed. The customer's own
// failures are forgiven up to its policy's grace window and charged past it; a
// delegated invoker's are never charged and count toward its cutoff, past
// which admission refuses it. Replays and changed terms behave as for any
// usage event, and the windows are PostgreSQL rows written with the event.
func TestFailedUsage(t *testing.T) {
	f := newFixture(t)
	client := f.runtimeDeclaring(t, "failed-"+uuid.NewString()[:8], billing.MerchantSettings{
		BillingPolicies: []billing.BillingPolicy{{Name: "grace", Kind: "outstanding_cap",
			BadSpendWindows: []billing.BudgetWindow{{Key: "hour", WindowSeconds: 3600, Limit: 30_000, Currency: "USD"}}}},
		BillingPolicyBindings:             []billing.BillingPolicyBinding{{PolicyName: "grace"}},
		DelegatedInvokerWastedSpendLimits: []billing.BudgetWindow{{Key: "burst", WindowSeconds: 3600, Limit: 50_000, Currency: "USD"}},
	})
	ctx := merchant.WithID(t.Context(), client.MerchantID())
	var err error
	customer := billing.CustomerID(uuid.New())
	_, err = createCreditGrant(ctx, client, customer, billing.CreateCreditGrantParams{Currency: "USD", Amount: 1_000_000, Source: "support", SourceID: "seed"})
	require.NoError(t, err)
	failed := func(sourceID string, amount int64) billing.RecordUsageParams {
		return billing.RecordUsageParams{CustomerID: customer, Invoker: customer.String(), Currency: "USD", EventType: "gpu",
			Amount: amount, Source: "worker", SourceID: sourceID, Outcome: billing.UsageFailed}
	}
	delegated := func(invoker, sourceID string, amount int64) billing.RecordUsageParams {
		in := failed(sourceID, amount)
		in.Invoker, in.InvokerType = invoker, billing.InvokerTypeDelegated
		return in
	}
	balance := func() int64 { return balanceOf(t, client, customer) }

	t.Run("grace forgives failures, and past it they are charged", func(t *testing.T) {
		partial := failed("f-bad", 1)
		partial.Outcome = "partial"
		results, err := client.RecordUsage(ctx, []billing.RecordUsageParams{failed("f-1", 20_000), failed("f-2", 25_000), partial})
		require.NoError(t, err)
		require.Equal(t, http.StatusCreated, results[0].Status, "%+v", results[0].Error)
		first := results[0].Event
		require.Equal(t, billing.UsageFailed, first.Outcome)
		require.Zero(t, first.Amount)
		require.EqualValues(t, 20_000, first.ForgivenAmount)
		require.Nil(t, first.BalanceTransactionID, "a forgiven failure moves no money")
		second := results[1].Event
		require.EqualValues(t, 15_000, second.Amount, "the hour's grace leaves 10,000")
		require.EqualValues(t, 10_000, second.ForgivenAmount)
		require.NotNil(t, second.BalanceTransactionID)
		require.ErrorIs(t, results[2].Err(), billing.ErrInvalid, "a bad item refuses only itself")
		require.Equal(t, "outcome", *results[2].Error.Param)
		require.EqualValues(t, 985_000, balance())

		replay, err := recordUsage(ctx, client, failed("f-2", 25_000))
		require.NoError(t, err)
		require.True(t, replay.Replayed)
		require.EqualValues(t, 15_000, replay.Amount)
		require.EqualValues(t, 10_000, replay.ForgivenAmount)
		_, err = recordUsage(ctx, client, failed("f-2", 26_000))
		require.ErrorIs(t, err, billing.ErrIdempotencyKeyReused, "a changed failure is refused")
		succeeded := failed("f-2", 25_000)
		succeeded.Outcome = billing.UsageSucceeded
		_, err = recordUsage(ctx, client, succeeded)
		require.ErrorIs(t, err, billing.ErrIdempotencyKeyReused, "an event fails or succeeds, not both")
		require.EqualValues(t, 985_000, balance(), "money never moves twice")

		third, err := recordUsage(ctx, client, failed("f-3", 5_000))
		require.NoError(t, err)
		require.EqualValues(t, 5_000, third.Amount, "the grace is spent")
		require.Zero(t, third.ForgivenAmount)
	})

	t.Run("a delegated invoker's failures count toward its cutoff", func(t *testing.T) {
		admit := func(invoker string) *billing.Admission {
			t.Helper()
			verdicts, err := client.Admit(ctx, []billing.AdmitParams{{RequestID: uuid.NewString(), CustomerID: customer, Invoker: invoker,
				InvokerType: billing.InvokerTypeDelegated, Currency: "USD"}})
			require.NoError(t, err)
			require.NotNil(t, verdicts[0].Admission, "%+v", verdicts[0])
			return verdicts[0].Admission
		}
		cutoff := func(a *billing.Admission) bool { return a.DenyCode != nil && *a.DenyCode == "failure_rate_limited" }

		event, err := recordUsage(ctx, client, delegated("agent-1", "d-1", 30_000))
		require.NoError(t, err)
		require.Zero(t, event.Amount)
		require.EqualValues(t, 30_000, event.ForgivenAmount)
		require.False(t, cutoff(admit("agent-1")), "under the cutoff")
		_, err = recordUsage(ctx, client, delegated("agent-1", "d-2", 25_000))
		require.NoError(t, err)
		require.True(t, cutoff(admit("agent-1")), "past the cutoff, admission refuses the invoker")
		require.False(t, cutoff(admit("agent-2")), "another invoker has its own cutoff")
		require.EqualValues(t, 980_000, balance(), "a delegated invoker's failure is never charged")

		bad := delegated("agent-1", "d-3", 1)
		bad.Outcome = billing.UsageSucceeded
		_, err = recordUsage(ctx, client, bad)
		require.ErrorIs(t, err, billing.ErrInvalid)
	})

	t.Run("the windows are PostgreSQL rows", func(t *testing.T) {
		rows, err := f.pool.Query(ctx, `SELECT invoker, window_key, amount FROM `+pgx.Identifier{f.schema, "failed_usage_windows"}.Sanitize()+
			` WHERE merchant_id = $1 AND customer_id = $2 ORDER BY invoker`, client.MerchantID().UUID(), customer.UUID())
		require.NoError(t, err)
		type window struct {
			Invoker, Key string
			Amount       int64
		}
		got, err := pgx.CollectRows(rows, pgx.RowToStructByPos[window])
		require.NoError(t, err)
		require.Equal(t, []window{{"", "hour", 50_000}, {"agent-1", "burst", 55_000}}, got)
	})

	t.Run("metrics report failed usage", func(t *testing.T) {
		from, to := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339), time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
		res, err := client.QueryMetrics(ctx, billing.MetricsQuery{
			Measures: []string{"usage_units", "usage_revenue", "forgiven_usage"}, By: []string{"outcome"},
			Range:   &billing.MetricsRange{From: from, To: to},
			Filters: map[string][]string{"customer": {customer.String()}},
		})
		require.NoError(t, err)
		got := map[string][]string{}
		for _, row := range metricsRows(res) {
			got[row["outcome"]] = []string{row["usage_units"], row["usage_revenue"], row["forgiven_usage"]}
		}
		require.Equal(t, map[string][]string{"failed": {"5", "20000", "85000"}}, got)
	})
}

// metricsRows reads a metrics result as rows of cells by column name, each as
// its JSON text.
func metricsRows(res *billing.MetricsResult) []map[string]string {
	out := make([]map[string]string, len(res.Rows))
	for i, row := range res.Rows {
		out[i] = map[string]string{}
		for j, col := range res.Columns {
			switch v := row[j].(type) {
			case string:
				out[i][col.Name] = v
			default:
				raw, _ := json.Marshal(v)
				out[i][col.Name] = string(raw)
			}
		}
	}
	return out
}
