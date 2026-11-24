//go:build integration

package critical_test

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
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
    counts: [{meter: work}]
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
    counts: [{meter: work}]
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
    counts: [{meter: work}]
products:
  plan: {display_name: Plan, limits: {quarter: "100"}}
  metered: {display_name: Metered, rate_cards: [{meter: work, price: {model: dynamic, currency: USD}}]}
`

// minuteQuota counts work units toward one-minute windows on the epoch's
// tiles: every customer's window closes at the same instant.
const minuteQuota = `schema_version: 1
meters:
  work: {event_type: work, value_property: units, aggregation: sum}
limits:
  minute:
    unit: units
    reset: {kind: fixed, window_seconds: 60, anchor: subscription}
    counts: [{meter: work}]
products:
  metered: {display_name: Metered, rate_cards: [{meter: work, price: {model: dynamic, currency: USD}}]}
`

// lapsePlan counts work units toward a limit of each reset kind a window
// row stands for: a daily first-use quota, a week tiled from the epoch, a
// rolling hour, an hour's session and a lifetime allowance.
const lapsePlan = `schema_version: 1
meters:
  work: {event_type: work, value_property: units, aggregation: sum}
limits:
  quota:
    unit: units
    reset: {kind: fixed, window_seconds: 86400, anchor: first_use}
    counts: [{meter: work}]
  weekly:
    unit: units
    reset: {kind: fixed, window_seconds: 604800, anchor: subscription}
    counts: [{meter: work}]
  recent:
    unit: units
    reset: {kind: rolling, window_seconds: 3600}
    counts: [{meter: work}]
  visit:
    unit: units
    reset: {kind: session, window_seconds: 3600}
    counts: [{meter: work}]
  ever:
    unit: units
    reset: {kind: lifetime}
    counts: [{meter: work}]
products:
  plan: {display_name: Plan, limits: {quota: "100", weekly: "500", recent: "100", visit: "100", ever: "1000"}}
  metered: {display_name: Metered, rate_cards: [{meter: work, price: {model: dynamic, currency: USD}}]}
