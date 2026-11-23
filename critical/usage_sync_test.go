//go:build integration

package critical_test

import (
	"sync"
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

// newLease is a fresh lease id, as a gate or a batch job picks one: a UUIDv7
// of the real clock, which the world's clock may run ahead of or behind.
func newLease() billing.LeaseID { return billing.LeaseID(uuid.Must(uuid.NewV7())) }

// leaseMintedAt is a UUIDv7 lease id minted at an instant.
func leaseMintedAt(at time.Time) billing.LeaseID {
	id := uuid.Must(uuid.NewV7())
	ms := uint64(at.UnixMilli())
	for i := 0; i < 6; i++ {
		id[i] = byte(ms >> (40 - 8*i))
	}
	return billing.LeaseID(id)
}

// applied is, per lease, what its gate applied of the last answer it got:
// syncLease reports it on the lease's next sync, as a gate does.
var applied sync.Map

// appliedOf is every hold version an answer carried.
func appliedOf(out *billing.SyncLeasesResult) []billing.HoldApplied {
	latest := map[[2]string]billing.HoldApplied{}
	var order [][2]string
	for _, h := range out.Holds {
		k := [2]string{h.CustomerID.String(), h.LimitSet}
		if prev, ok := latest[k]; !ok {
			order = append(order, k)
		} else if prev.Version >= h.Version {
			continue
		}
		latest[k] = billing.HoldApplied{CustomerID: h.CustomerID, LimitSet: h.LimitSet, Version: h.Version}
	}
	result := make([]billing.HoldApplied, 0, len(order))
	for _, k := range order {
		result = append(result, latest[k])
	}
	return result
}

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

// syncLease is one lease sync that must succeed as a call, made as a gate
// makes it: with no Applied given, it reports the versions of the lease's
// previous answer (a non-nil empty Applied is a lost reply). Every hold it
// answers carries a balance part.
func syncLease(t *testing.T, client *openrails.Client, params billing.SyncLeasesParams) *billing.SyncLeasesResult {
	t.Helper()
	if params.Applied == nil {
		if prev, ok := applied.Load(params.LeaseID); ok {
			params.Applied = prev.([]billing.HoldApplied)
		}
	}
	out, err := client.SyncLeases(t.Context(), params)
	require.NoError(t, err)
	applied.Store(params.LeaseID, appliedOf(out))
	for _, h := range out.Holds {
		require.NotEmpty(t, h.Limits)
		found := false
		for _, l := range h.Limits {
			found = found || l.Limit == "balance"
		}
		require.True(t, found, "every hold answered carries its balance: %+v", h)
	}
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
