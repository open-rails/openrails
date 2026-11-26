//go:build integration

package critical_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/modules/limits"
	"github.com/stretchr/testify/require"
)

// tokenPlan is a Claude-style plan in weighted tokens: a five-hour session
// and a weekly cap, both admitting the request that crosses them. Usage is
// money too, at a host-stated (dynamic) price.
const tokenPlan = `schema_version: 1
meters:
  input-tokens: {event_type: chat, value_property: input_tokens, aggregation: sum, group_by: {model: model}}
limits:
  five_hour:
    unit: usage_units
    reset: {kind: session, window_seconds: 18000}
    overrun: admit_while_positive
    counts:
    - {meter: input-tokens, per: 1000, filter: {model: [small]}}
    - {meter: input-tokens, weight: 5, per: 1000, filter: {model: [large]}}
  weekly:
    unit: usage_units
    reset: {kind: fixed, window_seconds: 604800, anchor: first_use}
    overrun: admit_while_positive
    counts:
    - {meter: input-tokens, per: 1000, filter: {model: [small]}}
    - {meter: input-tokens, weight: 5, per: 1000, filter: {model: [large]}}
products:
  pro: {display_name: Pro, limits: {five_hour: "1000", weekly: "1500"}}
  chat: {display_name: Chat, rate_cards: [{meter: input-tokens, price: {model: dynamic, currency: USD}}]}
`

// unitPlan counts work units toward a daily quota that admits only whole
// estimates.
const unitPlan = `schema_version: 1
meters:
  work: {event_type: work, value_property: units, aggregation: sum}
limits:
  quota:
    unit: units
    reset: {kind: fixed, window_seconds: 86400, anchor: first_use}
    counts: [{meter: work}]
products:
  plan: {display_name: Plan, limits: {quota: "100"}}
  metered: {display_name: Metered, rate_cards: [{meter: work, price: {model: dynamic, currency: USD}}]}
`

// weightedPlan counts input and output tokens toward one cap: output weighs
// four times input, and a large model's output six more.
const weightedPlan = `schema_version: 1
meters:
  input-tokens: {event_type: chat, value_property: input_tokens, aggregation: sum, group_by: {model: model}}
  output-tokens: {event_type: chat, value_property: output_tokens, aggregation: sum, group_by: {model: model}}
limits:
  tokens:
    unit: usage_units
    reset: {kind: lifetime}
    counts:
    - {meter: input-tokens, per: 1000}
    - {meter: output-tokens, weight: 4, per: 1000}
    - {meter: output-tokens, weight: 6, per: 1000, filter: {model: [large]}}
products:
  pro: {display_name: Pro, limits: {tokens: "100"}}
  chat: {display_name: Chat, rate_cards: [{meter: input-tokens, price: {model: dynamic, currency: USD}}]}
`

func usageAdmission(customer billing.CustomerID, id, eventType string, dims map[string]int64, meta map[string]any) billing.AdmitParams {
	return billing.AdmitParams{RequestID: id, CustomerID: customer, Invoker: customer.String(), InvokerType: billing.InvokerTypeCustomer, Currency: "USD",
		Usage: &billing.UsageEstimate{EventType: eventType, Dimensions: dims, Metadata: meta}}
}

func chatAdmission(customer billing.CustomerID, id, model string, tokens int64) billing.AdmitParams {
	return usageAdmission(customer, id, "chat", map[string]int64{"input_tokens": tokens}, map[string]any{"model": model})
}

func chatUsage(model string, tokens int64) billing.CaptureUsage {
	return billing.CaptureUsage{Dimensions: map[string]int64{"input_tokens": tokens}, Metadata: map[string]any{"model": model}}
}

// reservation is a host-stated amount of compute (meterForCapture).
func reservation(customer billing.CustomerID, id string, amount int64) billing.AdmitParams {
	return billing.AdmitParams{RequestID: id, CustomerID: customer, Invoker: customer.String(), InvokerType: billing.InvokerTypeCustomer, Currency: "USD", EstimatedAmount: amount}
}

