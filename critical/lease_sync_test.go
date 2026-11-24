//go:build integration

package critical_test

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/modules/limits"
	"github.com/stretchr/testify/require"
)

// computeView asks for a customer's view for compute requests.
func computeView(customer billing.CustomerID) billing.ViewParams {
	return billing.ViewParams{CustomerID: customer, Invoker: customer.String(), InvokerType: billing.InvokerTypeCustomer, Currency: "USD", EventType: ptr("compute")}
}

// workView asks for a customer's view for work requests (unitPlan).
func workView(customer billing.CustomerID) billing.ViewParams {
	return billing.ViewParams{CustomerID: customer, Invoker: customer.String(), InvokerType: billing.InvokerTypeCustomer, Currency: "USD", EventType: ptr("work")}
}

// viewOf is an answer's view of a customer.
func viewOf(t *testing.T, out *billing.SyncLeasesResult, customer billing.CustomerID) billing.View {
	t.Helper()
	for _, v := range out.Views {
		if v.CustomerID == customer {
			return v
		}
	}
	require.Failf(t, "no view", "%s: %+v", customer, out.Views)
	return billing.View{}
}

// lineOf is a view's line of a limit.
func lineOf(t *testing.T, v billing.View, limit string) billing.ViewLimit {
	t.Helper()
	for _, l := range v.Limits {
		if l.Limit == limit {
			return l
		}
	}
	require.Failf(t, "no line", "%s: %+v", limit, v.Limits)
	return billing.ViewLimit{}
}

// leaseAdmit admits a request as a request hold of a lease, as a gate does
// near a limit.
func leaseAdmit(t *testing.T, w *world, lease billing.LeaseID, p billing.AdmitParams) billing.AdmissionVerdict {
	t.Helper()
	p.LeaseID, p.ExpiresAt = &lease, nil
	return admitOne(t, w.alpha, p)
}

// spentOn reports n compute units of a lease's request hold.
func spentOn(customer billing.CustomerID, requestID, id string, units int64) billing.LeaseUsage {
	u := usageInput(customer, id, units)
	u.RequestID = &requestID
	return u
}

