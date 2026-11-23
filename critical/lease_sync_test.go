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
	"github.com/stretchr/testify/require"
)

// computeWant asks a lease for a hold covering one compute request of units.
func computeWant(customer billing.CustomerID, units int64) billing.LeaseWant {
	return billing.LeaseWant{CustomerID: customer, Invoker: customer.String(), InvokerType: billing.InvokerTypeCustomer, Currency: "USD",
		Usage: &billing.UsageEstimate{EventType: "compute", Dimensions: map[string]int64{"units": units}}}
}

// workWant asks for a hold covering one work request of units (unitPlan).
func workWant(customer billing.CustomerID, units int64) billing.LeaseWant {
	return billing.LeaseWant{CustomerID: customer, Invoker: customer.String(), InvokerType: billing.InvokerTypeCustomer, Currency: "USD",
		Usage: &billing.UsageEstimate{EventType: "work", Dimensions: map[string]int64{"units": units}}}
}

// heldIn is what an answer's hold of the customer and limit set holds of a
// limit; found is false when the answer does not list the hold.
func heldIn(out *billing.SyncLeasesResult, customer billing.CustomerID, set, limit string) (int64, bool) {
	for _, h := range out.Holds {
		if h.CustomerID != customer || h.LimitSet != set {
			continue
		}
		for _, l := range h.Limits {
			if l.Limit == limit {
				return l.Held, true
			}
		}
	}
	return 0, false
}

// wanted is the one want's answer: its limit set and refusal.
func wanted(t *testing.T, out *billing.SyncLeasesResult) (string, *billing.Refusal) {
	t.Helper()
	require.Len(t, out.Wants, 1)
	require.Nil(t, out.Wants[0].Error, "%+v", out.Wants[0].Error)
	require.Equal(t, http.StatusOK, out.Wants[0].Status)
	require.NotNil(t, out.Wants[0].LimitSet)
	return *out.Wants[0].LimitSet, out.Wants[0].Refusal
}

