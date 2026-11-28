//go:build integration

package critical_test

import (
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/billing"
	"github.com/stretchr/testify/require"
)

func TestKnownThroughMetrics(t *testing.T) {
	t.Run("OR-J42-S44/a-stale-psps-metrics-are-the-best-data-known-marked-never-read-as-churn", func(t *testing.T) {
		a, _, b, _ := staleImport(t)
		now := a.clock.Now().UTC()
		psps, err := a.alpha.ListPSPs(t.Context(), billing.PSPListParams{})
		require.NoError(t, err)
		require.Len(t, psps.Items, 1)
		psp := psps.Items[0].ID

		// NMI's own record of a cycle due inside the gap, as a pull would leave
		// it: missed. Nobody could see it, so no rebill rate counts it.
		sub, err := a.alpha.GetSubscription(t.Context(), b.subs[0])
		require.NoError(t, err)
		_, err = a.pool.Exec(t.Context(), "INSERT INTO "+pgx.Identifier{a.schema, "rebill_cycles"}.Sanitize()+
			` (merchant_id, subscription_id, customer_id, psp_id, rail, owner, due_at, amount, currency, missed_at, miss_reason)
			VALUES ($1, $2, $3, $4, 'nmi', 'nmi_schedule', $5, 10000000, 'USD', $6, 'provider_stalled')`,
			a.alpha.MerchantID().UUID(), sub.ID.UUID(), sub.CustomerID.UUID(), psp.UUID(), b.dues[0], b.dues[0].Add(25*time.Hour))
		require.NoError(t, err)

		window := &billing.MetricsRange{From: b.asOf.Add(-30 * 24 * time.Hour).Format(time.RFC3339), To: now.Add(time.Second).Format(time.RFC3339)}
		res, err := a.alpha.QueryMetrics(t.Context(), billing.QueryMetricsParams{Measures: []string{"cash", "payments"}, By: []string{"time"}, Grain: "day", Range: window})
		require.NoError(t, err)

		// Every read reports each PSP's known-through.
		require.Len(t, res.Freshness, 1)
		f := res.Freshness[0]
		require.Equal(t, psp, f.PSPID)
		require.Equal(t, "nmi", f.Key)
		require.False(t, f.Current)
		require.True(t, f.HasData)
		require.Equal(t, billing.PSPImported, f.Cause)
		require.True(t, f.KnownThrough.Equal(b.asOf))
		require.NotNil(t, res.CompleteThrough)
		require.True(t, res.CompleteThrough.Equal(b.asOf), "incomplete after the known-through")
		require.NotNil(t, res.KnownThrough, "every PSP with data is stale: the data is as of its known-through")
		require.True(t, res.KnownThrough.Equal(b.asOf))
		// What staleness touches is counted beside at-risk, not in it.
		require.Len(t, f.Awaiting, 1)
		require.Equal(t, "USD", string(f.Awaiting[0].Currency))
		require.EqualValues(t, 2, f.Awaiting[0].Subscriptions, "both renewals came due unseen")
		require.Positive(t, f.Awaiting[0].MRR)
		require.Empty(t, atRiskMRR(t, a.alpha, riskWindow(b.asOf, now.Add(time.Second))))

		// Cash and payments after the known-through are missing, never zero.
		var known, missing int
		for _, row := range metricTable(t, a.alpha, billing.QueryMetricsParams{Measures: []string{"cash", "payments"}, By: []string{"time"}, Grain: "day", Range: window}) {
			bucket, err := time.Parse(time.RFC3339, row["time"].(string))
			require.NoError(t, err)
			if bucket.Before(b.asOf) {
				require.NotNil(t, row["cash"], "cash on %s is known", bucket)
				require.NotNil(t, row["payments"])
				known++
				continue
			}
			require.Nil(t, row["cash"], "cash on %s is unknown, not zero", bucket)
			require.Nil(t, row["payments"])
			missing++
		}
		require.Positive(t, known)
		require.Positive(t, missing)

		// MRR carries the last known state forward: each subscription adds
		// what it did, all of it awaiting the provider.
		var carried int64
		for _, id := range b.subs {
			sub, err := a.alpha.GetSubscription(t.Context(), id)
			require.NoError(t, err)
			require.Equal(t, billing.SubscriptionActive, sub.Status)
			require.NotNil(t, sub.MRR)
			require.Positive(t, sub.MRR.Amount)
			carried += sub.MRR.Amount
		}
		require.Equal(t, carried, f.Awaiting[0].MRR)

		// The rebill due in the gap is left out of every rebill count.
		rebills := metricTable(t, a.alpha, billing.QueryMetricsParams{Measures: []string{"rebills"}, Range: window})
		for _, row := range rebills {
			require.Zero(t, whole(t, row["rebills"]), "a renewal nobody could see is no rebill")
		}
	})
}
