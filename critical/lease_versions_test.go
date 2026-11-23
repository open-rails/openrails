//go:build integration

package critical_test

import (
	"bytes"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/modules/limits"
	"github.com/open-rails/openrails/internal/retention"
	"github.com/stretchr/testify/require"
)

// crossingQuota counts work units toward a daily quota that admits the
// request that crosses it.
const crossingQuota = `schema_version: 1
meters:
  work: {event_type: work, value_property: units, aggregation: sum}
limits:
  quota:
    unit: units
    reset: {kind: fixed, window_seconds: 86400, anchor: first_use}
    overrun: admit_while_positive
    rate_cards: [{meter: work, price: {model: per_unit, per_unit: {unit_amount: "1"}}}]
products:
  plan: {display_name: Plan, limits: {quota: "100"}}
  metered: {display_name: Metered, rate_cards: [{meter: work, price: {model: dynamic, currency: USD}}]}
`

// burstSession counts work units toward a twenty-second session.
const burstSession = `schema_version: 1
meters:
  work: {event_type: work, value_property: units, aggregation: sum}
limits:
  burst:
    unit: units
    reset: {kind: session, window_seconds: 20}
    rate_cards: [{meter: work, price: {model: per_unit, per_unit: {unit_amount: "1"}}}]
products:
  plan: {display_name: Plan, limits: {burst: "100"}}
  metered: {display_name: Metered, rate_cards: [{meter: work, price: {model: dynamic, currency: USD}}]}
`

// quarterQuota counts work units toward a ninety-day window.
const quarterQuota = `schema_version: 1
meters:
  work: {event_type: work, value_property: units, aggregation: sum}
limits:
  quarter:
    unit: units
    reset: {kind: fixed, window_seconds: 7776000, anchor: first_use}
    rate_cards: [{meter: work, price: {model: per_unit, per_unit: {unit_amount: "1"}}}]
products:
  plan: {display_name: Plan, limits: {quarter: "100"}}
  metered: {display_name: Metered, rate_cards: [{meter: work, price: {model: dynamic, currency: USD}}]}
`

// goldTrust hold a gold trust level against its own spend window.
func goldTrust(w *world) {
	m := w.cfg.Merchants["alpha"]
	m.BillingPolicies = []billing.BillingPolicy{
		{Name: "free", Kind: "window_spend_cap", SpendWindows: []billing.BudgetWindow{{Key: "free-month", WindowSeconds: 30 * 24 * 3600, Limit: 1000, Currency: "USD"}}},
		{Name: "gold", Kind: "window_spend_cap", SpendWindows: []billing.BudgetWindow{{Key: "gold-month", WindowSeconds: 30 * 24 * 3600, Limit: 5000, Currency: "USD"}}},
	}
	m.BillingPolicyBindings = []billing.BillingPolicyBinding{{PolicyName: "free"}, {PolicyName: "gold", Tier: "gold"}}
	w.cfg.Merchants["alpha"] = m
}

// answeredHold is an answer's hold of the customer and limit set.
func answeredHold(t *testing.T, out *billing.SyncLeasesResult, customer billing.CustomerID, set string) billing.Hold {
	t.Helper()
	for _, h := range out.Holds {
		if h.CustomerID == customer && h.LimitSet == set {
			return h
		}
	}
	require.Failf(t, "hold not answered", "%s %s: %+v", customer, set, out.Holds)
	return billing.Hold{}
}

// answeredLimit is what a hold holds of a limit.
func answeredLimit(t *testing.T, h billing.Hold, limit string) billing.HoldLimit {
	t.Helper()
	for _, l := range h.Limits {
		if l.Limit == limit {
			return l
		}
	}
	require.Failf(t, "limit not held", "%s: %+v", limit, h.Limits)
	return billing.HoldLimit{}
}

// priceBound is a hold's highest marginal price of a meter.
func priceBound(t *testing.T, h billing.Hold, meter string) int64 {
	t.Helper()
	for _, b := range h.PriceBounds {
		if b.Meter == meter {
			return b.UnitAmount
		}
	}
	require.Failf(t, "meter not bounded", "%s: %+v", meter, h.PriceBounds)
	return 0
}