func TestLeaseSyncs(t *testing.T) {
	t.Run("OR-J24-S06/a-lost-reply-resends-a-gap-is-refused", func(t *testing.T) {
		w := newWorld(t)
		customer, _ := fundedCustomer(t, w)
		usageMeter(t, w.alpha, 100)
		lease := newLease()
		first := numbered(1, usageInput(customer, "a", 1), usageInput(customer, "b", 1), usageInput(customer, "c", 1))
		settleUsage(t, w.alpha, lease, first...)
		balance(t, w.alpha, customer, 700, 300, 1000)
		// The reply was lost: the gate resends its unacknowledged items with
		// the next one, and only the new one settles.
		out := settleUsage(t, w.alpha, lease, append(first, numbered(4, usageInput(customer, "d", 1))...)...)
		require.EqualValues(t, 4, out.Usage.ThroughN)
		balance(t, w.alpha, customer, 600, 400, 1000)
		// A gap is refused and settles nothing; the item that fills it does.
		gap := syncLease(t, w.alpha, billing.SyncLeasesParams{LeaseID: lease, Usage: numbered(6, usageInput(customer, "f", 1))})
		require.EqualValues(t, 4, gap.Usage.ThroughN)
		refused := refusedItem(t, gap, 6)
		require.Equal(t, http.StatusConflict, refused.Status)
		require.Equal(t, billing.CodeUsageOutOfOrder, refused.Error.Code)
		balance(t, w.alpha, customer, 600, 400, 1000)
		settleUsage(t, w.alpha, lease, numbered(5, usageInput(customer, "e", 1), usageInput(customer, "f", 1))...)
		balance(t, w.alpha, customer, 400, 600, 1000)
	})

	t.Run("OR-J24-S07/a-new-process-resends-a-dead-leases-outbox-once", func(t *testing.T) {
		w := newWorld(t)
		customer, _ := fundedCustomer(t, w)
		usageMeter(t, w.alpha, 100)
		lease := newLease()
		outbox := numbered(1, usageInput(customer, "a", 1), usageInput(customer, "b", 2), usageInput(customer, "c", 3))
		syncLease(t, w.alpha, billing.SyncLeasesParams{LeaseID: lease, Views: []billing.ViewParams{computeView(customer)}})
		require.True(t, leaseAdmit(t, w, lease, usageAdmission(w, customer, "near", "compute", map[string]int64{"units": 1}, nil)).Allowed())
		settleUsage(t, w.alpha, lease, outbox[:2]...)
		balance(t, w.alpha, customer, 600, 400, 1000)
		// The process dies with item 3 unsent and a request hold open; once
		// the lease stops syncing the hold counts nowhere.
		w.clock.Advance(limits.LeaseLifetime + time.Second)
		balance(t, w.alpha, customer, 700, 300, 1000)
		// Its successor resends the outbox against the dead lease with close:
		// the first two answer their receipts, the third settles as late
		// usage, and the close releases the hold for good.
		out := syncLease(t, w.alpha, billing.SyncLeasesParams{LeaseID: lease, Usage: outbox, Close: true})
		require.EqualValues(t, 3, out.Usage.ThroughN)
		require.Empty(t, out.Usage.Refused)
		require.False(t, out.Lease.ExpiresAt.After(w.clock.Now()), "a closed lease has ended")
		balance(t, w.alpha, customer, 400, 600, 1000)
		again := syncLease(t, w.alpha, billing.SyncLeasesParams{LeaseID: lease, Usage: outbox})
		require.Empty(t, again.Usage.Refused)
		balance(t, w.alpha, customer, 400, 600, 1000)
		admission, err := w.alpha.GetAdmission(t.Context(), "near")
		require.NoError(t, err)
		require.Equal(t, billing.AdmissionReleased, *admission.State)
	})
	t.Run("OR-J24-S08/a-batch-job-that-loses-its-first-reply-posts-once", func(t *testing.T) {
		w := newWorld(t)
		customer, _ := fundedCustomer(t, w)
		usageMeter(t, w.alpha, 100)
		lease := newLease()
		// A batch job numbers its items from its own cursor.
		batch := numbered(9041, usageInput(customer, "row-9041", 1), usageInput(customer, "row-9042", 1))
		settleUsage(t, w.alpha, lease, batch...)
		out := settleUsage(t, w.alpha, lease, batch...)
		require.EqualValues(t, 9042, out.Usage.ThroughN)
		balance(t, w.alpha, customer, 800, 200, 1000)
		fleet := startFleet(t, w)
		nextDay(w)
		postDays(t, fleet)
		require.Len(t, usageDeductions(t, w.alpha, customer), 1)
		balance(t, w.alpha, customer, 800, 0, 800)
	})

	t.Run("OR-J24-S09/a-batch-is-priced-on-its-total", func(t *testing.T) {
		w := newWorld(t)
		customer := billing.CustomerID(uuid.New())
		fund(t, w.alpha, customer, "funded", 1_000_000)
		product, err := w.alpha.CreateProduct(t.Context(), billing.CreateProductParams{Key: "tokens", DisplayName: "Tokens"})
		require.NoError(t, err)
		_, err = w.alpha.SetMeter(t.Context(), "tokens", billing.SetMeterParams{EventType: "chat", ValueProperty: "input_tokens", Aggregation: catalog.AggregationSum,
			RateCard: catalog.Value(billing.MeterRateCardParams{ProductID: product.ID, Price: catalog.RatePrice{Model: catalog.ModelPerUnit, Currency: "USD",
				PerUnit: &catalog.PerUnitPrice{UnitAmount: 150_000, DivideBy: 1_000_000}}})})
		require.NoError(t, err)
		items := make([]billing.LeaseUsage, 1000)
		for i := range items {
			items[i] = billing.LeaseUsage{CustomerID: customer, Invoker: customer.String(), InvokerType: billing.InvokerTypeCustomer, Currency: "USD",
				EventType: "chat", Dimensions: map[string]int64{"input_tokens": 1234}, Source: "critical", SourceID: fmt.Sprintf("req-%d", i)}
		}
		settleUsage(t, w.alpha, newLease(), numbered(1, items...)...)
		// 1,234,000 tokens at $0.15 per million.
		balance(t, w.alpha, customer, 1_000_000-185_100, 185_100, 1_000_000)
	})

	t.Run("OR-J24-S10/periods-and-rate-card-revisions-are-priced-apart", func(t *testing.T) {
		w := newWorld(t)
		product, err := w.alpha.CreateProduct(t.Context(), billing.CreateProductParams{Key: "graduated", DisplayName: "Graduated"})
		require.NoError(t, err)
		ten := int64(10)
		_, err = w.alpha.SetMeter(t.Context(), "graduated", billing.SetMeterParams{EventType: "graduated", ValueProperty: "units", Aggregation: catalog.AggregationSum,
			RateCard: catalog.Value(billing.MeterRateCardParams{ProductID: product.ID, Price: catalog.RatePrice{Model: catalog.ModelTiered, Currency: "USD",
				Tiered: &catalog.TieredPrice{Mode: catalog.TierModeGraduated, Tiers: []catalog.RateTier{{UpTo: &ten, UnitAmount: 1}, {UnitAmount: 100}}}}})})
		require.NoError(t, err)
		// Two items of 8 units in two pricing periods (30-day tiles from the
		// epoch): each is priced in its own period's first tier.
		periods := billing.CustomerID(uuid.New())
		fund(t, w.alpha, periods, "periods", 1000)
		now := w.clock.Now().UTC()
		start := time.Unix(now.Unix()-now.Unix()%(30*24*3600), 0).UTC()
		late := billing.LeaseUsage{CustomerID: periods, Invoker: periods.String(), Currency: "USD", EventType: "graduated", Dimensions: map[string]int64{"units": 8}, Source: "critical", SourceID: "late", OccurredAt: start.Add(-time.Hour)}
		current := late
		current.SourceID, current.OccurredAt = "current", time.Time{}
		settleUsage(t, w.alpha, newLease(), numbered(1, late, current)...)
		balance(t, w.alpha, periods, 1000-16, 16, 1000)

		// A lease's request hold pins the cards in force when it was
		// admitted, and its capture through the lease is priced by them;
		// other usage by the cards in force when it settles.
		_, params := usageMeter(t, w.alpha, 100)
		pinned := billing.CustomerID(uuid.New())
		fund(t, w.alpha, pinned, "pinned", 1000)
		lease := newLease()
		syncLease(t, w.alpha, billing.SyncLeasesParams{LeaseID: lease})
		require.True(t, leaseAdmit(t, w, lease, usageAdmission(w, pinned, "pinned-hold", "compute", map[string]int64{"units": 1}, nil)).Allowed())
		params.RateCard.Value.Price.PerUnit.UnitAmount = 300
		_, err = w.alpha.SetMeter(t.Context(), "compute", params)
		require.NoError(t, err)
		settleUsage(t, w.alpha, lease, numbered(1, spentOn(pinned, "pinned-hold", "under-the-hold", 1), usageInput(pinned, "without-a-hold", 1))...)
		balance(t, w.alpha, pinned, 1000-400, 400, 1000)
	})

	t.Run("OR-J24-S11/a-day-posts-once-and-once-more-before-a-top-up", func(t *testing.T) {
		w := newWorld(t, func(w *world) {
			// Keep the day's usage inside one UTC day whatever the hour.
			now := w.clock.Now().UTC()
			w.clock.Advance(now.Truncate(24 * time.Hour).Add(36 * time.Hour).Sub(now))
		})
		customer, _ := fundedCustomer(t, w)
		usageMeter(t, w.alpha, 100)
		lease := newLease()
		settleUsage(t, w.alpha, lease, numbered(1, usageInput(customer, "a", 2))...)
		balance(t, w.alpha, customer, 800, 200, 1000)
		// Until it posts, the accrual is held: the balance refuses as if it had.
		requireDenied(t, admitOne(t, w.alpha, reservation(w, customer, "too-much", 801)), "insufficient_balance")
		require.True(t, admitOne(t, w.alpha, reservation(w, customer, "fits", 800)).Allowed())
		released, err := w.alpha.ReleaseAdmissions(t.Context(), []string{"fits"})
		require.NoError(t, err)
		require.NoError(t, released[0].Err())

		// A top-up posts the day so far first.
		fund(t, w.alpha, customer, "top-up", 500)
		balance(t, w.alpha, customer, 1300, 0, 1300)
		require.Len(t, usageDeductions(t, w.alpha, customer), 1)
		settleUsage(t, w.alpha, lease, numbered(2, usageInput(customer, "b", 1))...)
		balance(t, w.alpha, customer, 1200, 100, 1300)

		// The rest of the day posts once the day ends.
		fleet := startFleet(t, w)
		postDays(t, fleet)
		require.Len(t, usageDeductions(t, w.alpha, customer), 1, "the day has not ended")
		nextDay(w)
		postDays(t, fleet)
		balance(t, w.alpha, customer, 1200, 0, 1200)
		deductions := usageDeductions(t, w.alpha, customer)
		require.Len(t, deductions, 2)
		require.EqualValues(t, -100, deductions[0].Amount)
		require.EqualValues(t, -200, deductions[1].Amount)
		for _, d := range deductions {
			require.Equal(t, w.clock.Now().UTC().Add(-24*time.Hour).Format(time.DateOnly), d.Usage.Day)
			require.EqualValues(t, 300, d.Usage.Amount, "each posting shows its day")
			require.Equal(t, []billing.UsageDeductionLine{{MeterKey: "compute", Requests: 2, Quantity: 3, Amount: 300}}, d.Usage.Lines)
		}
	})

	t.Run("OR-J24-S14/views-of-an-occasional-customer-reserve-nothing-and-show-what-settled", func(t *testing.T) {
		w := newWorld(t)
		applyLimits(t, w.alpha, unitPlan)
		customer := billing.CustomerID(uuid.New())
		grantLimitProduct(t, w.alpha, customer, "plan", 24)
		settleUsage(t, w.alpha, newLease(), numbered(1, workReport(customer, "opens", 1))...)
		gates := make([]billing.LeaseID, 10)
		for i := range gates {
			gates[i] = newLease()
			out := syncLease(t, w.alpha, billing.SyncLeasesParams{LeaseID: gates[i], Views: []billing.ViewParams{workView(customer)}})
			require.NotNil(t, out.LimitSets[0])
			require.Nil(t, out.LimitSets[0].Refusal)
			require.Contains(t, out.LimitSets[0].Limits, "quota")
			require.EqualValues(t, 99, lineOf(t, viewOf(t, out, customer), "quota").Remaining, "each process sees what is left")
			require.Equal(t, i+2, out.Gates, "every live lease counts, the batch job's too")
		}
		require.Zero(t, customerLimit(t, w.alpha, customer, "quota").Held, "views reserve nothing")

		// A batch settles 95 units: each process's next view shows it.
		settleUsage(t, w.alpha, newLease(), numbered(1, workReport(customer, "batch", 95))...)
		for _, gate := range gates {
			out := syncLease(t, w.alpha, billing.SyncLeasesParams{LeaseID: gate, Views: []billing.ViewParams{workView(customer)}})
			line := lineOf(t, viewOf(t, out, customer), "quota")
			require.EqualValues(t, 96, line.Used)
			require.EqualValues(t, 4, line.Remaining)
		}
	})
	t.Run("OR-J24-S16/a-closed-lease-settles-releases-its-holds-and-clears-its-report", func(t *testing.T) {
		w := newWorld(t)
		customer, _ := fundedCustomer(t, w)
		usageMeter(t, w.alpha, 100)
		lease := newLease()
		syncLease(t, w.alpha, billing.SyncLeasesParams{LeaseID: lease, InFlight: []billing.InFlight{{CustomerID: customer, Limit: "balance", Unit: "USD", Amount: 100}}})
		require.True(t, leaseAdmit(t, w, lease, usageAdmission(w, customer, "held", "compute", map[string]int64{"units": 1}, nil)).Allowed())
		balance(t, w.alpha, customer, 800, 200, 1000)

		// A sync with close settles its usage, then releases the lease's
		// holds, clears its report and ends it, long before it would expire.
		out := syncLease(t, w.alpha, billing.SyncLeasesParams{LeaseID: lease, Usage: numbered(1, usageInput(customer, "last", 1)), Close: true})
		require.EqualValues(t, 1, out.Usage.ThroughN)
		require.Empty(t, out.Usage.Refused)
		require.False(t, out.Lease.ExpiresAt.After(w.clock.Now()), "a closed lease has ended")
		balance(t, w.alpha, customer, 900, 100, 1000)

		// A closed lease owns no new hold; usage against it still settles,
		// in full, and its report stays clear.
		v := leaseAdmit(t, w, lease, usageAdmission(w, customer, "after-close", "compute", map[string]int64{"units": 1}, nil))
		require.NotNil(t, v.Error)
		require.Equal(t, http.StatusConflict, v.Status)
		require.Equal(t, billing.CodeLeaseNotHeld, v.Error.Code)
		out = syncLease(t, w.alpha, billing.SyncLeasesParams{LeaseID: lease, Usage: numbered(2, usageInput(customer, "late", 1)),
			InFlight: []billing.InFlight{{CustomerID: customer, Limit: "balance", Unit: "USD", Amount: 100}}})
		require.EqualValues(t, 2, out.Usage.ThroughN)
		balance(t, w.alpha, customer, 800, 200, 1000)
		again := syncLease(t, w.alpha, billing.SyncLeasesParams{LeaseID: lease, Close: true})
		require.EqualValues(t, 2, again.Usage.ThroughN)
		require.Equal(t, out.Lease.ExpiresAt, again.Lease.ExpiresAt, "a lease closes once")
	})

	t.Run("OR-J24-S37/a-batch-reporters-usage-only-sync-answers-as-before", func(t *testing.T) {
		w := newWorld(t)
		customer, _ := fundedCustomer(t, w)
		usageMeter(t, w.alpha, 100)
		// The shape a batch reporter sends: a lease id and numbered usage,
		// nothing else.
		lease := newLease()
		items := numbered(1, usageInput(customer, "fee-1", 1), usageInput(customer, "fee-2", 2))
		out, err := w.alpha.SyncLeases(t.Context(), billing.SyncLeasesParams{LeaseID: lease, Usage: items})
		require.NoError(t, err)
		require.EqualValues(t, 2, out.Usage.ThroughN)
		require.Empty(t, out.Usage.Refused)
		require.Empty(t, out.Views)
		require.Empty(t, out.LimitSets)
		balance(t, w.alpha, customer, 700, 300, 1000)
		again, err := w.alpha.SyncLeases(t.Context(), billing.SyncLeasesParams{LeaseID: lease, Usage: items})
		require.NoError(t, err)
		require.EqualValues(t, 2, again.Usage.ThroughN)
		require.Empty(t, again.Usage.Refused, "a resend answers its receipt")
		changed := items[1]
		changed.Dimensions = map[string]int64{"units": 3}
		out, err = w.alpha.SyncLeases(t.Context(), billing.SyncLeasesParams{LeaseID: lease, Usage: []billing.LeaseUsage{items[0], changed}})
		require.NoError(t, err)
		require.Equal(t, billing.CodeIdempotencyKeyReused, refusedItem(t, out, 2).Error.Code)
		balance(t, w.alpha, customer, 700, 300, 1000)
	})

	t.Run("OR-J24-S38/usage-counts-in-the-spend-windows-of-the-attributes-it-reports", func(t *testing.T) {
		w := newWorld(t, goldTrust)
		usageMeter(t, w.alpha, 100)
		customer := billing.CustomerID(uuid.New())
		fund(t, w.alpha, customer, "funded", 10_000)
		gold := computeView(customer)
		gold.Invoker, gold.TrustLevel = "apiKey:gold", ptr("gold")
		lease := newLease()
		out := syncLease(t, w.alpha, billing.SyncLeasesParams{LeaseID: lease, Views: []billing.ViewParams{computeView(customer), gold}})
		free, golden := out.LimitSets[0].Limits[1], out.LimitSets[1].Limits[1]
		// Usage a batch job reports with no attributes counts in the window of
		// the customer's own trust level; usage reported as the gold invoker
		// in the gold window.
		goldWork := usageInput(customer, "gold", 2)
		goldWork.Invoker, goldWork.TrustLevel = "apiKey:gold", ptr("gold")
		out = syncLease(t, w.alpha, billing.SyncLeasesParams{LeaseID: lease, Usage: numbered(1, usageInput(customer, "batch", 3), goldWork),
			Views: []billing.ViewParams{computeView(customer), gold}})
		require.Empty(t, out.Usage.Refused)
		v := viewOf(t, out, customer)
		require.EqualValues(t, 300, lineOf(t, v, free).Used)
		require.EqualValues(t, 200, lineOf(t, v, golden).Used)
	})
}