`

// sameWindowTrust caps one spend window apart by trust level.
func sameWindowTrust(w *world) {
	m := cfgMerchant(w.cfg, "alpha")
	m.BillingPolicies = []billing.BillingPolicy{
		{Name: "free", Kind: "window_spend_cap", SpendWindows: []billing.BudgetWindow{{Key: "month", WindowSeconds: 30 * 24 * 3600, Limit: 1000, Currency: "USD"}}},
		{Name: "gold", Kind: "window_spend_cap", SpendWindows: []billing.BudgetWindow{{Key: "month", WindowSeconds: 30 * 24 * 3600, Limit: 5000, Currency: "USD"}}},
	}
	m.BillingPolicyBindings = []billing.BillingPolicyBinding{{PolicyName: "free"}, {PolicyName: "gold", Tier: "gold"}}
	setCfgMerchant(&w.cfg, "alpha", m)
}

// goldTrust hold a gold trust level against its own spend window.
func goldTrust(w *world) {
	m := cfgMerchant(w.cfg, "alpha")
	m.BillingPolicies = []billing.BillingPolicy{
		{Name: "free", Kind: "window_spend_cap", SpendWindows: []billing.BudgetWindow{{Key: "free-month", WindowSeconds: 30 * 24 * 3600, Limit: 1000, Currency: "USD"}}},
		{Name: "gold", Kind: "window_spend_cap", SpendWindows: []billing.BudgetWindow{{Key: "gold-month", WindowSeconds: 30 * 24 * 3600, Limit: 5000, Currency: "USD"}}},
	}
	m.BillingPolicyBindings = []billing.BillingPolicyBinding{{PolicyName: "free"}, {PolicyName: "gold", Tier: "gold"}}
	setCfgMerchant(&w.cfg, "alpha", m)
}

// boundOf is a pricing's highest marginal price of a meter.
func boundOf(t *testing.T, out *billing.SyncLeasesResult, meter string) int64 {
	t.Helper()
	for _, p := range out.Pricing {
		for _, b := range p.PriceBounds {
			if b.Meter == meter {
				return b.UnitAmount
			}
		}
	}
	require.Failf(t, "meter not bounded", "%s: %+v", meter, out.Pricing)
	return 0
}

// workReport reports n work units for a customer.
func workReport(customer billing.CustomerID, id string, units int64) billing.LeaseUsage {
	return billing.LeaseUsage{CustomerID: customer, Invoker: customer.String(), InvokerType: billing.InvokerTypeCustomer, Currency: "USD", EventType: "work",
		Dimensions: map[string]int64{"units": units}, Source: "critical", SourceID: id}
}

func overrideLimit(t *testing.T, w *world, customer billing.CustomerID, limit string, capacity int64) {
	t.Helper()
	_, err := w.alpha.UpdateCustomer(t.Context(), customer, billing.UpdateCustomerParams{Limits: []billing.LimitOverride{{Limit: limit, Capacity: capacity}}})
	require.NoError(t, err)
}

func TestLeaseVersions(t *testing.T) {
	t.Run("OR-J24-S18/a-lapsed-leases-report-and-holds-count-nowhere-until-it-syncs-again", func(t *testing.T) {
		w := newWorld(t)
		customer, _ := fundedCustomer(t, w)
		usageMeter(t, w.alpha, 100)
		lease := newLease()
		report := []billing.InFlight{{CustomerID: customer, Limit: "balance", Unit: "USD", Amount: 200}}
		syncLease(t, w.alpha, billing.SyncLeasesParams{LeaseID: lease, InFlight: report})
		require.True(t, leaseAdmit(t, w, lease, usageAdmission(w, customer, "held", "compute", map[string]int64{"units": 1}, nil)).Allowed())
		balance(t, w.alpha, customer, 700, 300, 1000)

		// Past its lifetime the lease's report and hold count nowhere.
		w.clock.Advance(limits.LeaseLifetime + time.Second)
		balance(t, w.alpha, customer, 1000, 0, 1000)
		// Its next sync makes it live again: both count.
		out := syncLease(t, w.alpha, billing.SyncLeasesParams{LeaseID: lease, InFlight: report})
		require.True(t, out.Lease.ExpiresAt.After(w.clock.Now()))
		balance(t, w.alpha, customer, 700, 300, 1000)
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
		syncLease(t, a.alphaRemote, billing.SyncLeasesParams{LeaseID: lease, Views: []billing.ViewParams{computeView(alice)}})
		a.clock.Advance(limits.LeaseLifetime + time.Second)
		closed := syncLease(t, a.alphaRemote, billing.SyncLeasesParams{LeaseID: lease, Usage: numbered(1, usageInput(alice, "outbox", 1)), Close: true})
		require.EqualValues(t, 1, closed.Usage.ThroughN)
		require.Empty(t, closed.Usage.Refused)
		require.False(t, closed.Lease.ExpiresAt.After(a.clock.Now()))
		balance(t, a.alphaRemote, alice, 900, 100, 1000)
		lapsed := alice.String()
		after, err := a.alphaRemote.Admit(t.Context(), []billing.AdmitParams{{RequestID: "after-close", CustomerID: alice, Invoker: lapsed, InvokerType: billing.InvokerTypeCustomer,
			Currency: "USD", EstimatedAmount: 1, LeaseID: &lease}})
		require.NoError(t, err)
		require.NotNil(t, after[0].Error)
		require.Equal(t, billing.CodeLeaseNotHeld, after[0].Error.Code)
	})

	t.Run("OR-J24-S20/views-price-with-the-cards-in-force-and-gate-usage-settles-at-them", func(t *testing.T) {
		w := newWorld(t)
		customer, _ := fundedCustomer(t, w)
		_, params := usageMeter(t, w.alpha, 100)
		lease := newLease()
		out := syncLease(t, w.alpha, billing.SyncLeasesParams{LeaseID: lease, Views: []billing.ViewParams{computeView(customer)}})
		require.EqualValues(t, 100, boundOf(t, out, "compute"))

		// A price change reaches the gate with its next view; usage admitted
		// before it and settled after is priced by the cards in force.
		params.RateCard.Value.Price.PerUnit.UnitAmount = 300
		_, err := w.alpha.SetMeter(t.Context(), "compute", params)
		require.NoError(t, err)
		admitted := usageInput(customer, "before-the-edit", 1)
		admitted.AdmittedAt = ptr(w.clock.Now().Add(-time.Second))
		out = syncLease(t, w.alpha, billing.SyncLeasesParams{LeaseID: lease, Usage: numbered(1, admitted), Views: []billing.ViewParams{computeView(customer)}})
		require.Empty(t, out.Usage.Refused)
		require.EqualValues(t, 300, boundOf(t, out, "compute"))
		require.EqualValues(t, 300, lineOf(t, viewOf(t, out, customer), "balance").Settled, "the view shows what the sync's own usage settled")
		balance(t, w.alpha, customer, 700, 300, 1000)
	})
	t.Run("OR-J24-S21/what-a-days-usage-leaves-owed-is-dated-on-its-day", func(t *testing.T) {
		a, _, customer := usageBillingWorld(t, true, true)
		oneUsage(t, a.alpha, usageInput(customer, "late-in-the-day", 3))
		fleet := startFleet(t, a.world)
		// The month's close posts it, before the usage-billing check: what it
		// leaves owed is dated before its pricing period ended, so the bill
		// of that period bills it.
		postUsageMonth(t, a.world, fleet)
		runUsageDue(t, fleet)
		order := oneUsageOrder(t, a, customer)
		require.EqualValues(t, 30_000_000, order.Total)
		dayEnd := time.Date(2026, 9, 24, 23, 59, 59, 999_999_000, time.UTC)
		require.True(t, dayEnd.Equal(owedAccruedAt(t, a.world, order.ID)), "dated at its day's end, not at its posting")
		require.True(t, order.Usage.PeriodStartsAt.Before(dayEnd), "the order's period starts with the usage's: %s", order.Usage.PeriodStartsAt)
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
		nextMonth(w)
		closeMonths(t, fleet)
		b, err := w.alpha.GetBalance(t.Context(), customer, billing.GetBalanceParams{Currency: "USD"})
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

	t.Run("OR-J24-S24/views-answer-per-customer-and-a-host-stated-set-claims-only-money", func(t *testing.T) {
		w := newWorld(t)
		applyLimits(t, w.alpha, unitPlan)
		usageMeter(t, w.alpha, 100)
		one, _ := fundedCustomer(t, w)
		two, _ := fundedCustomer(t, w)
		for _, c := range []billing.CustomerID{one, two} {
			grantLimitProduct(t, w.alpha, c, "plan", 24)
		}
		stated := billing.ViewParams{CustomerID: one, Invoker: one.String(), InvokerType: billing.InvokerTypeCustomer, Currency: "USD"}
		out := syncLease(t, w.alpha, billing.SyncLeasesParams{LeaseID: newLease(), Views: []billing.ViewParams{workView(one), workView(two), stated}})
		require.Len(t, out.Views, 2, "one view per customer and currency")
		for i, c := range []billing.CustomerID{one, two} {
			set := out.LimitSets[i]
			require.NotNil(t, set)
			require.Nil(t, set.Refusal)
			require.Equal(t, "balance", set.Limits[0])
			require.Contains(t, set.Limits, "quota")
			require.Equal(t, billing.DenyInsufficientBalance, set.BalanceDenyCode)
			require.EqualValues(t, 1000, lineOf(t, viewOf(t, out, c), "balance").Remaining)
			require.EqualValues(t, 100, lineOf(t, viewOf(t, out, c), "quota").Remaining)
		}
		require.NotContains(t, out.LimitSets[2].Limits, "quota", "a host-stated request claims no named-unit limit")
		balance(t, w.alpha, one, 1000, 0, 1000)
	})
	t.Run("OR-J24-S25/a-trust-levels-policy-is-its-own-limit-set-and-usage-counts-in-its-window", func(t *testing.T) {
		w := newWorld(t, goldTrust)
		usageMeter(t, w.alpha, 100)
		customer := billing.CustomerID(uuid.New())
		fund(t, w.alpha, customer, "funded", 10_000)
		plain := computeView(customer)
		plain.Invoker = "apiKey:plain"
		gold := computeView(customer)
		gold.Invoker, gold.TrustLevel = "apiKey:gold", ptr("gold")
		lease := newLease()
		out := syncLease(t, w.alpha, billing.SyncLeasesParams{LeaseID: lease, Views: []billing.ViewParams{plain, gold}})
		plainSet, goldSet := out.LimitSets[0], out.LimitSets[1]
		require.Len(t, plainSet.Limits, 2)
		require.Len(t, goldSet.Limits, 2)
		require.NotEqual(t, plainSet.Limits[1], goldSet.Limits[1], "another trust level's policy claims its own spend window")

		// Usage reported with the gold invoker's attributes counts in the gold
		// window, not the free one.
		work := usageInput(customer, "gold-work", 3)
		work.Invoker, work.TrustLevel = "apiKey:gold", ptr("gold")
		out = syncLease(t, w.alpha, billing.SyncLeasesParams{LeaseID: lease, Usage: numbered(1, work), Views: []billing.ViewParams{plain, gold}})
		require.Empty(t, out.Usage.Refused)
		v := viewOf(t, out, customer)
		require.EqualValues(t, 300, lineOf(t, v, goldSet.Limits[1]).Used)
		require.Zero(t, lineOf(t, v, plainSet.Limits[1]).Used)
	})
	t.Run("OR-J24-S26/a-view-never-opens-a-session-and-the-first-use-opens-it-at-its-admission", func(t *testing.T) {
		w := newWorld(t)
		applyLimits(t, w.alpha, burstSession)
		customer := billing.CustomerID(uuid.New())
		grantLimitProduct(t, w.alpha, customer, "plan", 24)
		lease := newLease()
		out := syncLease(t, w.alpha, billing.SyncLeasesParams{LeaseID: lease, Views: []billing.ViewParams{workView(customer)}})
		line := lineOf(t, viewOf(t, out, customer), "burst")
		require.Nil(t, line.WindowStartsAt, "a view opens no session")
		require.EqualValues(t, 100, line.Remaining)
		require.Nil(t, customerLimit(t, w.alpha, customer, "burst").WindowStartsAt)

		// The first use settles a few seconds after its admission: the session
		// opens at the admission.
		admitted := w.clock.Now().UTC().Truncate(time.Second)
		w.clock.Advance(3 * time.Second)
		first := workReport(customer, "first", 2)
		first.AdmittedAt = &admitted
		out = syncLease(t, w.alpha, billing.SyncLeasesParams{LeaseID: lease, Usage: numbered(1, first), Views: []billing.ViewParams{workView(customer)}})
		require.Empty(t, out.Usage.Refused)
		line = lineOf(t, viewOf(t, out, customer), "burst")
		require.NotNil(t, line.WindowStartsAt)
		require.True(t, admitted.Equal(*line.WindowStartsAt), "opened at %s, admitted at %s", line.WindowStartsAt, admitted)
		require.EqualValues(t, 98, line.Remaining)

		// Usage admitted in it and settled after it ended counts there, and
		// opens no other.
		w.clock.Advance(20 * time.Second)
		late := workReport(customer, "late", 1)
		late.AdmittedAt = ptr(admitted.Add(10 * time.Second))
		syncLease(t, w.alpha, billing.SyncLeasesParams{LeaseID: lease, Usage: numbered(2, late)})
		require.Nil(t, customerLimit(t, w.alpha, customer, "burst").WindowStartsAt, "the session ended and none opened")
	})
	t.Run("OR-J24-S27/admit-while-positive-admits-the-crossing-request-once-on-the-server", func(t *testing.T) {
		w := newWorld(t)
		applyLimits(t, w.alpha, crossingQuota)
		customer := billing.CustomerID(uuid.New())
		grantLimitProduct(t, w.alpha, customer, "plan", 24)
		overrideLimit(t, w, customer, "quota", 10)
		lease := newLease()
		syncLease(t, w.alpha, billing.SyncLeasesParams{LeaseID: lease, Usage: numbered(1, workReport(customer, "most", 8))})
		work := func(id string, units int64) billing.AdmitParams {
			return usageAdmission(w, customer, id, "work", map[string]int64{"units": units}, nil)
		}
		// Two left: the request of five crosses, and is admitted once; the
		// next finds nothing left.
		require.True(t, leaseAdmit(t, w, lease, work("crossing", 5)).Allowed())
		requireDenied(t, leaseAdmit(t, w, lease, work("after", 1)), billing.DenyLimitReached)
		// The capture reports the admitted event type.
		use := workReport(customer, "crossing-use", 5)
		use.RequestID = ptr("crossing")
		out := syncLease(t, w.alpha, billing.SyncLeasesParams{LeaseID: lease, Usage: numbered(2, use), Views: []billing.ViewParams{workView(customer)}})
		require.Empty(t, out.Usage.Refused)
		require.EqualValues(t, -3, lineOf(t, viewOf(t, out, customer), "quota").Remaining, "the crossing request settles in full")
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
				for round := range 5 {
					// Each gate settles, reports and asks for its customers in its
					// own order, while lease-owned admissions lock them.
					params := billing.SyncLeasesParams{LeaseID: lease}
					for i := range customers {
						c := customers[(g+i+round)%len(customers)]
						params.Views = append(params.Views, workView(c))
						params.InFlight = append(params.InFlight, billing.InFlight{CustomerID: c, Limit: "quota", Unit: "units", Amount: 1})
						params.Usage = append(params.Usage, workReport(c, fmt.Sprintf("use-%d-%d-%d", g, round, i), 1))
					}
					params.Usage = numbered(int64(round*len(customers))+1, params.Usage...)
					if _, err := w.alpha.SyncLeases(t.Context(), params); err != nil {
						errs <- err
						return
					}
					admission := usageAdmission(w, customers[(g+round)%len(customers)], fmt.Sprintf("race-%d-%d", g, round), "work", map[string]int64{"units": 1}, nil)
					admission.LeaseID, admission.ExpiresAt = &lease, nil
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
			require.EqualValues(t, 20, customerLimit(t, w.alpha, c, "quota").Used, "every report settled once")
		}
	})
	t.Run("OR-J24-S31/a-sync-that-cannot-take-its-locks-in-time-fails-fast-and-commits-nothing", func(t *testing.T) {
		w := newWorld(t)
		customer, _ := fundedCustomer(t, w)
		usageMeter(t, w.alpha, 100)
		lease := newLease()
		syncLease(t, w.alpha, billing.SyncLeasesParams{LeaseID: lease})
		lastSync := func() time.Time {
			var at time.Time
			require.NoError(t, w.pool.QueryRow(t.Context(), "SELECT last_sync_at FROM "+pgx.Identifier{w.schema, "leases"}.Sanitize()+" WHERE merchant_id = $1 AND id = $2",
				w.alpha.MerchantID().UUID(), lease.UUID()).Scan(&at))
			return at
		}
		before := lastSync()
		w.clock.Advance(time.Second)
		// Another transaction holds the customer's spend lock past the sync's
		// timeout: the sync gives up waiting well inside its work budget,
		// before the commit guard, and is answered a retryable 503.
		tx, err := w.pool.Begin(t.Context())
		require.NoError(t, err)
		var rolledBack sync.Once
		release := func() { rolledBack.Do(func() { _ = tx.Rollback(context.Background()) }) }
		defer release()
		_, err = tx.Exec(t.Context(), "SELECT id FROM "+pgx.Identifier{w.schema, "customers"}.Sanitize()+" WHERE merchant_id = $1 AND id = $2 FOR UPDATE",
			w.alpha.MerchantID().UUID(), customer.UUID())
		require.NoError(t, err)
		unlocked := make(chan struct{})
		started := time.Now()
		unlock := time.AfterFunc(limits.SyncTimeout+time.Second, func() {
			release()
			close(unlocked)
		})
		defer unlock.Stop()
		items := numbered(1, usageInput(customer, "slow", 1))
		_, err = w.alpha.SyncLeases(t.Context(), billing.SyncLeasesParams{LeaseID: lease, Usage: items})
		require.Less(t, time.Since(started), limits.SyncTimeout-time.Second, "the sync failed fast, before the commit guard")
		var failure *billing.StatusError
		require.ErrorAs(t, err, &failure, "the sync never commits")
		require.Equal(t, http.StatusServiceUnavailable, failure.Status, "a gate takes the answer as OpenRails unreachable and sends the usage again")
		require.Equal(t, billing.CodeServiceUnavailable, failure.Code)
		<-unlocked
		require.Equal(t, before, lastSync(), "the lease was not renewed")
		balance(t, w.alpha, customer, 1000, 0, 1000)
		out := settleUsage(t, w.alpha, lease, items...)
		require.EqualValues(t, 1, out.Usage.ThroughN, "the retry settles what the refused sync rolled back")
		balance(t, w.alpha, customer, 900, 100, 1000)
	})

	t.Run("OR-J24-S32/trust-levels-whose-spend-windows-share-a-key-but-not-a-cap-decide-apart", func(t *testing.T) {
		w := newWorld(t, sameWindowTrust)
		usageMeter(t, w.alpha, 100)
		customer := billing.CustomerID(uuid.New())
		fund(t, w.alpha, customer, "funded", 10_000)
		plain := computeView(customer)
		plain.Invoker = "apiKey:plain"
		gold := computeView(customer)
		gold.Invoker, gold.TrustLevel = "apiKey:gold", ptr("gold")
		out := syncLease(t, w.alpha, billing.SyncLeasesParams{LeaseID: newLease(), Views: []billing.ViewParams{plain, gold}})
		plainSet, goldSet := out.LimitSets[0], out.LimitSets[1]
		require.Equal(t, plainSet.Limits, goldSet.Limits, "both claim the same window")
		require.Len(t, plainSet.Capacities, 1)
		require.Len(t, goldSet.Capacities, 1)
		require.EqualValues(t, 1000, plainSet.Capacities[0].Capacity, "each under its own cap")
		require.EqualValues(t, 5000, goldSet.Capacities[0].Capacity)
		// Admitted on the server, each is refused by its own cap.
		plainWork := usageAdmission(w, customer, "plain-15", "compute", map[string]int64{"units": 15}, nil)
		plainWork.Invoker = "apiKey:plain"
		requireDenied(t, admitOne(t, w.alpha, plainWork), billing.DenyBudgetExceeded)
		goldWork := usageAdmission(w, customer, "gold-15", "compute", map[string]int64{"units": 15}, nil)
		goldWork.Invoker, goldWork.TrustLevel = "apiKey:gold", "gold"
		require.True(t, admitOne(t, w.alpha, goldWork).Allowed())
	})
	t.Run("OR-J24-S33/usage-without-a-grant-opens-no-window-and-a-lapsed-grants-windows-keep-counting", func(t *testing.T) {
		w := newWorld(t)
		applyLimits(t, w.alpha, lapsePlan)
		customer := billing.CustomerID(uuid.New())
		every := []string{"quota", "weekly", "recent", "visit", "ever"}
		// A free customer's heavy use opens no window of any kind: buying the
		// plan mid-week gives its whole capacity, and unpaid use starts no
		// session.
		settleUsage(t, w.alpha, newLease(), numbered(1, workReport(customer, "free", 500))...)
		grantLimitProduct(t, w.alpha, customer, "plan", 1)
		for _, key := range every {
			line := customerLimit(t, w.alpha, customer, key)
			require.Zero(t, line.Used, "%s counted usage from before any grant", key)
			require.Equal(t, line.Capacity, line.Remaining, key)
		}
		require.Nil(t, customerLimit(t, w.alpha, customer, "visit").WindowStartsAt, "unpaid use started no session")
		settleUsage(t, w.alpha, newLease(), numbered(1, workReport(customer, "granted", 10))...)
		for _, key := range every {
			require.EqualValues(t, 10, customerLimit(t, w.alpha, customer, key).Used, "granted usage opens and counts in %s", key)
		}
		w.clock.Advance(2 * time.Hour)
		page, err := w.alpha.ListCustomerLimits(t.Context(), billing.CustomerLimitListParams{CustomerID: customer})
		require.NoError(t, err)
		require.NotContains(t, limitKeys(page.Items), "quota", "a customer reads only the limits it is granted")
		require.NotContains(t, limitKeys(page.Items), "ever")
		// While the grant has lapsed nothing enforces the limits; usage counts
		// in the windows the grant opened while they last, and opens none.
		out := syncLease(t, w.alpha, billing.SyncLeasesParams{LeaseID: newLease(), Views: []billing.ViewParams{workView(customer)}})
		require.NotContains(t, out.LimitSets[0].Limits, "quota", "a lapsed grant's limits bind nothing")
		settleUsage(t, w.alpha, newLease(), numbered(1, workReport(customer, "lapsed", 30))...)
		grantLimitProduct(t, w.alpha, customer, "plan", 24)
		quota := customerLimit(t, w.alpha, customer, "quota")
		require.EqualValues(t, 100, quota.Capacity)
		require.EqualValues(t, 40, quota.Used, "the day's window kept counting while the grant lapsed")
		require.EqualValues(t, 40, customerLimit(t, w.alpha, customer, "ever").Used, "so did the lifetime row the first grant opened")
		visit := customerLimit(t, w.alpha, customer, "visit")
		require.Zero(t, visit.Used, "the session ended with its hour, and lapsed use started none")
		require.Nil(t, visit.WindowStartsAt)
	})

	t.Run("OR-J24-S34/a-backlog-across-a-thousand-customers-settles-a-bounded-prefix-per-sync", func(t *testing.T) {
		w := newWorld(t)
		usageMeter(t, w.alpha, 1)
		items := make([]billing.LeaseUsage, billing.MaxSyncUsageItems)
		for i := range items {
			items[i] = usageInput(billing.CustomerID(uuid.New()), fmt.Sprintf("backlog-%d", i), 1)
		}
		items = numbered(1, items...)
		lease := newLease()
		through, syncs := int64(0), 0
		for through < int64(len(items)) {
			syncs++
			require.LessOrEqual(t, syncs, len(items)/20, "each sync settles at least one chunk of 20 customers")
			out := syncLease(t, w.alpha, billing.SyncLeasesParams{LeaseID: lease, Usage: items[through:]})
			require.Empty(t, out.Usage.Refused, "items past the prefix are neither settled nor refused")
			require.Greater(t, out.Usage.ThroughN, through, "every sync settles a prefix")
			require.LessOrEqual(t, out.Usage.ThroughN-through, int64(100), "of at most 100 customers")
			through = out.Usage.ThroughN
		}
		require.GreaterOrEqual(t, syncs, len(items)/100, "the backlog drained over several syncs")
		// A resend of the whole backlog answers it as settled and charges
		// nothing again.
		out := syncLease(t, w.alpha, billing.SyncLeasesParams{LeaseID: lease, Usage: items})
		require.Empty(t, out.Usage.Refused)
		require.EqualValues(t, len(items), out.Usage.ThroughN)
		require.EqualValues(t, len(items), storeCount(t, w, "SELECT count() FROM {db}.usage_events FINAL WHERE merchant_id = {m}"), "every item settled exactly once")
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
		_, err := source.alpha.AdjustCustomerLimit(t.Context(), customer, billing.AdjustCustomerLimitParams{Limit: "ever", Amount: 500, SourceID: "ever-bonus"})
		require.NoError(t, err)
		_, err = source.alpha.AdjustCustomerLimit(t.Context(), customer, billing.AdjustCustomerLimitParams{Limit: "monthly", Amount: 50, SourceID: "monthly-bonus"})
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
		retried, err := target.AdjustCustomerLimit(t.Context(), customer, billing.AdjustCustomerLimitParams{Limit: "ever", Amount: 500, SourceID: "ever-bonus"})
		require.NoError(t, err)
		require.Equal(t, ever, *retried, "a staff adjustment's retry adds nothing at the destination")
	})
}