// workReport reports n work units for a customer.
func workReport(customer billing.CustomerID, id string, units int64) billing.LeaseUsage {
	return billing.LeaseUsage{CustomerID: customer, Invoker: customer.String(), InvokerType: billing.InvokerTypeCustomer, Currency: "USD", EventType: "work",
		Dimensions: map[string]int64{"units": units}, Source: "critical", SourceID: id}
}

// workDecided reports n work units decided against a hold.
func workDecided(customer billing.CustomerID, set, id string, units int64) billing.LeaseUsage {
	u := workReport(customer, id, units)
	u.LimitSet = &set
	return u
}

func overrideLimit(t *testing.T, w *world, customer billing.CustomerID, limit string, capacity int64) {
	t.Helper()
	_, err := w.alpha.UpdateCustomer(t.Context(), customer, billing.UpdateCustomerParams{Limits: []billing.LimitOverride{{Limit: limit, Capacity: capacity}}})
	require.NoError(t, err)
}

func TestLeaseVersions(t *testing.T) {
	t.Run("OR-J24-S17/a-smaller-size-is-released-once-its-gate-applies-it-and-never-below-its-tickets", func(t *testing.T) {
		w := newWorld(t)
		applyLimits(t, w.alpha, unitPlan)
		customer := billing.CustomerID(uuid.New())
		grantLimitProduct(t, w.alpha, customer, "plan", 24)
		overrideLimit(t, w, customer, "quota", 10)
		lease := newLease()
		out := syncLease(t, w.alpha, billing.SyncLeasesParams{LeaseID: lease, Wants: []billing.LeaseWant{workWant(customer, 4)}})
		set, refusal := wanted(t, out)
		require.Nil(t, refusal)
		first := answeredHold(t, out, customer, set)
		quota := answeredLimit(t, first, "quota")
		require.EqualValues(t, 4, quota.Held)
		require.Positive(t, first.Version)
		committed := func(n int64) []billing.HoldCommitment {
			return []billing.HoldCommitment{{CustomerID: customer, LimitSet: set, Limits: []billing.LimitCommitment{{Limit: "quota", WindowStartsAt: quota.WindowStartsAt, Committed: n}}}}
		}

		// Another lease's usage takes the customer near its limit: the hold's
		// next answer is smaller, at a new version, and its capacity stays set
		// aside until the gate applies it.
		w.clock.Advance(time.Second)
		settleUsage(t, w.alpha, newLease(), numbered(1, workReport(customer, "batch", 7))...)
		out = syncLease(t, w.alpha, billing.SyncLeasesParams{LeaseID: lease, Committed: committed(1)})
		shrunk := answeredHold(t, out, customer, set)
		require.Greater(t, shrunk.Version, first.Version)
		require.EqualValues(t, 1, answeredLimit(t, shrunk, "quota").Held, "near the limit an idle hold keeps what its tickets took")
		require.EqualValues(t, 4, customerLimit(t, w.alpha, customer, "quota").Held, "until the gate applies the answer, its capacity stays set aside")

		// The reply was lost: the next sync reports nothing applied, and the
		// answer comes again with its version.
		out = syncLease(t, w.alpha, billing.SyncLeasesParams{LeaseID: lease, Committed: committed(1), Applied: []billing.HoldApplied{}})
		again := answeredHold(t, out, customer, set)
		require.Equal(t, shrunk.Version, again.Version)
		require.EqualValues(t, 1, answeredLimit(t, again, "quota").Held)
		require.EqualValues(t, 4, customerLimit(t, w.alpha, customer, "quota").Held)

		// The gate applied it while tickets it admitted under the larger size
		// still hold 3: the release stops at what they took.
		syncLease(t, w.alpha, billing.SyncLeasesParams{LeaseID: lease, Committed: committed(3)})
		require.EqualValues(t, 3, customerLimit(t, w.alpha, customer, "quota").Held, "never below what the gate's tickets took")
		syncLease(t, w.alpha, billing.SyncLeasesParams{LeaseID: lease})
		require.EqualValues(t, 1, customerLimit(t, w.alpha, customer, "quota").Held, "once they are gone, down to the answered size")
	})

	t.Run("OR-J24-S18/a-sync-past-its-leases-lifetime-renews-nothing-and-its-holds-count-through-the-grace", func(t *testing.T) {
		w := newWorld(t)
		customer, _ := fundedCustomer(t, w)
		usageMeter(t, w.alpha, 100)
		lease := newLease()
		out := syncLease(t, w.alpha, billing.SyncLeasesParams{LeaseID: lease, Wants: []billing.LeaseWant{computeWant(customer, 1)}})
		require.True(t, out.Lease.Live)
		_, refusal := wanted(t, out)
		require.Nil(t, refusal)
		balance(t, w.alpha, customer, 900, 100, 1000)

		// Past its lifetime a sync renews nothing and says so.
		w.clock.Advance(limits.LeaseLifetime + time.Second)
		late := syncLease(t, w.alpha, billing.SyncLeasesParams{LeaseID: lease, Wants: []billing.LeaseWant{computeWant(customer, 1)}})
		require.False(t, late.Lease.Live)
		require.False(t, late.Lease.ExpiresAt.After(w.clock.Now()))
		require.NotNil(t, late.Wants[0].Error)
		require.Equal(t, billing.CodeLeaseNotHeld, late.Wants[0].Error.Code)
		require.Empty(t, late.Holds)
		// Readers count its holds through the grace, which outlasts any sync's
		// transaction: a sync that straddled its death revived nothing a
		// reader had already released.
		balance(t, w.alpha, customer, 900, 100, 1000)
		w.clock.Advance(limits.LeaseGrace)
		balance(t, w.alpha, customer, 1000, 0, 1000)
		fresh := syncLease(t, w.alpha, billing.SyncLeasesParams{LeaseID: newLease(), Wants: []billing.LeaseWant{computeWant(customer, 1)}})
		require.True(t, fresh.Lease.Live)
		_, refusal = wanted(t, fresh)
		require.Nil(t, refusal)
	})

	t.Run("OR-J24-S19/lease-ids-are-uuidv7-an-id-past-the-replay-horizon-is-not-found-and-a-usage-only-close-ends-an-earlier-lease", func(t *testing.T) {
		a := newAuthorityWorld(t)
		usageMeter(t, a.alphaRemote, 100)
		alice := billing.CustomerID(uuid.MustParse(a.alice.ID))
		fund(t, a.alphaRemote, alice, "funded", 1000)

		random := billing.LeaseID(uuid.New())
		_, err := a.alphaRemote.SyncLeases(t.Context(), billing.SyncLeasesParams{LeaseID: random})
		require.ErrorIs(t, err, billing.ErrInvalid)
		status, raw := a.request(t, http.MethodPost, "/v1/app/leases/sync", a.alphaKey.Secret, map[string]any{"lease_id": random.String()})
		require.Equal(t, http.StatusBadRequest, status, string(raw))
		require.Contains(t, string(raw), `"code":"invalid_param"`)

		// An id minted before the replay horizon names a lease that may be
		// forgotten: its usage never posts again.
		old := leaseMintedAt(a.clock.Now().Add(-retention.Leases))
		_, err = a.alphaRemote.SyncLeases(t.Context(), billing.SyncLeasesParams{LeaseID: old, Usage: numbered(1, usageInput(alice, "forgotten", 1))})
		requireCode(t, err, billing.CodeLeaseNotFound)
		balance(t, a.alphaRemote, alice, 1000, 0, 1000)

		// A process resending an earlier lease's usage with close settles it
		// and ends the lease.
		lease := newLease()
		set, refusal := wanted(t, syncLease(t, a.alphaRemote, billing.SyncLeasesParams{LeaseID: lease, Wants: []billing.LeaseWant{computeWant(alice, 1)}}))
		require.Nil(t, refusal)
		a.clock.Advance(limits.LeaseLifetime + limits.LeaseGrace + time.Second)
		closed := syncLease(t, a.alphaRemote, billing.SyncLeasesParams{LeaseID: lease, Usage: numbered(1, spent(alice, set, "outbox", 1)), Close: true})
		require.EqualValues(t, 1, closed.Usage.ThroughN)
		require.Empty(t, closed.Usage.Refused)
		require.False(t, closed.Lease.Live)
		require.False(t, closed.Lease.ExpiresAt.After(a.clock.Now()))
		balance(t, a.alphaRemote, alice, 900, 100, 1000)
		after := syncLease(t, a.alphaRemote, billing.SyncLeasesParams{LeaseID: lease, Wants: []billing.LeaseWant{computeWant(alice, 1)}})
		require.NotNil(t, after.Wants[0].Error)
		require.Equal(t, billing.CodeLeaseNotHeld, after.Wants[0].Error.Code)
	})

	t.Run("OR-J24-S20/a-renewed-hold-reads-the-rate-cards-in-force", func(t *testing.T) {
		w := newWorld(t)
		customer, _ := fundedCustomer(t, w)
		_, params := usageMeter(t, w.alpha, 100)
		lease := newLease()
		out := syncLease(t, w.alpha, billing.SyncLeasesParams{LeaseID: lease, Wants: []billing.LeaseWant{computeWant(customer, 1)}})
		set, refusal := wanted(t, out)
		require.Nil(t, refusal)
		hold := answeredHold(t, out, customer, set)
		require.EqualValues(t, 100, priceBound(t, hold, "compute"))

		// A price change makes the hold stale: its next sync issues it again
		// with the cards in force, and usage decided against it is priced so.
		params.RateCard.Value.Price.PerUnit.UnitAmount = 300
		_, err := w.alpha.SetMeter(t.Context(), "compute", params)
		require.NoError(t, err)
		out = syncLease(t, w.alpha, billing.SyncLeasesParams{LeaseID: lease})
		renewed := answeredHold(t, out, customer, set)
		require.Greater(t, renewed.Version, hold.Version)
		require.EqualValues(t, 300, priceBound(t, renewed, "compute"), "a renewal never copies the cards it was issued with")
		syncLease(t, w.alpha, billing.SyncLeasesParams{LeaseID: lease, Usage: numbered(1, spent(customer, set, "after-the-edit", 1)),
			Returns: []billing.HoldReturn{{CustomerID: customer, LimitSet: set}}})
		balance(t, w.alpha, customer, 700, 300, 1000)
	})

	t.Run("OR-J24-S21/what-a-days-usage-leaves-owed-is-dated-on-its-day", func(t *testing.T) {
		a, _, customer := usageBillingWorld(t, true, true)
		oneUsage(t, a.alpha, usageInput(customer, "late-in-the-day", 3))
		fleet := startFleet(t, a.world)
		// The day's accrual posts after midnight, before the usage-billing
		// check; the bill dated the next day bills it as the day's usage.
		postUsageDay(t, a.world, fleet)
		runUsageDue(t, fleet)
		order := oneUsageOrder(t, a, customer)
		require.EqualValues(t, 30_000_000, order.Total)
		dayEnd := time.Date(2026, 9, 24, 23, 59, 59, 999_999_000, time.UTC)
		require.True(t, dayEnd.Equal(order.Usage.PeriodStartsAt), "dated at its day's end, not at its posting: %s", order.Usage.PeriodStartsAt)
	})

	t.Run("OR-J24-S22/usage-after-a-credit-lot-lapses-never-draws-it", func(t *testing.T) {
		w := newWorld(t, func(w *world) {
			now := w.clock.Now().UTC()
			w.clock.Advance(now.Truncate(24 * time.Hour).Add(36 * time.Hour).Sub(now))
		})
		usageMeter(t, w.alpha, 100)
		customer := billing.CustomerID(uuid.New())
		expires := w.clock.Now().Add(time.Hour)
		_, err := w.alpha.CreateCreditGrants(t.Context(), []billing.CreateCreditGrantParams{{CustomerID: customer, Currency: "USD", Amount: 1000, Source: "critical", SourceID: "lapsing", ExpiresAt: &expires}})
		require.NoError(t, err)
		lease := newLease()
		settleUsage(t, w.alpha, lease, numbered(1, usageInput(customer, "while-live", 2))...)
		w.clock.Advance(2 * time.Hour)
		settleUsage(t, w.alpha, lease, numbered(2, usageInput(customer, "after-it-lapsed", 3))...)
		fleet := startFleet(t, w)
		nextDay(w)
		postDays(t, fleet)
		b, err := w.alpha.GetBalance(t.Context(), customer, "USD")
		require.NoError(t, err)
		require.Zero(t, b.BalanceAmount, "the lot paid for the usage before it lapsed and expired with the rest")
		require.EqualValues(t, 300, b.OwedAmount, "usage after the lapse is owed, never drawn from the lapsed lot")
	})

	t.Run("OR-J24-S23/a-lease-keeps-refused-items-numbers-and-codes-within-its-bound", func(t *testing.T) {
		w := newWorld(t)
		customer, _ := fundedCustomer(t, w)
		usageMeter(t, w.alpha, 100)
		lease := newLease()
		// Four syncs of a thousand refused items each: the oldest receipt goes
		// past the bound, and the lease keeps syncing.
		first := int64(1)
		for round := range 4 {
			items := make([]billing.LeaseUsage, billing.MaxSyncUsageItems)
			for i := range items {
				items[i] = usageInput(billing.CustomerID{}, fmt.Sprintf("bad-%d-%d", round, i), 1)
			}
			out := syncLease(t, w.alpha, billing.SyncLeasesParams{LeaseID: lease, Usage: numbered(first, items...)})
			require.EqualValues(t, first+int64(len(items))-1, out.Usage.ThroughN)
			require.Len(t, out.Usage.Refused, len(items))
			first += int64(len(items))
		}
		settleUsage(t, w.alpha, lease, numbered(first, usageInput(customer, "good", 1))...)
		balance(t, w.alpha, customer, 900, 100, 1000)

		// The newest refused item's resend answers its code from the receipt.
		out := syncLease(t, w.alpha, billing.SyncLeasesParams{LeaseID: lease,
			Usage: numbered(first-1, usageInput(billing.CustomerID{}, "bad-3-999", 1), usageInput(customer, "good", 1))})
		refused := refusedItem(t, out, first-1)
		require.Equal(t, http.StatusBadRequest, refused.Status)
		require.Equal(t, billing.CodeInvalidParam, refused.Error.Code)
		require.Len(t, out.Usage.Refused, 1, "the settled item answers as settled")
		// The oldest receipt is gone: its item answers as settled, and posts
		// nothing again.
		out = syncLease(t, w.alpha, billing.SyncLeasesParams{LeaseID: lease, Usage: numbered(1, usageInput(billing.CustomerID{}, "bad-0-0", 1))})
		require.Empty(t, out.Usage.Refused)
		balance(t, w.alpha, customer, 900, 100, 1000)
	})

	t.Run("OR-J24-S24/wants-answer-per-customer-and-a-host-stated-want-holds-its-estimate", func(t *testing.T) {
		w := newWorld(t)
		usageMeter(t, w.alpha, 100)
		one, _ := fundedCustomer(t, w)
		two, _ := fundedCustomer(t, w)
		lease := newLease()
		out := syncLease(t, w.alpha, billing.SyncLeasesParams{LeaseID: lease, Wants: []billing.LeaseWant{computeWant(one, 1), computeWant(two, 1)}})
		require.Len(t, out.Wants, 2)
		for _, want := range out.Wants {
			require.Nil(t, want.Error, "%+v", want.Error)
			require.Nil(t, want.Refusal)
			require.NotNil(t, want.LimitSet)
		}
		set := *out.Wants[0].LimitSet
		require.Equal(t, set, *out.Wants[1].LimitSet, "the same limits are in force for both")
		for _, c := range []billing.CustomerID{one, two} {
			h, listed := heldIn(out, c, set, "balance")
			require.True(t, listed, "each customer gets its own hold")
			require.EqualValues(t, 100, h)
			balance(t, w.alpha, c, 900, 100, 1000)
		}

		// A host-stated want holds the money it states, and may not state
		// usage too.
		stated := billing.LeaseWant{CustomerID: one, Invoker: one.String(), InvokerType: billing.InvokerTypeCustomer, Currency: "USD", EstimatedAmount: ptr(int64(250))}
		both := computeWant(one, 1)
		both.EstimatedAmount = ptr(int64(250))
		out = syncLease(t, w.alpha, billing.SyncLeasesParams{LeaseID: lease, Wants: []billing.LeaseWant{stated, both}})
		require.Len(t, out.Wants, 2)
		require.Nil(t, out.Wants[0].Error, "%+v", out.Wants[0].Error)
		require.Nil(t, out.Wants[0].Refusal)
		statedSet := *out.Wants[0].LimitSet
		require.NotEqual(t, set, statedSet, "a host-stated want's hold names no event type")
		h, listed := heldIn(out, one, statedSet, "balance")
		require.True(t, listed)
		require.EqualValues(t, 250, h)
		require.NotNil(t, out.Wants[1].Error)
		require.Equal(t, http.StatusBadRequest, out.Wants[1].Status)
		require.Equal(t, billing.CodeInvalidParam, out.Wants[1].Error.Code)
		balance(t, w.alpha, one, 650, 350, 1000)
	})

	t.Run("OR-J24-S25/a-trust-levels-policy-is-its-own-limit-set-and-its-hold-renews-as-its-invoker", func(t *testing.T) {
		w := newWorld(t, goldTrust)
		usageMeter(t, w.alpha, 100)
		customer := billing.CustomerID(uuid.New())
		fund(t, w.alpha, customer, "funded", 10_000)
		plain := computeWant(customer, 1)
		plain.Invoker = "apiKey:plain"
		gold := computeWant(customer, 1)
		gold.Invoker, gold.TrustLevel = "apiKey:gold", ptr("gold")
		lease := newLease()
		out := syncLease(t, w.alpha, billing.SyncLeasesParams{LeaseID: lease, Wants: []billing.LeaseWant{plain, gold}})
		require.Len(t, out.Wants, 2)
		plainSet, goldSet := *out.Wants[0].LimitSet, *out.Wants[1].LimitSet
		require.NotEqual(t, plainSet, goldSet, "another trust level's policy is another limit set")

		// Usage on the gold hold renews it: resolved again as its gold invoker,
		// it is the same limit set and is topped up.
		out = syncLease(t, w.alpha, billing.SyncLeasesParams{LeaseID: lease, Usage: numbered(1, spent(customer, goldSet, "gold-work", 1))})
		h, listed := heldIn(out, customer, goldSet, "balance")
		require.True(t, listed)
		require.Positive(t, h, "a renewal resolves as the invoker the hold was resolved for")
	})

	t.Run("OR-J24-S26/a-renewal-never-opens-a-session", func(t *testing.T) {
		w := newWorld(t)
		applyLimits(t, w.alpha, burstSession)
		customer := billing.CustomerID(uuid.New())
		grantLimitProduct(t, w.alpha, customer, "plan", 24)
		lease := newLease()
		set, refusal := wanted(t, syncLease(t, w.alpha, billing.SyncLeasesParams{LeaseID: lease, Wants: []billing.LeaseWant{workWant(customer, 2)}}))
		require.Nil(t, refusal)
		opened := customerLimit(t, w.alpha, customer, "burst").WindowStartsAt
		require.NotNil(t, opened, "a want opens the session")
		for range 3 {
			w.clock.Advance(5 * time.Second)
			syncLease(t, w.alpha, billing.SyncLeasesParams{LeaseID: lease})
		}
		// The session ends while the lease lives. Usage from it settles there,
		// and the renewal that follows answers the hold empty rather than open
		// another session.
		w.clock.Advance(5 * time.Second)
		last := workDecided(customer, set, "in-the-session", 1)
		last.OccurredAt = opened.Add(19 * time.Second)
		out := syncLease(t, w.alpha, billing.SyncLeasesParams{LeaseID: lease, Usage: numbered(1, last)})
		require.Empty(t, out.Usage.Refused)
		h, listed := heldIn(out, customer, set, "burst")
		require.True(t, listed)
		require.Zero(t, h)
		require.Nil(t, customerLimit(t, w.alpha, customer, "burst").WindowStartsAt, "only a want or a use opens a session")
		_, refusal = wanted(t, syncLease(t, w.alpha, billing.SyncLeasesParams{LeaseID: lease, Wants: []billing.LeaseWant{workWant(customer, 2)}}))
		require.Nil(t, refusal)
		require.NotNil(t, customerLimit(t, w.alpha, customer, "burst").WindowStartsAt, "the next want opens the next session")
	})

	t.Run("OR-J24-S27/admit-while-positive-wants-only-capacity-free-beyond-their-tickets", func(t *testing.T) {
		w := newWorld(t)
		applyLimits(t, w.alpha, crossingQuota)
		customer := billing.CustomerID(uuid.New())
		grantLimitProduct(t, w.alpha, customer, "plan", 24)
		overrideLimit(t, w, customer, "quota", 10)
		first, second := newLease(), newLease()
		var set string
		for _, want := range []int64{2, 4, 5} {
			out := syncLease(t, w.alpha, billing.SyncLeasesParams{LeaseID: first, Wants: []billing.LeaseWant{workWant(customer, 2)}})
			var refusal *billing.Refusal
			set, refusal = wanted(t, out)
			require.Nil(t, refusal)
			h, _ := heldIn(out, customer, set, "quota")
			require.EqualValues(t, want, h)
		}

		// Less is free than the request costs, but some is: the request is
		// admitted, and may cross the limit.
		out := syncLease(t, w.alpha, billing.SyncLeasesParams{LeaseID: second, Wants: []billing.LeaseWant{workWant(customer, 7)}})
		_, refusal := wanted(t, out)
		require.Nil(t, refusal, "admit_while_positive takes any capacity free beyond its tickets")
		held, _ := heldIn(out, customer, set, "quota")
		require.EqualValues(t, 5, held, "it holds what is free")
		window := answeredLimit(t, answeredHold(t, out, customer, set), "quota").WindowStartsAt

		// With nothing free beyond what its tickets took, capacity the first
		// lease holds is capacity_held, not limit_reached.
		committed := []billing.HoldCommitment{{CustomerID: customer, LimitSet: set, Limits: []billing.LimitCommitment{{Limit: "quota", WindowStartsAt: window, Committed: 5}}}}
		_, refusal = wanted(t, syncLease(t, w.alpha, billing.SyncLeasesParams{LeaseID: second, Wants: []billing.LeaseWant{workWant(customer, 1)}, Committed: committed}))
		require.NotNil(t, refusal)
		require.Equal(t, billing.DenyCapacityHeld, refusal.DenyCode)
	})

	t.Run("OR-J24-S28/no-hold-holds-past-the-30-day-hold-lifetime", func(t *testing.T) {
		w := newWorld(t)
		applyLimits(t, w.alpha, quarterQuota)
		customer := billing.CustomerID(uuid.New())
		grantLimitProduct(t, w.alpha, customer, "plan", 24*120)
		require.True(t, admitOne(t, w.alpha, usageAdmission(w, customer, "abandoned", "work", map[string]int64{"units": 5}, nil)).Allowed())
		require.EqualValues(t, 5, customerLimit(t, w.alpha, customer, "quarter").Held)
		// Past its deadline an abandoned admission still counts in its window,
		// but no hold row holds past the hold lifetime.
		w.clock.Advance(29 * 24 * time.Hour)
		require.EqualValues(t, 5, customerLimit(t, w.alpha, customer, "quarter").Held)
		w.clock.Advance(2 * 24 * time.Hour)
		require.Zero(t, customerLimit(t, w.alpha, customer, "quarter").Held)
	})

	t.Run("OR-J24-S29/concurrent-syncs-and-admissions-on-the-same-customers-never-deadlock", func(t *testing.T) {
		w := newWorld(t)
		applyLimits(t, w.alpha, unitPlan)
		customers := []billing.CustomerID{billing.CustomerID(uuid.New()), billing.CustomerID(uuid.New()), billing.CustomerID(uuid.New())}
		for _, c := range customers {
			grantLimitProduct(t, w.alpha, c, "plan", 24)
			overrideLimit(t, w, c, "quota", 10)
		}
		var wg sync.WaitGroup
		errs := make(chan error, 64)
		for g := range 4 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				lease := newLease()
				var versions []billing.HoldApplied
				for round := range 5 {
					// Each gate wants its customers in its own order, and an
					// admission marks the gates' holds contended meanwhile.
					wants := make([]billing.LeaseWant, 0, len(customers))
					for i := range customers {
						wants = append(wants, workWant(customers[(g+i+round)%len(customers)], 3))
					}
					out, err := w.alpha.SyncLeases(t.Context(), billing.SyncLeasesParams{LeaseID: lease, Wants: wants, Applied: versions})
					if err != nil {
						errs <- err
						return
					}
					versions = appliedOf(out)
					admission := usageAdmission(w, customers[(g+round)%len(customers)], fmt.Sprintf("race-%d-%d", g, round), "work", map[string]int64{"units": 4}, nil)
					if _, err := w.alpha.Admit(t.Context(), []billing.AdmitParams{admission}); err != nil {
						errs <- err
						return
					}
				}
			}()
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			require.NoError(t, err)
		}
		for _, c := range customers {
			quota := customerLimit(t, w.alpha, c, "quota")
			require.LessOrEqual(t, quota.Used+quota.Held, quota.Capacity, "no hold takes the limit past its capacity")
		}
	})
}