// computeUsage is a reservation's usage: it charges the stated amount.
var computeUsage = billing.CaptureUsage{EventType: "compute", Dimensions: map[string]int64{"units": 1}}

// decided is one request a gate of its own decided on a view it read for
// it. An admitted request's limit windows are reported in flight before
// admitOne returns; the balance never is.
type decided struct {
	g *openrails.Gate
	*openrails.Ticket
}

func admitOne(t *testing.T, client *openrails.Client, p billing.AdmitParams) decided {
	t.Helper()
	g := newGate(t, client, openrails.GateDeps{})
	ticket, err := g.Admit(t.Context(), p)
	require.NoError(t, err)
	if ticket.Allowed() {
		inFlightReported()
	}
	return decided{g, ticket}
}

// capture captures the request's usage and returns once it settled.
func (d decided) capture(t *testing.T, u billing.CaptureUsage) {
	t.Helper()
	require.NoError(t, d.Capture(t.Context(), u))
	gateFlush(t, d.g)
}

// release gives the request's take back and returns once it is no longer
// reported in flight.
func (d decided) release(t *testing.T) {
	t.Helper()
	require.NoError(t, d.Release(t.Context()))
	inFlightReported()
}

// inFlightReported waits for an active gate's report to catch up: it reports a
// ticket once a sync has passed it, and it syncs every interval.
func inFlightReported() { time.Sleep(2*limits.SyncInterval + 500*time.Millisecond) }

func requireLimitReached(t *testing.T, d decided, limit string) *billing.Refusal {
	t.Helper()
	require.False(t, d.Allowed())
	refusal := d.Refusal()
	require.NotNil(t, refusal)
	require.Equal(t, billing.DenyLimitReached, refusal.DenyCode)
	require.NotNil(t, refusal.Limit)
	require.Equal(t, limit, *refusal.Limit)
	return refusal
}

func requireDenied(t *testing.T, d decided, code billing.DenyCode) *billing.Refusal {
	t.Helper()
	require.False(t, d.Allowed())
	refusal := d.Refusal()
	require.NotNil(t, refusal)
	require.Equal(t, code, refusal.DenyCode)
	return refusal
}

func limitsReached(t *testing.T, client *openrails.Client, from, to time.Time) int64 {
	t.Helper()
	result, err := client.QueryMetrics(t.Context(), billing.QueryMetricsParams{Measures: []string{"limits_reached"},
		Range: &billing.MetricsRange{From: from.Format(time.RFC3339), To: to.Format(time.RFC3339)}})
	require.NoError(t, err)
	var total int64
	for _, row := range result.Rows {
		var n int64
		_, err := fmt.Sscan(fmt.Sprint(row[len(row)-1]), &n)
		require.NoError(t, err)
		total += n
	}
	return total
}

func spendCap(windows ...billing.BudgetWindow) func(*world) {
	return func(w *world) {
		m := cfgMerchant(w.cfg, "alpha")
		m.BillingPolicies = []billing.BillingPolicy{{Name: "capped", SpendWindows: windows}}
		m.BillingPolicyBindings = []billing.BillingPolicyBinding{{PolicyName: "capped"}}
		setCfgMerchant(&w.cfg, "alpha", m)
	}
}

func fund(t *testing.T, client *openrails.Client, customer billing.CustomerID, id string, amount int64) {
	t.Helper()
	_, err := client.CreateCreditGrants(t.Context(), []billing.CreateCreditGrantParams{{CustomerID: customer, Currency: "USD", Amount: amount, Source: "critical", SourceID: id}})
	require.NoError(t, err)
}