// spent reports n compute units decided against a hold.
func spent(customer billing.CustomerID, set, id string, units int64) billing.LeaseUsage {
	u := usageInput(customer, id, units)
	u.LimitSet = &set
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
		set, _ := wanted(t, syncLease(t, w.alpha, billing.SyncLeasesParams{LeaseID: lease, Wants: []billing.LeaseWant{computeWant(customer, 1)}}))
		settleUsage(t, w.alpha, lease, outbox[:2]...)
		// The process dies with item 3 unsent and its lease's hold open; once
		// the lease stops syncing the hold is released.
		w.clock.Advance(11 * time.Second)
		balance(t, w.alpha, customer, 700, 300, 1000)
		// Its successor resends the outbox against the dead lease: the first
		// two answer their receipts, the third settles as late usage, and the
		// dead lease takes no want and renews nothing.
		outbox[2].LimitSet = &set
		out := syncLease(t, w.alpha, billing.SyncLeasesParams{LeaseID: lease, Usage: outbox, Wants: []billing.LeaseWant{computeWant(customer, 1)}})
		require.EqualValues(t, 3, out.Usage.ThroughN)
		require.Empty(t, out.Usage.Refused)
		require.NotNil(t, out.Wants[0].Error)
		require.Equal(t, billing.CodeLeaseNotHeld, out.Wants[0].Error.Code)
		require.True(t, out.Lease.ExpiresAt.Before(w.clock.Now()), "a dead lease is not renewed")
		balance(t, w.alpha, customer, 400, 600, 1000)
		again := syncLease(t, w.alpha, billing.SyncLeasesParams{LeaseID: lease, Usage: outbox})
		require.Empty(t, again.Usage.Refused)
		balance(t, w.alpha, customer, 400, 600, 1000)
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

		// A hold pins the cards in force when it was issued; usage without a
		// hold is priced by the cards in force now.
		_, params := usageMeter(t, w.alpha, 100)
		pinned := billing.CustomerID(uuid.New())
		fund(t, w.alpha, pinned, "pinned", 1000)
		lease := newLease()
		set, refusal := wanted(t, syncLease(t, w.alpha, billing.SyncLeasesParams{LeaseID: lease, Wants: []billing.LeaseWant{computeWant(pinned, 1)}}))
		require.Nil(t, refusal)
		params.RateCard.Value.Price.PerUnit.UnitAmount = 300
		_, err = w.alpha.SetMeter(t.Context(), "compute", params)
		require.NoError(t, err)
		syncLease(t, w.alpha, billing.SyncLeasesParams{LeaseID: lease, Usage: numbered(1, spent(pinned, set, "under-the-hold", 1), usageInput(pinned, "without-a-hold", 1)),
			Returns: []billing.HoldReturn{{CustomerID: pinned, LimitSet: set}}})
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

	t.Run("OR-J24-S12/two-leases-drain-one-customer-within-its-balance", func(t *testing.T) {
		w := newWorld(t)
		customer, _ := fundedCustomer(t, w)
		usageMeter(t, w.alpha, 100)
		leases := []billing.LeaseID{newLease(), newLease()}
		held := []int64{0, 0}
		var set string
		n := []int64{1, 1}
		refused := 0
		for round := 0; round < 12 && refused < 2; round++ {
			for i, lease := range leases {
				params := billing.SyncLeasesParams{LeaseID: lease, Wants: []billing.LeaseWant{computeWant(customer, 1)}}
				// A gate spends what it holds, a request of 100 at a time.
				for spend := held[i]; spend >= 100; spend -= 100 {
					params.Usage = append(params.Usage, numbered(n[i], spent(customer, set, fmt.Sprintf("%d-%d", i, n[i]), 1))...)
					n[i]++
				}
				out := syncLease(t, w.alpha, params)
				require.Empty(t, out.Usage.Refused)
				var refusal *billing.Refusal
				set, refusal = wanted(t, out)
				b, err := w.alpha.GetBalance(t.Context(), customer, "USD")
				require.NoError(t, err)
				require.GreaterOrEqual(t, b.AvailableAmount, int64(0), "no hold takes the balance past what it holds")
				if refusal != nil {
					require.Contains(t, []string{billing.DenyCapacityHeld, "insufficient_balance"}, refusal.DenyCode)
					refused++
					held[i] = 0
					continue
				}
				h, ok := heldIn(out, customer, set, "balance")
				require.True(t, ok)
				require.GreaterOrEqual(t, h, int64(100), "a wanted hold covers one request")
				if b.AvailableAmount+h < 200 {
					require.EqualValues(t, 100, h, "near the balance a hold shrinks to exactly the wanted units")
				}
				held[i] = h
			}
		}
		require.Positive(t, refused, "the balance runs out")
		b, err := w.alpha.GetBalance(t.Context(), customer, "USD")
		require.NoError(t, err)
		require.LessOrEqual(t, b.HeldAmount, b.BalanceAmount, "settled and held never pass the balance")
	})

	t.Run("OR-J24-S13/capacity-another-lease-holds-is-capacity-held-not-limit-reached", func(t *testing.T) {
		w := newWorld(t)
		applyLimits(t, w.alpha, unitPlan)
		customer := billing.CustomerID(uuid.New())
		grantLimitProduct(t, w.alpha, customer, "plan", 24)
		_, err := w.alpha.UpdateCustomer(t.Context(), customer, billing.UpdateCustomerParams{Limits: []billing.LimitOverride{{Limit: "quota", Capacity: 10}}})
		require.NoError(t, err)
		first, second := newLease(), newLease()
		// The first lease's hold grows with its wants to half what is free.
		var set string
		var h int64
		for _, want := range []int64{2, 4, 5} {
			out := syncLease(t, w.alpha, billing.SyncLeasesParams{LeaseID: first, Wants: []billing.LeaseWant{workWant(customer, 2)}})
			var refusal *billing.Refusal
			set, refusal = wanted(t, out)
			require.Nil(t, refusal)
			h, _ = heldIn(out, customer, set, "quota")
			require.EqualValues(t, want, h)
		}
		require.EqualValues(t, 5, customerLimit(t, w.alpha, customer, "quota").Held)

		// Seven would fit were the first lease's hold returned; eleven would not.
		_, refusal := wanted(t, syncLease(t, w.alpha, billing.SyncLeasesParams{LeaseID: second, Wants: []billing.LeaseWant{workWant(customer, 7)}}))
		require.NotNil(t, refusal)
		require.Equal(t, billing.DenyCapacityHeld, refusal.DenyCode)
		require.NotNil(t, refusal.RetryAfterSeconds)
		require.EqualValues(t, 1, *refusal.RetryAfterSeconds)
		_, refusal = wanted(t, syncLease(t, w.alpha, billing.SyncLeasesParams{LeaseID: second, Wants: []billing.LeaseWant{workWant(customer, 11)}}))
		require.NotNil(t, refusal)
		require.Equal(t, billing.DenyLimitReached, refusal.DenyCode)
		require.Equal(t, "quota", *refusal.Limit)

		// The first lease's next sync tells its gate the smaller size; the one
		// after releases it, and the second lease gets it.
		out := syncLease(t, w.alpha, billing.SyncLeasesParams{LeaseID: first})
		h, listed := heldIn(out, customer, set, "quota")
		require.True(t, listed)
		require.Zero(t, h, "an idle hold another lease waits for gives back what no ticket took")
		syncLease(t, w.alpha, billing.SyncLeasesParams{LeaseID: first})
		require.Zero(t, customerLimit(t, w.alpha, customer, "quota").Held)
		set2, refusal := wanted(t, syncLease(t, w.alpha, billing.SyncLeasesParams{LeaseID: second, Wants: []billing.LeaseWant{workWant(customer, 7)}}))
		require.Nil(t, refusal)
		require.Equal(t, set, set2, "one customer's invokers with the same limits share a limit set")
		require.EqualValues(t, 7, customerLimit(t, w.alpha, customer, "quota").Held)
	})

	t.Run("OR-J24-S14/an-occasional-customer-is-decided-locally-and-its-idle-holds-come-back", func(t *testing.T) {
		w := newWorld(t)
		applyLimits(t, w.alpha, unitPlan)
		customer := billing.CustomerID(uuid.New())
		grantLimitProduct(t, w.alpha, customer, "plan", 24)
		gates := make([]billing.LeaseID, 10)
		var set string
		for i := range gates {
			gates[i] = newLease()
			out := syncLease(t, w.alpha, billing.SyncLeasesParams{LeaseID: gates[i], Wants: []billing.LeaseWant{workWant(customer, 1)}})
			var refusal *billing.Refusal
			set, refusal = wanted(t, out)
			require.Nil(t, refusal)
			h, _ := heldIn(out, customer, set, "quota")
			require.EqualValues(t, 1, h, "each process holds the customer's next request")
		}
		// Idle syncs keep one request's worth while the customer is far from
		// its limit: the next request on each process is decided locally.
		for _, gate := range gates {
			out := syncLease(t, w.alpha, billing.SyncLeasesParams{LeaseID: gate})
			_, listed := heldIn(out, customer, set, "quota")
			require.False(t, listed, "an idle hold far from the limit does not change")
		}
		require.EqualValues(t, 10, customerLimit(t, w.alpha, customer, "quota").Held)

		// Near the limit each process's next sync takes its idle hold back.
		w.clock.Advance(time.Second)
		heavy := billing.LeaseUsage{CustomerID: customer, Invoker: customer.String(), Currency: "USD", EventType: "work", Dimensions: map[string]int64{"units": 95}, Source: "critical", SourceID: "batch"}
		settleUsage(t, w.alpha, newLease(), numbered(1, heavy)...)
		for _, gate := range gates {
			out := syncLease(t, w.alpha, billing.SyncLeasesParams{LeaseID: gate})
			h, listed := heldIn(out, customer, set, "quota")
			require.True(t, listed)
			require.Zero(t, h)
		}
		for _, gate := range gates {
			syncLease(t, w.alpha, billing.SyncLeasesParams{LeaseID: gate})
		}
		require.Zero(t, customerLimit(t, w.alpha, customer, "quota").Held)
	})

	t.Run("OR-J24-S15/a-hold-grows-with-its-traffic", func(t *testing.T) {
		w := newWorld(t)
		customer := billing.CustomerID(uuid.New())
		fund(t, w.alpha, customer, "funded", 1_000_000)
		usageMeter(t, w.alpha, 100)
		lease := newLease()
		var set string
		var held int64
		n := int64(1)
		// Traffic grows to ten requests a sync: the gate wants whenever its
		// hold runs dry, and stops waiting within a few syncs.
		syncs := 0
		for ; syncs < 8; syncs++ {
			params := billing.SyncLeasesParams{LeaseID: lease}
			for spend := min(held, 1000); spend >= 100; spend -= 100 {
				params.Usage = append(params.Usage, numbered(n, spent(customer, set, fmt.Sprintf("r-%d", n), 1))...)
				n++
			}
			if held < 1000 {
				params.Wants = []billing.LeaseWant{computeWant(customer, 1)}
			}
			out := syncLease(t, w.alpha, params)
			require.Empty(t, out.Usage.Refused)
			if len(params.Wants) == 0 {
				break
			}
			set, _ = wanted(t, out)
			h, ok := heldIn(out, customer, set, "balance")
			require.True(t, ok)
			require.Greater(t, h, held/2, "a hold that ran dry grows")
			held = h
		}
		require.LessOrEqual(t, syncs, 6, "no setting changed, and the gate stopped waiting")
	})

	t.Run("OR-J24-S16/a-closed-lease-settles-returns-its-holds-and-takes-no-want", func(t *testing.T) {
		w := newWorld(t)
		customer, _ := fundedCustomer(t, w)
		usageMeter(t, w.alpha, 100)
		lease := newLease()
		var set string
		for range 2 {
			var refusal *billing.Refusal
			set, refusal = wanted(t, syncLease(t, w.alpha, billing.SyncLeasesParams{LeaseID: lease, Wants: []billing.LeaseWant{computeWant(customer, 1)}}))
			require.Nil(t, refusal)
		}
		balance(t, w.alpha, customer, 800, 200, 1000)

		// A sync with close settles its usage, then returns every hold and
		// ends the lease, long before it would have expired.
		out := syncLease(t, w.alpha, billing.SyncLeasesParams{LeaseID: lease, Usage: numbered(1, spent(customer, set, "last", 1)), Close: true})
		require.EqualValues(t, 1, out.Usage.ThroughN)
		require.Empty(t, out.Usage.Refused)
		h, listed := heldIn(out, customer, set, "balance")
		require.True(t, listed)
		require.Zero(t, h)
		require.False(t, out.Lease.ExpiresAt.After(w.clock.Now()), "a closed lease has ended")
		balance(t, w.alpha, customer, 900, 100, 1000)

		// A closed lease takes no want; usage against it still settles, in full.
		out = syncLease(t, w.alpha, billing.SyncLeasesParams{LeaseID: lease, Usage: numbered(2, spent(customer, set, "late", 1)), Wants: []billing.LeaseWant{computeWant(customer, 1)}})
		require.EqualValues(t, 2, out.Usage.ThroughN)
		require.Empty(t, out.Usage.Refused)
		require.NotNil(t, out.Wants[0].Error)
		require.Equal(t, http.StatusConflict, out.Wants[0].Status)
		require.Equal(t, billing.CodeLeaseNotHeld, out.Wants[0].Error.Code)
		require.Empty(t, out.Holds)
		balance(t, w.alpha, customer, 800, 200, 1000)
		again := syncLease(t, w.alpha, billing.SyncLeasesParams{LeaseID: lease, Close: true})
		require.EqualValues(t, 2, again.Usage.ThroughN)
		require.Empty(t, again.Holds)
		require.Equal(t, out.Lease.ExpiresAt, again.Lease.ExpiresAt, "a lease closes once")
	})
}
