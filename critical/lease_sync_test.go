//go:build integration

package critical_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
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

// quotaPlan is a lifetime quota of work units: what a gate reports in flight
// against it is held while its lease is live. The balance is never reported.
const quotaPlan = `schema_version: 1
meters:
  work: {event_type: work, value_property: units, aggregation: sum}
limits:
  quota:
    unit: units
    reset: {kind: lifetime}
    counts: [{meter: work}]
products:
  plan: {display_name: Plan, limits: {quota: "1000"}}
`

// quotaInFlight is amount of the customer's quota in flight.
func quotaInFlight(customer billing.CustomerID, amount int64) []billing.InFlight {
	return []billing.InFlight{{CustomerID: customer, Limit: "quota", Unit: "units", Amount: amount}}
}

// grantQuota declares quotaPlan and gives the customer its quota.
func grantQuota(t *testing.T, w *world, customer billing.CustomerID) {
	t.Helper()
	applyLimits(t, w.alpha, quotaPlan)
	grantLimitProduct(t, w.alpha, customer, "plan", 24)
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
		grantQuota(t, w, customer)
		outbox := numbered(1, usageInput(customer, "a", 1), usageInput(customer, "b", 2), usageInput(customer, "c", 3))
		out := syncLease(t, w.alpha, billing.SyncLeasesParams{LeaseID: lease, Usage: outbox[:2], Views: []billing.ViewParams{computeView(customer)},
			InFlight: quotaInFlight(customer, 100)})
		require.Empty(t, out.Usage.Refused)
		balance(t, w.alpha, customer, 700, 300, 1000)
		require.EqualValues(t, 100, customerLimit(t, w.alpha, customer, "quota").Held)
		// The process dies with item 3 unsent and a request in flight; once
		// the lease stops syncing its report counts nowhere.
		w.clock.Advance(limits.LeaseLifetime + time.Second)
		require.Zero(t, customerLimit(t, w.alpha, customer, "quota").Held)
		balance(t, w.alpha, customer, 700, 300, 1000)
		// Its successor resends the outbox against the dead lease with close:
		// the first two answer their receipts, the third settles as late
		// usage, and the close clears the report for good.
		out = syncLease(t, w.alpha, billing.SyncLeasesParams{LeaseID: lease, Usage: outbox, Close: true})
		require.EqualValues(t, 3, out.Usage.ThroughN)
		require.Empty(t, out.Usage.Refused)
		require.False(t, out.Lease.ExpiresAt.After(w.clock.Now()), "a closed lease has ended")
		balance(t, w.alpha, customer, 400, 600, 1000)
		require.Zero(t, customerLimit(t, w.alpha, customer, "quota").Held)
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
		nextMonth(w)
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
	})

	t.Run("OR-J24-S11/a-day-posts-once-and-a-top-up-posts-nothing", func(t *testing.T) {
		w := newWorld(t, earlyInMonth)
		customer, _ := fundedCustomer(t, w)
		usageMeter(t, w.alpha, 100)
		lease := newLease()
		settleUsage(t, w.alpha, lease, numbered(1, usageInput(customer, "a", 2))...)
		balance(t, w.alpha, customer, 800, 200, 1000)
		// Until it posts, the usage is pending: a view leaves what remains.
		out := syncLease(t, w.alpha, billing.SyncLeasesParams{LeaseID: lease, Views: []billing.ViewParams{computeView(customer)}})
		require.EqualValues(t, 800, lineOf(t, viewOf(t, out, customer), catalog.BalanceLimit).Remaining)

		// A top-up posts nothing: the usage stays pending.
		w.clock.Advance(time.Minute)
		fund(t, w.alpha, customer, "top-up", 500)
		balance(t, w.alpha, customer, 1300, 200, 1500)
		require.Empty(t, usageDeductions(t, w.alpha, customer))
		settleUsage(t, w.alpha, lease, numbered(2, usageInput(customer, "b", 1))...)
		balance(t, w.alpha, customer, 1200, 300, 1500)

		// The day's usage posts once, after the day ends.
		fleet := startFleet(t, w)
		postDays(t, fleet)
		require.Empty(t, usageDeductions(t, w.alpha, customer), "the day has not ended")
		now := w.clock.Now().UTC()
		day := time.Date(now.Year(), now.Month(), now.Day()+1, 0, 0, 0, 0, time.UTC)
		nextDay(w)
		postDays(t, fleet)
		balance(t, w.alpha, customer, 1200, 0, 1200)
		deductions := usageDeductions(t, w.alpha, customer)
		require.Len(t, deductions, 1)
		require.EqualValues(t, -300, deductions[0].Amount)
		require.True(t, deductions[0].Usage.Through.After(day), "a posting takes everything the event store holds")
		require.EqualValues(t, 300, deductions[0].Usage.Amount)
		require.Equal(t, []billing.UsageDeductionLine{{MeterKey: "compute", Requests: 2, Quantity: 3, Amount: 300}}, deductions[0].Usage.Lines)
		postDays(t, fleet)
		require.Len(t, usageDeductions(t, w.alpha, customer), 1, "a posted day posts nothing again")
	})

	t.Run("OR-J24-S71/usage-past-the-balance-is-pending-and-posts-as-owed", func(t *testing.T) {
		w := newWorld(t, earlyInMonth)
		customer, _ := fundedCustomer(t, w)
		usageMeter(t, w.alpha, 100)
		// Gates decide on views that lag settled usage by a few syncs: usage
		// can settle past zero. It is pending, and the balance shows it.
		settleUsage(t, w.alpha, newLease(), numbered(1, usageInput(customer, "past-zero", 15))...)
		balance(t, w.alpha, customer, -500, 1500, 1000)
		out := syncLease(t, w.alpha, billing.SyncLeasesParams{LeaseID: newLease(), Views: []billing.ViewParams{computeView(customer)}})
		require.EqualValues(t, -500, lineOf(t, viewOf(t, out, customer), catalog.BalanceLimit).Remaining)
		// The posting takes the balance and leaves the rest owed.
		fleet := startFleet(t, w)
		nextDay(w)
		postDays(t, fleet)
		balance(t, w.alpha, customer, 0, 0, 0)
		require.EqualValues(t, 500, owed(t, w.alpha, customer))
		deductions := usageDeductions(t, w.alpha, customer)
		require.NotEmpty(t, deductions)
		require.EqualValues(t, 1500, deductions[0].Usage.Amount, "one posting took it all")
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
	t.Run("OR-J24-S16/a-closed-lease-settles-and-clears-its-report", func(t *testing.T) {
		w := newWorld(t)
		customer, _ := fundedCustomer(t, w)
		usageMeter(t, w.alpha, 100)
		grantQuota(t, w, customer)
		lease := newLease()
		syncLease(t, w.alpha, billing.SyncLeasesParams{LeaseID: lease, InFlight: quotaInFlight(customer, 200)})
		require.EqualValues(t, 200, customerLimit(t, w.alpha, customer, "quota").Held)
		balance(t, w.alpha, customer, 1000, 0, 1000)

		// A sync with close settles its usage, then clears its report and
		// ends it, long before it would expire.
		out := syncLease(t, w.alpha, billing.SyncLeasesParams{LeaseID: lease, Usage: numbered(1, usageInput(customer, "last", 1)), Close: true})
		require.EqualValues(t, 1, out.Usage.ThroughN)
		require.Empty(t, out.Usage.Refused)
		require.False(t, out.Lease.ExpiresAt.After(w.clock.Now()), "a closed lease has ended")
		require.Zero(t, customerLimit(t, w.alpha, customer, "quota").Held)
		balance(t, w.alpha, customer, 900, 100, 1000)

		// Usage against a closed lease still settles, in full, and its report
		// stays clear.
		out = syncLease(t, w.alpha, billing.SyncLeasesParams{LeaseID: lease, Usage: numbered(2, usageInput(customer, "late", 1)),
			InFlight: quotaInFlight(customer, 100)})
		require.EqualValues(t, 2, out.Usage.ThroughN)
		require.Zero(t, customerLimit(t, w.alpha, customer, "quota").Held)
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

	t.Run("OR-J24-S42/a-view-request-is-refused-alone-and-the-sync-stands", func(t *testing.T) {
		// The gold tier's spend window is in yen and FX is down: its policy
		// cannot be resolved in dollars.
		w := newWorld(t, func(w *world) {
			w.deps.FXTransport = failingFX{}
			m := cfgMerchant(w.cfg, "alpha")
			m.BillingPolicies = []billing.BillingPolicy{{Name: "yen", Kind: "window_spend_cap", PolicyCurrency: "JPY",
				SpendWindows: []billing.BudgetWindow{{Key: "hourly", WindowSeconds: 3600, Limit: 1_000_000}}}}
			m.BillingPolicyBindings = []billing.BillingPolicyBinding{{PolicyName: "yen", Tier: "gold"}}
			setCfgMerchant(&w.cfg, "alpha", m)
		})
		usageMeter(t, w.alpha, 100)
		plain, _ := fundedCustomer(t, w)
		gold, _ := fundedCustomer(t, w)
		btc := computeView(plain)
		btc.Currency = "BTC"
		agent := computeView(plain)
		agent.InvokerType = "agent"
		goldView := computeView(gold)
		goldView.TrustLevel = ptr("gold")
		goldWork := usageInput(gold, "gold", 2)
		goldWork.TrustLevel = ptr("gold")
		lease := newLease()
		out := syncLease(t, w.alpha, billing.SyncLeasesParams{LeaseID: lease, Usage: numbered(1, usageInput(plain, "plain", 1), goldWork),
			Views: []billing.ViewParams{btc, agent, goldView, computeView(plain)}})
		// Usage settles, the gold customer's in no spend window, and the
		// plain customer's view is answered.
		require.EqualValues(t, 2, out.Usage.ThroughN)
		require.Empty(t, out.Usage.Refused)
		balance(t, w.alpha, plain, 900, 100, 1000)
		balance(t, w.alpha, gold, 800, 200, 1000)
		require.NotNil(t, out.LimitSets[3])
		viewOf(t, out, plain)
		// Each refused request is answered alone, as Admit would answer it.
		require.Len(t, out.RefusedViews, 3)
		for i, want := range []struct {
			status int
			code   billing.ErrorCode
			param  string
		}{{http.StatusBadRequest, billing.CodeCurrencyUnsupported, "views[0].currency"}, {http.StatusBadRequest, billing.CodeInvalidParam, "views[1].invoker_type"},
			{http.StatusInternalServerError, billing.CodeInternalError, ""}} {
			refused := out.RefusedViews[i]
			require.Equal(t, i, refused.Index)
			require.Nil(t, out.LimitSets[i])
			require.Equal(t, want.status, refused.Status)
			require.Equal(t, want.code, refused.Error.Code)
			if want.param != "" {
				require.Equal(t, want.param, *refused.Error.Param)
			}
		}
	})

	t.Run("OR-J24-S43/usage-counts-in-the-spend-window-of-its-admission-to-the-millisecond", func(t *testing.T) {
		w := newWorld(t, func(w *world) {
			m := cfgMerchant(w.cfg, "alpha")
			m.BillingPolicies = []billing.BillingPolicy{{Name: "hourly", Kind: "window_spend_cap",
				SpendWindows: []billing.BudgetWindow{{Key: "hourly", WindowSeconds: 3600, Limit: 1_000_000, Currency: "USD"}}}}
			m.BillingPolicyBindings = []billing.BillingPolicyBinding{{PolicyName: "hourly"}}
			setCfgMerchant(&w.cfg, "alpha", m)
		})
		usageMeter(t, w.alpha, 100)
		// A window whose end falls inside a second: its offset is the
		// customer's.
		var customer billing.CustomerID
		var window string
		var end time.Time
		for {
			customer, _ = fundedCustomer(t, w)
			out := syncLease(t, w.alpha, billing.SyncLeasesParams{LeaseID: newLease(), Views: []billing.ViewParams{computeView(customer)}})
			window = out.LimitSets[0].Limits[1]
			end = *lineOf(t, viewOf(t, out, customer), window).WindowEndsAt
			if end.Nanosecond() != 0 {
				break
			}
		}
		w.clock.Advance(end.Sub(w.clock.Now()) + 2*time.Second)
		before, after := usageInput(customer, "before", 1), usageInput(customer, "after", 2)
		before.AdmittedAt, after.AdmittedAt = ptr(end.Add(-time.Millisecond)), ptr(end)
		out := syncLease(t, w.alpha, billing.SyncLeasesParams{LeaseID: newLease(), Usage: numbered(1, before, after), Views: []billing.ViewParams{computeView(customer)}})
		require.Empty(t, out.Usage.Refused)
		line := lineOf(t, viewOf(t, out, customer), window)
		require.True(t, line.WindowStartsAt.Equal(end), "the view is of the window that starts at the end")
		require.EqualValues(t, 200, line.Used, "only the item admitted at the end counts in the next window")
	})

	t.Run("OR-J24-S45/a-report-naming-a-customer-another-transaction-spends-does-not-wait", func(t *testing.T) {
		w := newWorld(t)
		customer, _ := fundedCustomer(t, w)
		usageMeter(t, w.alpha, 100)
		lease := newLease()
		syncLease(t, w.alpha, billing.SyncLeasesParams{LeaseID: lease})
		// A host transaction holds the customer's spend lock.
		tx, err := w.pool.Begin(t.Context())
		require.NoError(t, err)
		defer tx.Rollback(context.Background()) //nolint:errcheck
		_, err = w.local.StartRunningChargeTx(t.Context(), tx, billing.StartRunningChargeParams{Reference: "host", CustomerID: customer, Currency: "USD", RatePerHour: 3600})
		require.NoError(t, err)
		// A sync reporting the customer in flight for the first time takes no
		// lock of the customer and so commits at once.
		started := time.Now()
		_, err = w.alpha.SyncLeases(t.Context(), billing.SyncLeasesParams{LeaseID: lease, InFlight: quotaInFlight(customer, 200)})
		require.NoError(t, err)
		require.Less(t, time.Since(started), time.Second)
		require.NoError(t, tx.Commit(t.Context()))
		balance(t, w.alpha, customer, 1000, 0, 1000)
	})
}

// failingFX is an FX wire that is down.
type failingFX struct{}

func (failingFX) RoundTrip(r *http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: http.StatusServiceUnavailable, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
}