func TestLimitDecisions(t *testing.T) {
	t.Run("OR-J23-S13/a-session-cap-lets-the-crossing-request-finish-and-refuses-the-next-until-it-resets", func(t *testing.T) {
		w := newWorld(t)
		applyLimits(t, w.alpha, tokenPlan)
		customer := billing.CustomerID(uuid.New())
		grantLimitProduct(t, w.alpha, customer, "pro", 24*30)
		require.Nil(t, customerLimit(t, w.alpha, customer, "five_hour").WindowStartsAt, "no use, no session")

		first := admitOne(t, w.alpha, chatAdmission(customer, "first", "large", 100_000))
		require.True(t, first.Allowed(), "100k large tokens weigh 500 units")
		first.capture(t, chatUsage("large", 100_000))
		five := customerLimit(t, w.alpha, customer, "five_hour")
		require.NotNil(t, five.WindowStartsAt, "first use opens the session")
		require.True(t, five.WindowStartsAt.Add(5*time.Hour).Equal(*five.ResetsAt))
		require.EqualValues(t, 500, five.Used)
		require.EqualValues(t, 500, five.Remaining)
		weekly := customerLimit(t, w.alpha, customer, "weekly")
		require.NotNil(t, weekly.WindowStartsAt, "the weekly window is anchored at the first use")
		require.EqualValues(t, 1000, weekly.Remaining)

		second := admitOne(t, w.alpha, chatAdmission(customer, "second", "small", 400_000))
		require.True(t, second.Allowed())
		second.capture(t, chatUsage("small", 400_000))
		require.EqualValues(t, 100, customerLimit(t, w.alpha, customer, "five_hour").Remaining)

		// The session has 100 left: the crossing request is admitted while
		// any capacity remains and settles in full.
		crossing := admitOne(t, w.alpha, chatAdmission(customer, "crossing", "large", 100_000))
		require.True(t, crossing.Allowed(), "admit_while_positive admits while any capacity remains")
		crossing.capture(t, chatUsage("large", 100_000))
		five = customerLimit(t, w.alpha, customer, "five_hour")
		require.EqualValues(t, 1400, five.Used, "never clamped")
		require.EqualValues(t, -400, five.Remaining)
		require.EqualValues(t, 100, customerLimit(t, w.alpha, customer, "weekly").Remaining)

		next := admitOne(t, w.alpha, chatAdmission(customer, "next", "small", 10_000))
		refusal := requireLimitReached(t, next, "five_hour")
		require.NotNil(t, refusal.ResetsAt)
		require.True(t, five.ResetsAt.Equal(*refusal.ResetsAt), "the session resets five hours after its first use")
		require.NotNil(t, refusal.RetryAfterSeconds)
		require.Positive(t, *refusal.RetryAfterSeconds)
		require.EqualValues(t, 1400, customerLimit(t, w.alpha, customer, "five_hour").Used, "a refusal takes nothing")
	})

	t.Run("OR-J23-S35/a-limit-counting-two-meters-by-weight-admits-and-refuses-at-their-weighted-total", func(t *testing.T) {
		w := newAuthorityWorld(t).world
		applyLimits(t, w.alpha, weightedPlan)
		declared, err := w.alpha.GetLimit(t.Context(), "tokens")
		require.NoError(t, err)
		require.Equal(t, []catalog.LimitCount{
			{Meter: "input-tokens", Weight: 1, Per: 1000},
			{Meter: "output-tokens", Weight: 4, Per: 1000},
			{Meter: "output-tokens", Weight: 6, Per: 1000, Filter: map[string][]string{"model": {"large"}}},
		}, declared.Counts, "weight and per default to 1")

		customer := billing.CustomerID(uuid.New())
		grantLimitProduct(t, w.alpha, customer, "pro", 48)
		tokens := func(id, model string, in, out int64) billing.AdmitParams {
			return usageAdmission(customer, id, "chat", map[string]int64{"input_tokens": in, "output_tokens": out}, map[string]any{"model": model})
		}
		used := func(model string, in, out int64) billing.CaptureUsage {
			return billing.CaptureUsage{Dimensions: map[string]int64{"input_tokens": in, "output_tokens": out}, Metadata: map[string]any{"model": model}}
		}
		small := admitOne(t, w.alpha, tokens("small", "small", 20_000, 5_000))
		require.True(t, small.Allowed(), "20 + 20 units")
		require.EqualValues(t, 40, customerLimit(t, w.alpha, customer, "tokens").Held, "what a gate admits it reports in flight")
		large := admitOne(t, w.alpha, tokens("large", "large", 1_000, 5_000))
		require.True(t, large.Allowed(), "1 + 20 + 30 units")
		require.EqualValues(t, 9, customerLimit(t, w.alpha, customer, "tokens").Remaining)
		requireLimitReached(t, admitOne(t, w.alpha, tokens("ten-in", "small", 10_000, 0)), "tokens")
		requireLimitReached(t, admitOne(t, w.alpha, tokens("ten-out", "small", 0, 2_500)), "tokens")
		require.True(t, admitOne(t, w.alpha, tokens("nine", "small", 9_000, 0)).Allowed(), "an estimate that fits exactly")
		require.Zero(t, customerLimit(t, w.alpha, customer, "tokens").Remaining)

		// Each count rounds half up: 20.5 + 20.5 settle as 42, 1.499 + 20 + 30
		// as 51, past what was estimated.
		small.capture(t, used("small", 20_500, 5_125))
		large.capture(t, used("large", 1_499, 5_000))
		line := customerLimit(t, w.alpha, customer, "tokens")
		require.EqualValues(t, 93, line.Used)
		require.EqualValues(t, 9, line.Held)
		require.EqualValues(t, -2, line.Remaining)

		// One gate costs its requests by the same counts.
		other := billing.CustomerID(uuid.New())
		grantLimitProduct(t, w.alpha, other, "pro", 48)
		g := newGate(t, w.alpha, openrails.GateDeps{})
		gateTokens := func(id, model string, in, out int64) billing.AdmitParams {
			p := usageAdmission(other, id, "chat", map[string]int64{"input_tokens": in, "output_tokens": out}, map[string]any{"model": model})
			p.Usage.Amount = ptr(int64(0))
			return p
		}
		eighty := gateAdmit(t, g, gateTokens("eighty", "small", 40_000, 10_000))
		require.NoError(t, eighty.Capture(t.Context(), used("small", 10_000, 5_000)), "30 units of the 80 estimated")
		gateFlush(t, g)
		sixty := gateAdmit(t, g, gateTokens("sixty", "large", 10_000, 5_000))
		refused, err := g.Admit(t.Context(), gateTokens("past", "large", 1_000, 2_500))
		require.NoError(t, err)
		require.False(t, refused.Allowed(), "26 units do not fit the 10 left")
		require.Equal(t, billing.DenyLimitReached, refused.Refusal().DenyCode)
		require.Equal(t, "tokens", *refused.Refusal().Limit)
		require.NoError(t, sixty.Capture(t.Context(), used("large", 10_000, 5_000)))
		gateFlush(t, g)
		require.EqualValues(t, 90, customerLimit(t, w.alpha, other, "tokens").Used)
	})

	t.Run("OR-J23-S14/a-request-past-the-balance-or-a-monthly-spend-cap-takes-from-neither", func(t *testing.T) {
		w := newWorld(t, spendCap(billing.BudgetWindow{Key: "monthly", WindowSeconds: 30 * 24 * 3600, Limit: 5000, Currency: "USD"}))
		meterForCapture(t, w)
		customer := billing.CustomerID(uuid.New())
		fund(t, w.alpha, customer, "funded", 8000)

		// An open ticket holds nothing of the balance on the server: its
		// usage counts once it settles. The spend window counts it in flight.
		a := admitOne(t, w.alpha, reservation(customer, "a", 4000))
		require.True(t, a.Allowed())
		balance(t, w.alpha, customer, 8000, 0, 8000)
		line := customerLimit(t, w.alpha, customer, "balance")
		require.Equal(t, "USD", line.Unit)
		require.Equal(t, billing.CustomerLimit{CustomerID: customer, Limit: "balance", Unit: "USD", Capacity: 8000, Held: 0, Remaining: 8000}, line,
			"balance is a limit: the prepaid balance, its pending usage and what remains")

		refusal := requireDenied(t, admitOne(t, w.alpha, reservation(customer, "b", 2000)), billing.DenyBudgetExceeded)
		require.NotNil(t, refusal.RetryAfterSeconds)
		require.Positive(t, *refusal.RetryAfterSeconds)
		balance(t, w.alpha, customer, 8000, 0, 8000)

		c := admitOne(t, w.alpha, reservation(customer, "c", 1000))
		require.True(t, c.Allowed(), "the window has exactly 1000 left")
		c.release(t)
		balance(t, w.alpha, customer, 8000, 0, 8000)

		// The capture is pending against the balance and settles in the window.
		a.capture(t, computeUsage)
		balance(t, w.alpha, customer, 4000, 4000, 8000)
		requireDenied(t, admitOne(t, w.alpha, reservation(customer, "d", 1500)), billing.DenyBudgetExceeded)
		require.True(t, admitOne(t, w.alpha, reservation(customer, "e", 1000)).Allowed(), "4000 settled leaves 1000 in the window")

		// The balance refuses while the window has room: the window takes nothing.
		thin := billing.CustomerID(uuid.New())
		fund(t, w.alpha, thin, "thin", 500)
		requireDenied(t, admitOne(t, w.alpha, reservation(thin, "x", 800)), billing.DenyInsufficientBalance)
		balance(t, w.alpha, thin, 500, 0, 500)
		require.True(t, admitOne(t, w.alpha, reservation(thin, "y", 500)).Allowed())
		fund(t, w.alpha, thin, "more", 4500)
		require.True(t, admitOne(t, w.alpha, reservation(thin, "z", 4500)).Allowed(), "the refused 800 took nothing from the window")
		fund(t, w.alpha, thin, "spare", 1000)
		requireDenied(t, admitOne(t, w.alpha, reservation(thin, "q", 1)), billing.DenyBudgetExceeded)
		balance(t, w.alpha, thin, 6000, 0, 6000)
	})

	t.Run("OR-J23-S16/an-overrun-settles-past-capacity-and-records-reached-once", func(t *testing.T) {
		w := newWorld(t)
		applyLimits(t, w.alpha, unitPlan)
		customer := billing.CustomerID(uuid.New())
		grantLimitProduct(t, w.alpha, customer, "plan", 24)
		began := w.clock.Now()
		work := func(id string, n int64) billing.AdmitParams {
			p := usageAdmission(customer, id, "work", map[string]int64{"units": n}, nil)
			p.Usage.Amount = ptr(int64(0))
			return p
		}
		units := func(n int64) billing.CaptureUsage {
			return billing.CaptureUsage{Dimensions: map[string]int64{"units": n}}
		}

		estimate := admitOne(t, w.alpha, work("estimate", 60))
		require.True(t, estimate.Allowed())
		require.Zero(t, limitsReached(t, w.alpha, began.Add(-time.Hour), began.Add(time.Hour)), "60 of 100 in flight is short of capacity")
		estimate.capture(t, units(130))
		quota := customerLimit(t, w.alpha, customer, "quota")
		require.EqualValues(t, 130, quota.Used, "usage above its estimate settles in full")
		require.EqualValues(t, -30, quota.Remaining)
		require.EqualValues(t, 1, limitsReached(t, w.alpha, began.Add(-time.Hour), began.Add(time.Hour)))
		requireLimitReached(t, admitOne(t, w.alpha, work("refused", 1)), "quota")

		// A refill makes room again; a second overrun records nothing new.
		_, err := w.alpha.AdjustCustomerLimit(t.Context(), customer, billing.AdjustCustomerLimitParams{Limit: "quota", Amount: 50, SourceID: "refill"})
		require.NoError(t, err)
		refilled := admitOne(t, w.alpha, work("after-refill", 10))
		require.True(t, refilled.Allowed())
		refilled.capture(t, units(40))
		quota = customerLimit(t, w.alpha, customer, "quota")
		require.EqualValues(t, 170, quota.Used)
		require.EqualValues(t, 150, quota.Capacity)
		requireLimitReached(t, admitOne(t, w.alpha, work("refused-again", 1)), "quota")
		require.EqualValues(t, 1, limitsReached(t, w.alpha, began.Add(-time.Hour), began.Add(time.Hour)), "a window records reaching capacity once")
	})

	t.Run("OR-J23-S17/a-delegated-invoker-is-refused-and-takes-nothing", func(t *testing.T) {
		w := newWorld(t)
		applyLimits(t, w.alpha, unitPlan)
		customer := billing.CustomerID(uuid.New())
		grantLimitProduct(t, w.alpha, customer, "plan", 24)
		fund(t, w.alpha, customer, "funded", 1000)
		work := func(id string) billing.AdmitParams {
			p := usageAdmission(customer, id, "work", map[string]int64{"units": 5}, nil)
			p.Usage.Amount = ptr(int64(0))
			return p
		}
		delegated := func(p billing.AdmitParams) billing.AdmitParams {
			p.Invoker, p.InvokerType = "agent:"+uuid.NewString(), billing.InvokerTypeDelegated
			return p
		}
		for _, p := range []billing.AdmitParams{delegated(reservation(customer, "money", 100)), delegated(work("units"))} {
			requireDenied(t, admitOne(t, w.alpha, p), billing.DenyDelegatedSpendNotAllowed)
		}
		balance(t, w.alpha, customer, 1000, 0, 1000)
		quota := customerLimit(t, w.alpha, customer, "quota")
		require.Nil(t, quota.WindowStartsAt, "a refused request opens no window")
		require.Zero(t, quota.Held)
		own := admitOne(t, w.alpha, work("own"))
		require.True(t, own.Allowed(), "the customer's own credential is admitted")
	})

	t.Run("OR-J23-S18/invokers-with-one-limit-set-share-windows-another-trust-level-has-its-own", func(t *testing.T) {
		w := newWorld(t, func(w *world) {
			m := cfgMerchant(w.cfg, "alpha")
			m.BillingPolicies = []billing.BillingPolicy{
				{Name: "free", SpendWindows: []billing.BudgetWindow{{Key: "free-month", WindowSeconds: 30 * 24 * 3600, Limit: 1000, Currency: "USD"}}},
				{Name: "gold", SpendWindows: []billing.BudgetWindow{{Key: "gold-month", WindowSeconds: 30 * 24 * 3600, Limit: 5000, Currency: "USD"}}},
			}
			m.BillingPolicyBindings = []billing.BillingPolicyBinding{{PolicyName: "free"}, {PolicyName: "gold", Tier: "gold"}}
			setCfgMerchant(&w.cfg, "alpha", m)
		})
		meterForCapture(t, w)
		customer := billing.CustomerID(uuid.New())
		fund(t, w.alpha, customer, "funded", 10_000)
		key := func(p billing.AdmitParams, invoker, trust string) billing.AdmitParams {
			p.Invoker, p.TrustLevel = invoker, trust
			return p
		}
		require.True(t, admitOne(t, w.alpha, key(reservation(customer, "key-one", 600), "apiKey:one", "")).Allowed())
		requireDenied(t, admitOne(t, w.alpha, key(reservation(customer, "key-two", 600), "apiKey:two", "")), billing.DenyBudgetExceeded)
		require.True(t, admitOne(t, w.alpha, key(reservation(customer, "key-two-small", 400), "apiKey:two", "")).Allowed(),
			"two keys with the same limits share one window")
		require.True(t, admitOne(t, w.alpha, key(reservation(customer, "gold-key", 600), "apiKey:three", "gold")).Allowed(),
			"another trust level is decided against its own window")
		// Open tickets hold their windows, never the balance on the server.
		balance(t, w.alpha, customer, 10_000, 0, 10_000)
	})

	t.Run("OR-J23-S33/a-spend-window-that-names-no-currency-is-in-its-policys-currency", func(t *testing.T) {
		// ¥100 an hour; at the fixture's rate of 1, $100 of USD requests.
		w := newWorld(t, func(w *world) {
			m := cfgMerchant(w.cfg, "alpha")
			m.BillingPolicies = []billing.BillingPolicy{{Name: "yen", PolicyCurrency: "JPY",
				SpendWindows: []billing.BudgetWindow{{Key: "hourly", WindowSeconds: 3600, Limit: 1_000_000}}}}
			m.BillingPolicyBindings = []billing.BillingPolicyBinding{{PolicyName: "yen"}}
			setCfgMerchant(&w.cfg, "alpha", m)
		})
		customer := billing.CustomerID(uuid.New())
		fund(t, w.alpha, customer, "funded", 300_000_000)
		require.True(t, admitOne(t, w.alpha, reservation(customer, "within", 60_000_000)).Allowed(), "$60 is within ¥100, not past 1,000,000 USD units")
		requireDenied(t, admitOne(t, w.alpha, reservation(customer, "past", 50_000_000)), billing.DenyBudgetExceeded)
		balance(t, w.alpha, customer, 300_000_000, 0, 300_000_000)
	})

	t.Run("OR-J23-S42/one-policy-caps-what-is-owed-and-spend-per-window-and-a-customers-own-limit-overrides-its-cap", func(t *testing.T) {
		// The terms give $30 of credit; the policy caps what is owed at $20
		// and spend at $15 an hour. A gate refuses on the set's balance code
		// once the line is gone, and with budget_exceeded once the window is:
		// both are read from its view.
		a, _, customer := usageBillingWorld(t, false, func(w *world) {
			m := cfgMerchant(w.cfg, "alpha")
			m.BillingPolicies = []billing.BillingPolicy{{Name: "team", OutstandingCapAmount: 20_000_000,
				SpendWindows: []billing.BudgetWindow{{Key: "hourly", WindowSeconds: 3600, Limit: 15_000_000, Currency: "USD"}}}}
			m.BillingPolicyBindings = []billing.BillingPolicyBinding{{PolicyName: "team"}}
			setCfgMerchant(&w.cfg, "alpha", m)
		})
		view := func() (*billing.LimitSet, billing.ViewLimit) {
			t.Helper()
			out := syncLease(t, a.alpha, billing.SyncLeasesParams{LeaseID: newLease(), Views: []billing.ViewParams{computeView(customer)}})
			set := out.LimitSets[0]
			require.Nil(t, set.Refusal)
			for _, key := range set.Limits {
				if strings.HasPrefix(key, limits.SpendKeyPrefix) {
					return set, lineOf(t, viewOf(t, out, customer), key)
				}
			}
			require.FailNow(t, "the policy's spend window is in the set", "%v", set.Limits)
			return nil, billing.ViewLimit{}
		}
		set, window := view()
		require.EqualValues(t, 20_000_000, set.CreditLine, "the policy's cap, not the terms' line")
		require.Equal(t, billing.DenyInsufficientCredit, set.BalanceDenyCode)
		require.EqualValues(t, 15_000_000, window.Remaining)

		// $20 owed takes the whole line.
		_, err := a.alpha.StartRunningCharge(t.Context(), billing.StartRunningChargeParams{Reference: "owed", CustomerID: customer, Currency: "USD", RatePerHour: 20_000_000})
		require.NoError(t, err)
		a.clock.Advance(time.Hour)
		_, err = a.alpha.StopRunningCharge(t.Context(), "owed", billing.StopRunningChargeParams{})
		require.NoError(t, err)
		set, _ = view()
		require.Zero(t, set.CreditLine)
		require.Equal(t, billing.DenyOutstandingCapReached, set.BalanceDenyCode)

		// The customer's own $40 overrides the cap.
		_, err = a.alpha.UpdateCustomer(t.Context(), customer, billing.UpdateCustomerParams{CreditLimits: []billing.CreditLimit{{Currency: "USD", Amount: 40_000_000}}})
		require.NoError(t, err)
		set, window = view()
		require.EqualValues(t, 20_000_000, set.CreditLine, "$40 less the $20 owed")
		require.Equal(t, billing.DenyInsufficientCredit, set.BalanceDenyCode)

		// Another gate's $20 of open requests fills the window while the line
		// has room.
		syncLease(t, a.alpha, billing.SyncLeasesParams{LeaseID: newLease(),
			InFlight: []billing.InFlight{{CustomerID: customer, Limit: window.Limit, Unit: window.Unit, Amount: 20_000_000}}})
		set, window = view()
		require.EqualValues(t, 20_000_000, window.Held)
		require.LessOrEqual(t, window.Remaining, int64(0), "the window refuses")
		require.EqualValues(t, 20_000_000, set.CreditLine)
	})
}
