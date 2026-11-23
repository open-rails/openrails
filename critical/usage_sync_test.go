//go:build integration

package critical_test

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	"github.com/riverqueue/river"
	"github.com/stretchr/testify/require"
)

// Usage arrives in lease syncs: a test is its own batch job, with its own
// lease and numbering.

func usageMeter(t *testing.T, client *openrails.Client, amount int64) (*billing.Product, billing.SetMeterParams) {
	t.Helper()
	product, err := client.CreateProduct(t.Context(), billing.CreateProductParams{Key: "usage-compute", DisplayName: "Usage compute"})
	require.NoError(t, err)
	params := billing.SetMeterParams{EventType: "compute", ValueProperty: "units", Aggregation: catalog.AggregationSum, RateCard: catalog.Value(billing.MeterRateCardParams{ProductID: product.ID, Price: catalog.RatePrice{Model: catalog.ModelPerUnit, Currency: "USD", PerUnit: &catalog.PerUnitPrice{UnitAmount: amount, DivideBy: 1}}})}
	_, err = client.SetMeter(t.Context(), "compute", params)
	require.NoError(t, err)
	return product, params
}

// usageInput is one compute report of units for a customer; its number is
// given when it is sent.
func usageInput(customer billing.CustomerID, id string, units int64) billing.LeaseUsage {
	return billing.LeaseUsage{CustomerID: customer, Invoker: customer.String(), InvokerType: billing.InvokerTypeCustomer, Currency: "USD", EventType: "compute",
		Dimensions: map[string]int64{"units": units}, Source: "critical", SourceID: id, Metadata: map[string]any{"source": "host work receipt"}}
}

// newLease is a fresh lease id, as a gate or a batch job picks one; random,
// so the world's clock may run ahead of its own.
func newLease() billing.LeaseID { return billing.LeaseID(uuid.New()) }

// numbered numbers items from first on, in order. An item that names no
// time occurred when it settles.
func numbered(first int64, items ...billing.LeaseUsage) []billing.LeaseUsage {
	out := make([]billing.LeaseUsage, len(items))
	for i, item := range items {
		item.N = first + int64(i)
		out[i] = item
	}
	return out
}

// syncLease is one lease sync that must succeed as a call.
func syncLease(t *testing.T, client *openrails.Client, params billing.SyncLeasesParams) *billing.SyncLeasesResult {
	t.Helper()
	out, err := client.SyncLeases(t.Context(), params)
	require.NoError(t, err)
	return out
}

// settleUsage sends numbered items on a lease and requires that it settled every
// one.
func settleUsage(t *testing.T, client *openrails.Client, lease billing.LeaseID, items ...billing.LeaseUsage) *billing.SyncLeasesResult {
	t.Helper()
	out := syncLease(t, client, billing.SyncLeasesParams{LeaseID: lease, Usage: items})
	require.Empty(t, out.Usage.Refused, "%+v", out.Usage.Refused)
	require.Equal(t, items[len(items)-1].N, out.Usage.ThroughN)
	return out
}

// oneUsage settles one report on a lease of its own.
func oneUsage(t *testing.T, client *openrails.Client, input billing.LeaseUsage) billing.LeaseID {
	t.Helper()
	lease := newLease()
	settleUsage(t, client, lease, numbered(1, input)...)
	return lease
}

// refusedItem is the refusal of item n.
func refusedItem(t *testing.T, out *billing.SyncLeasesResult, n int64) billing.LeaseUsageRefusal {
	t.Helper()
	for _, r := range out.Usage.Refused {
		if r.N == n {
			require.NotNil(t, r.Error)
			return r
		}
	}
	require.Failf(t, "item not refused", "item %d settled: %+v", n, out.Usage.Refused)
	return billing.LeaseUsageRefusal{}
}

// nextDay moves the clock past the next 00:00 UTC.
func nextDay(w *world) {
	now := w.clock.Now().UTC()
	w.clock.Advance(now.Truncate(24 * time.Hour).Add(24*time.Hour + time.Minute).Sub(now))
}

// postDays runs a billing pass: every accrual of a day that ended posts.
func postDays(t *testing.T, fleet *river.Client[pgx.Tx]) {
	t.Helper()
	runRegistered(t, fleet, "openrails.billing_pass", 1)
}

// usageDeductions are the customer's balance transactions that posted usage,
// newest first.
func usageDeductions(t *testing.T, client *openrails.Client, customer billing.CustomerID) []billing.BalanceTransaction {
	t.Helper()
	page, err := client.ListBalanceTransactions(t.Context(), customer, billing.BalanceTransactionListParams{Currency: "USD"})
	require.NoError(t, err)
	var out []billing.BalanceTransaction
	for _, tx := range page.Items {
		if tx.Usage != nil {
			out = append(out, tx)
		}
	}
	return out
}