func TestLimitArchive(t *testing.T) {
	t.Run("OR-J31-S14/open-limit-windows-and-staff-adjustments-move-with-the-book", func(t *testing.T) {
		source := newWorld(t)
		applyLimits(t, source.alpha, `schema_version: 1
limits:
  ever: {unit: requests, reset: {kind: lifetime}}
  monthly: {unit: requests, reset: {kind: fixed, window_seconds: 2592000, anchor: first_use}}
products:
  plan: {display_name: Plan, limits: {ever: "1000", monthly: "100"}}
`)
		customer := billing.CustomerID(uuid.New())
		grantLimitProduct(t, source.alpha, customer, "plan", 24*30)
		_, err := source.alpha.AdjustCustomerLimit(t.Context(), customer, billing.AdjustLimitParams{Limit: "ever", Amount: 500, SourceID: "ever-bonus"})
		require.NoError(t, err)
		_, err = source.alpha.AdjustCustomerLimit(t.Context(), customer, billing.AdjustLimitParams{Limit: "monthly", Amount: 50, SourceID: "monthly-bonus"})
		require.NoError(t, err)
		ever, monthly := customerLimit(t, source.alpha, customer, "ever"), customerLimit(t, source.alpha, customer, "monthly")
		require.EqualValues(t, 1500, ever.Capacity)
		require.EqualValues(t, 150, monthly.Capacity)

		archive := archiveBytes(t, source.alpha)
		_, target := archiveTarget(t, source)
		_, err = target.ImportBillingArchive(t.Context(), bytes.NewReader(archive))
		require.NoError(t, err)
		require.Equal(t, ever, customerLimit(t, target, customer, "ever"), "a lifetime limit's window moves with its adjustment")
		require.Equal(t, monthly, customerLimit(t, target, customer, "monthly"), "an open window moves with its adjustment")
		retried, err := target.AdjustCustomerLimit(t.Context(), customer, billing.AdjustLimitParams{Limit: "ever", Amount: 500, SourceID: "ever-bonus"})
		require.NoError(t, err)
		require.Equal(t, ever, retried, "a staff adjustment's retry adds nothing at the destination")
	})
}
