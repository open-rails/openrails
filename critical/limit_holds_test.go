//go:build integration

package critical_test

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
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

func usageAdmission(w *world, customer billing.CustomerID, id, eventType string, dims map[string]int64, meta map[string]any) billing.AdmitParams {
	return billing.AdmitParams{RequestID: id, CustomerID: customer, Invoker: customer.String(), InvokerType: billing.InvokerTypeCustomer, Currency: "USD",
		Usage: &billing.UsageEstimate{EventType: eventType, Dimensions: dims, Metadata: meta}, ExpiresAt: ptr(w.clock.Now().Add(time.Hour))}
}

func chatAdmission(w *world, customer billing.CustomerID, id, model string, tokens int64) billing.AdmitParams {
	return usageAdmission(w, customer, id, "chat", map[string]int64{"input_tokens": tokens}, map[string]any{"model": model})
}

func admitOne(t *testing.T, client *openrails.Client, p billing.AdmitParams) billing.AdmissionVerdict {
	t.Helper()
	verdicts, err := client.Admit(t.Context(), []billing.AdmitParams{p})
	require.NoError(t, err)
	require.Len(t, verdicts, 1)
	require.Nil(t, verdicts[0].Error, "%+v", verdicts[0].Error)
	return verdicts[0]
}

func captureUsage(t *testing.T, client *openrails.Client, id, eventType string, dims map[string]int64, meta map[string]any) *billing.CaptureReceipt {
	t.Helper()
	results, err := client.CaptureAdmissions(t.Context(), []billing.CaptureAdmissionParams{{RequestID: id, Usage: billing.CaptureUsage{EventType: eventType, Dimensions: dims, Metadata: meta}}})
	require.NoError(t, err)
	require.Len(t, results, 1)
	require.NoError(t, results[0].Err())
	return results[0].Receipt
}

func captureChat(t *testing.T, client *openrails.Client, id, model string, tokens int64) {
	t.Helper()
	captureUsage(t, client, id, "chat", map[string]int64{"input_tokens": tokens}, map[string]any{"model": model})
}

func requireLimitReached(t *testing.T, v billing.AdmissionVerdict, limit string) *billing.Refusal {
	t.Helper()
	require.False(t, v.Allowed())
	require.Equal(t, 429, v.Status)
	require.NotNil(t, v.Admission.BlockedBy)
	require.Equal(t, billing.AdmissionBlockedByLimit, *v.Admission.BlockedBy)
	refusal := v.Admission.Refusal()
	require.NotNil(t, refusal)
	require.Equal(t, billing.DenyLimitReached, refusal.DenyCode)
	require.NotNil(t, refusal.Limit)
	require.Equal(t, limit, *refusal.Limit)
	return refusal
}

func requireDenied(t *testing.T, v billing.AdmissionVerdict, code billing.DenyCode) {
	t.Helper()
	require.False(t, v.Allowed())
	require.NotNil(t, v.Admission.DenyCode)
	require.Equal(t, code, *v.Admission.DenyCode)
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
		m.BillingPolicies = []billing.BillingPolicy{{Name: "capped", Kind: "window_spend_cap", SpendWindows: windows}}
		m.BillingPolicyBindings = []billing.BillingPolicyBinding{{PolicyName: "capped"}}
		setCfgMerchant(&w.cfg, "alpha", m)
	}
}

func fund(t *testing.T, client *openrails.Client, customer billing.CustomerID, id string, amount int64) {
	t.Helper()
	_, err := client.CreateCreditGrants(t.Context(), []billing.CreateCreditGrantParams{{CustomerID: customer, Currency: "USD", Amount: amount, Source: "critical", SourceID: id}})
	require.NoError(t, err)
}

func TestLimitHolds(t *testing.T) {
	t.Run("OR-J23-S13/a-session-and-a-weekly-cap-let-the-crossing-request-finish", func(t *testing.T) {
		w := newWorld(t)
		applyLimits(t, w.alpha, tokenPlan)
		customer := billing.CustomerID(uuid.New())
		grantLimitProduct(t, w.alpha, customer, "pro", 24*30)
		require.Nil(t, customerLimit(t, w.alpha, customer, "five_hour").WindowStartsAt, "no use, no session")

		opened := w.clock.Now().UTC().Truncate(time.Second)
		first := admitOne(t, w.alpha, chatAdmission(w, customer, "first", "large", 100_000))
		require.True(t, first.Allowed(), "100k large tokens weigh 500 units")
		five := customerLimit(t, w.alpha, customer, "five_hour")
		require.NotNil(t, five.WindowStartsAt, "first use opens the session")
		require.True(t, opened.Equal(*five.WindowStartsAt))
		require.True(t, opened.Add(5*time.Hour).Equal(*five.ResetsAt))
		require.EqualValues(t, 500, five.Held)
		require.EqualValues(t, 500, five.Remaining)
		weekly := customerLimit(t, w.alpha, customer, "weekly")
		require.EqualValues(t, 500, weekly.Held)
		require.EqualValues(t, 1000, weekly.Remaining)
		captureChat(t, w.alpha, "first", "large", 100_000)
		five = customerLimit(t, w.alpha, customer, "five_hour")
		require.EqualValues(t, 500, five.Used)
		require.Zero(t, five.Held, "a capture ends its hold")

		// A hold issued before the session ends keeps its window.
		w.clock.Advance(4*time.Hour + 59*time.Minute)
		late := admitOne(t, w.alpha, chatAdmission(w, customer, "late", "large", 80_000))
		require.True(t, late.Allowed())
		w.clock.Advance(2 * time.Minute)
		captureChat(t, w.alpha, "late", "large", 120_000)
		five = customerLimit(t, w.alpha, customer, "five_hour")
		require.Nil(t, five.WindowStartsAt, "the capture settles in the ended session and opens none")
		require.Zero(t, five.Used)
		weekly = customerLimit(t, w.alpha, customer, "weekly")
		require.EqualValues(t, 1100, weekly.Used, "600 units settle in full above the 400 held")
		require.EqualValues(t, 400, weekly.Remaining)

		// The weekly cap has 400 left: the crossing request is admitted, holds
		// what remains, and settles in full.
		reopened := w.clock.Now().UTC().Truncate(time.Second)
		crossing := admitOne(t, w.alpha, chatAdmission(w, customer, "crossing", "large", 100_000))
		require.True(t, crossing.Allowed(), "admit_while_positive admits while any capacity remains")
		five = customerLimit(t, w.alpha, customer, "five_hour")
		require.True(t, reopened.Equal(*five.WindowStartsAt), "a use after the session opens the next")
		require.EqualValues(t, 500, five.Held)
		weekly = customerLimit(t, w.alpha, customer, "weekly")
		require.EqualValues(t, 400, weekly.Held, "the hold takes only what remains")
		require.Zero(t, weekly.Remaining)
		captureChat(t, w.alpha, "crossing", "large", 100_000)
		weekly = customerLimit(t, w.alpha, customer, "weekly")
		require.EqualValues(t, 1600, weekly.Used, "never clamped")
		require.EqualValues(t, -100, weekly.Remaining)

		next := admitOne(t, w.alpha, chatAdmission(w, customer, "next", "small", 10_000))
		refusal := requireLimitReached(t, next, "weekly")
		require.NotNil(t, refusal.ResetsAt)
		require.True(t, opened.Add(7*24*time.Hour).Equal(*refusal.ResetsAt), "the weekly window resets a week after its first use")
		require.NotNil(t, refusal.RetryAfterSeconds)
		require.Positive(t, *refusal.RetryAfterSeconds)
		require.EqualValues(t, 500, customerLimit(t, w.alpha, customer, "five_hour").Remaining, "the session still has room")
		_, err := w.alpha.GetAdmission(t.Context(), "next")
		require.ErrorIs(t, err, billing.ErrNotFound, "a refusal holds nothing")
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
			return usageAdmission(w, customer, id, "chat", map[string]int64{"input_tokens": in, "output_tokens": out}, map[string]any{"model": model})
		}
		require.True(t, admitOne(t, w.alpha, tokens("small", "small", 20_000, 5_000)).Allowed(), "20 + 20 units")
		require.EqualValues(t, 40, customerLimit(t, w.alpha, customer, "tokens").Held)
		require.True(t, admitOne(t, w.alpha, tokens("large", "large", 1_000, 5_000)).Allowed(), "1 + 20 + 30 units")
		require.EqualValues(t, 9, customerLimit(t, w.alpha, customer, "tokens").Remaining)
		requireLimitReached(t, admitOne(t, w.alpha, tokens("ten-in", "small", 10_000, 0)), "tokens")
		requireLimitReached(t, admitOne(t, w.alpha, tokens("ten-out", "small", 0, 2_500)), "tokens")
		require.True(t, admitOne(t, w.alpha, tokens("nine", "small", 9_000, 0)).Allowed(), "an estimate that fits exactly")
		require.Zero(t, customerLimit(t, w.alpha, customer, "tokens").Remaining)

		// Each count rounds half up: 20.5 + 20.5 settle as 42, 1.499 + 20 + 30
		// as 51, past what was held.
		captureUsage(t, w.alpha, "small", "chat", map[string]int64{"input_tokens": 20_500, "output_tokens": 5_125}, map[string]any{"model": "small"})
		captureUsage(t, w.alpha, "large", "chat", map[string]int64{"input_tokens": 1_499, "output_tokens": 5_000}, map[string]any{"model": "large"})
		line := customerLimit(t, w.alpha, customer, "tokens")
		require.EqualValues(t, 93, line.Used)
		require.EqualValues(t, 9, line.Held)
		require.EqualValues(t, -2, line.Remaining)

		// A gate costs a request by the same counts.
		other := billing.CustomerID(uuid.New())
		grantLimitProduct(t, w.alpha, other, "pro", 48)
		g := newGate(t, w.alpha, openrails.GateDeps{})
		gateTokens := func(id, model string, in, out int64) billing.AdmitParams {
			return billing.AdmitParams{RequestID: id, CustomerID: other, Invoker: other.String(), InvokerType: billing.InvokerTypeCustomer, Currency: "USD",
				Usage: &billing.UsageEstimate{EventType: "chat", Dimensions: map[string]int64{"input_tokens": in, "output_tokens": out}, Metadata: map[string]any{"model": model}, Amount: ptr(int64(0))}}
		}
		used := func(model string, in, out int64) billing.CaptureUsage {
			return billing.CaptureUsage{Dimensions: map[string]int64{"input_tokens": in, "output_tokens": out}, Metadata: map[string]any{"model": model}}
		}
		eighty := gateAdmit(t, g, gateTokens("eighty", "small", 40_000, 10_000))
		require.NoError(t, eighty.Capture(t.Context(), used("small", 10_000, 5_000)), "30 units of the 80 held")
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

	t.Run("OR-J23-S14/balance-and-a-monthly-spend-cap-hold-both-or-neither", func(t *testing.T) {
		w := newWorld(t, spendCap(billing.BudgetWindow{Key: "monthly", WindowSeconds: 30 * 24 * 3600, Limit: 5000, Currency: "USD"}))
		meterForCapture(t, w)
		customer := billing.CustomerID(uuid.New())
		fund(t, w.alpha, customer, "funded", 8000)

		require.True(t, admitOne(t, w.alpha, reservation(w, customer, "a", 4000)).Allowed())
		balance(t, w.alpha, customer, 4000, 4000, 8000)
		line := customerLimit(t, w.alpha, customer, "balance")
		require.Equal(t, "USD", line.Unit)
		require.Equal(t, billing.CustomerLimit{CustomerID: customer, Limit: "balance", Unit: "USD", Capacity: 8000, Held: 4000, Remaining: 4000}, line,
			"balance is a limit: the prepaid balance, what holds set aside and what remains")

		over := admitOne(t, w.alpha, reservation(w, customer, "b", 2000))
		requireDenied(t, over, "budget_exceeded")
		require.NotNil(t, over.Admission.RetryAfterSeconds)
		require.Positive(t, *over.Admission.RetryAfterSeconds)
		balance(t, w.alpha, customer, 4000, 4000, 8000)

		require.True(t, admitOne(t, w.alpha, reservation(w, customer, "c", 1000)).Allowed(), "the window has exactly 1000 left")
		balance(t, w.alpha, customer, 3000, 5000, 8000)
		released, err := w.alpha.ReleaseAdmissions(t.Context(), []string{"c"})
		require.NoError(t, err)
		require.NoError(t, released[0].Err())
		balance(t, w.alpha, customer, 4000, 4000, 8000)

		// The capture accrues against the balance and settles in the window,
		// in full.
		settled, err := w.alpha.CaptureAdmissions(t.Context(), []billing.CaptureAdmissionParams{capture("a", 4500)})
		require.NoError(t, err)
		require.NoError(t, settled[0].Err())
		require.EqualValues(t, 4500, settled[0].Receipt.Amount)
		balance(t, w.alpha, customer, 3500, 4500, 8000)
		requireDenied(t, admitOne(t, w.alpha, reservation(w, customer, "d", 600)), "budget_exceeded")
		balance(t, w.alpha, customer, 3500, 4500, 8000)
		require.True(t, admitOne(t, w.alpha, reservation(w, customer, "e", 500)).Allowed(), "4500 settled leaves 500 in the window")

		// The balance refuses while the window has room: the window takes nothing.
		thin := billing.CustomerID(uuid.New())
		fund(t, w.alpha, thin, "thin", 500)
		requireDenied(t, admitOne(t, w.alpha, reservation(w, thin, "x", 800)), "insufficient_balance")
		balance(t, w.alpha, thin, 500, 0, 500)
		require.True(t, admitOne(t, w.alpha, reservation(w, thin, "y", 500)).Allowed())
		fund(t, w.alpha, thin, "more", 4500)
		require.True(t, admitOne(t, w.alpha, reservation(w, thin, "z", 4500)).Allowed(), "the refused 800 took nothing from the window")
		fund(t, w.alpha, thin, "spare", 1000)
		requireDenied(t, admitOne(t, w.alpha, reservation(w, thin, "q", 1)), "budget_exceeded")
		balance(t, w.alpha, thin, 1000, 5000, 6000)
	})

	t.Run("OR-J23-S15/four-replicas-racing-for-the-last-units-admit-exactly-the-capacity", func(t *testing.T) {
		w := newWorld(t)
		applyLimits(t, w.alpha, unitPlan)
		customer := billing.CustomerID(uuid.New())
		grantLimitProduct(t, w.alpha, customer, "plan", 24)
		_, err := w.alpha.UpdateCustomer(t.Context(), customer, billing.UpdateCustomerParams{Limits: []billing.LimitOverride{{Limit: "quota", Capacity: 10}}})
		require.NoError(t, err)
		replicas := []*openrails.Client{w.alpha}
		for range 3 {
			engine, err := openrails.New(t.Context(), w.cfg, w.deps)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, engine.Close(context.Background())) })
			client, err := engine.Merchant(w.alpha.MerchantID())
			require.NoError(t, err)
			replicas = append(replicas, client)
		}
		var mu sync.Mutex
		allowed := 0
		var wg sync.WaitGroup
		start := make(chan struct{})
		for r, client := range replicas {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				for i := range 5 {
					verdicts, err := client.Admit(t.Context(), []billing.AdmitParams{usageAdmission(w, customer, fmt.Sprintf("race-%d-%d", r, i), "work", map[string]int64{"units": 1}, nil)})
					if err != nil || len(verdicts) != 1 {
						t.Errorf("admit: %v", err)
						return
					}
					mu.Lock()
					if verdicts[0].Allowed() {
						allowed++
					} else if verdicts[0].Admission == nil || verdicts[0].Admission.DenyCode == nil || *verdicts[0].Admission.DenyCode != billing.DenyLimitReached {
						t.Errorf("a refusal past capacity is limit_reached: %+v", verdicts[0])
					}
					mu.Unlock()
				}
			}()
		}
		close(start)
		wg.Wait()
		require.Equal(t, 10, allowed, "twenty requests for ten units admit exactly ten")
		quota := customerLimit(t, w.alpha, customer, "quota")
		require.EqualValues(t, 10, quota.Held)
		require.Zero(t, quota.Remaining)
	})

	t.Run("OR-J23-S16/an-overrun-settles-past-capacity-and-records-reached-once", func(t *testing.T) {
		w := newWorld(t)
		applyLimits(t, w.alpha, unitPlan)
		customer := billing.CustomerID(uuid.New())
		grantLimitProduct(t, w.alpha, customer, "plan", 24)
		began := w.clock.Now()
		units := func(n int64) map[string]int64 { return map[string]int64{"units": n} }

		require.True(t, admitOne(t, w.alpha, usageAdmission(w, customer, "estimate", "work", units(60), nil)).Allowed())
		require.Zero(t, limitsReached(t, w.alpha, began.Add(-time.Hour), began.Add(time.Hour)), "60 of 100 held is short of capacity")
		captureUsage(t, w.alpha, "estimate", "work", units(130), nil)
		quota := customerLimit(t, w.alpha, customer, "quota")
		require.EqualValues(t, 130, quota.Used, "the capture above its hold posts in full")
		require.EqualValues(t, -30, quota.Remaining)
		require.EqualValues(t, 1, limitsReached(t, w.alpha, began.Add(-time.Hour), began.Add(time.Hour)))
		requireLimitReached(t, admitOne(t, w.alpha, usageAdmission(w, customer, "refused", "work", units(1), nil)), "quota")

		// A refill lets holds grow again; a second overrun records nothing new.
		_, err := w.alpha.AdjustCustomerLimit(t.Context(), customer, billing.AdjustCustomerLimitParams{Limit: "quota", Amount: 50, SourceID: "refill"})
		require.NoError(t, err)
		require.True(t, admitOne(t, w.alpha, usageAdmission(w, customer, "after-refill", "work", units(10), nil)).Allowed())
		captureUsage(t, w.alpha, "after-refill", "work", units(40), nil)
		quota = customerLimit(t, w.alpha, customer, "quota")
		require.EqualValues(t, 170, quota.Used)
		require.EqualValues(t, 150, quota.Capacity)
		requireLimitReached(t, admitOne(t, w.alpha, usageAdmission(w, customer, "refused-again", "work", units(1), nil)), "quota")
		require.EqualValues(t, 1, limitsReached(t, w.alpha, began.Add(-time.Hour), began.Add(time.Hour)), "a window records reaching capacity once")
	})

	t.Run("OR-J23-S17/a-delegated-invoker-is-refused-and-holds-nothing", func(t *testing.T) {
		w := newWorld(t)
		applyLimits(t, w.alpha, unitPlan)
		customer := billing.CustomerID(uuid.New())
		grantLimitProduct(t, w.alpha, customer, "plan", 24)
		fund(t, w.alpha, customer, "funded", 1000)
		delegated := func(p billing.AdmitParams) billing.AdmitParams {
			p.Invoker, p.InvokerType = "agent:"+uuid.NewString(), billing.InvokerTypeDelegated
			return p
		}
		for _, p := range []billing.AdmitParams{
			delegated(reservation(w, customer, "money", 100)),
			delegated(usageAdmission(w, customer, "units", "work", map[string]int64{"units": 5}, nil)),
		} {
			v := admitOne(t, w.alpha, p)
			requireDenied(t, v, "delegated_spend_not_allowed")
			require.Equal(t, 403, v.Status)
			require.Equal(t, billing.AdmissionBlockedByBudget, *v.Admission.BlockedBy)
		}
		balance(t, w.alpha, customer, 1000, 0, 1000)
		quota := customerLimit(t, w.alpha, customer, "quota")
		require.Nil(t, quota.WindowStartsAt, "a refused request opens no window")
		require.Zero(t, quota.Held)
		own := admitOne(t, w.alpha, usageAdmission(w, customer, "own", "work", map[string]int64{"units": 5}, nil))
		require.True(t, own.Allowed(), "the customer's own credential is admitted")
		require.EqualValues(t, 5, customerLimit(t, w.alpha, customer, "quota").Held)
	})

	t.Run("OR-J23-S18/invokers-with-one-limit-set-share-windows-another-trust-level-has-its-own", func(t *testing.T) {
		w := newWorld(t, func(w *world) {
			m := cfgMerchant(w.cfg, "alpha")
			m.BillingPolicies = []billing.BillingPolicy{
				{Name: "free", Kind: "window_spend_cap", SpendWindows: []billing.BudgetWindow{{Key: "free-month", WindowSeconds: 30 * 24 * 3600, Limit: 1000, Currency: "USD"}}},
				{Name: "gold", Kind: "window_spend_cap", SpendWindows: []billing.BudgetWindow{{Key: "gold-month", WindowSeconds: 30 * 24 * 3600, Limit: 5000, Currency: "USD"}}},
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
		require.True(t, admitOne(t, w.alpha, key(reservation(w, customer, "key-one", 600), "apiKey:one", "")).Allowed())
		requireDenied(t, admitOne(t, w.alpha, key(reservation(w, customer, "key-two", 600), "apiKey:two", "")), "budget_exceeded")
		require.True(t, admitOne(t, w.alpha, key(reservation(w, customer, "key-two-small", 400), "apiKey:two", "")).Allowed(),
			"two keys with the same limits share one window")
		require.True(t, admitOne(t, w.alpha, key(reservation(w, customer, "gold-key", 600), "apiKey:three", "gold")).Allowed(),
			"another trust level is held against its own window")
		balance(t, w.alpha, customer, 8400, 1600, 10_000)
	})

	t.Run("OR-J23-S26/an-admission-that-fits-but-for-gate-holds-is-capacity-held-and-takes-them-back", func(t *testing.T) {
		w := newWorld(t)
		applyLimits(t, w.alpha, unitPlan)
		customer := billing.CustomerID(uuid.New())
		grantLimitProduct(t, w.alpha, customer, "plan", 24)
		_, err := w.alpha.UpdateCustomer(t.Context(), customer, billing.UpdateCustomerParams{Limits: []billing.LimitOverride{{Limit: "quota", Capacity: 10}}})
		require.NoError(t, err)
		work := func(id string, units int64) billing.AdmitParams {
			return usageAdmission(w, customer, id, "work", map[string]int64{"units": units}, nil)
		}
		lease := newLease()
		set, refusal := wanted(t, syncLease(t, w.alpha, billing.SyncLeasesParams{LeaseID: lease, Wants: []billing.LeaseWant{workWant(customer, 4)}}))
		require.Nil(t, refusal)
		held := customerLimit(t, w.alpha, customer, "quota").Held
		require.Positive(t, held)

		// One unit more than is free fits only were the gate's hold returned:
		// capacity_held, in the host's own transaction too.
		tx, err := w.pool.Begin(t.Context())
		require.NoError(t, err)
		defer tx.Rollback(context.Background()) //nolint:errcheck
		verdicts, err := w.local.AdmitTx(t.Context(), tx, []billing.AdmitParams{work("fits-if-returned", 10-held+1)})
		require.NoError(t, err)
		require.NoError(t, tx.Commit(t.Context()))
		require.Len(t, verdicts, 1)
		v := verdicts[0]
		requireDenied(t, v, billing.DenyCapacityHeld)
		require.Equal(t, http.StatusTooManyRequests, v.Status)
		require.Equal(t, billing.AdmissionBlockedByLimit, *v.Admission.BlockedBy)
		require.NotNil(t, v.Admission.RetryAfterSeconds)
		require.EqualValues(t, 1, *v.Admission.RetryAfterSeconds)
		require.Nil(t, v.Admission.Limit)
		// More than the capacity would not fit even then.
		tooBig := admitOne(t, w.alpha, work("too-big", 11))
		requireDenied(t, tooBig, billing.DenyLimitReached)
		require.Equal(t, "quota", *tooBig.Admission.Limit)

		// The refusal takes the hold back as a competing want does: the gate's
		// next sync tells it the smaller size, the one after releases it, and
		// the admission fits.
		h, listed := heldIn(syncLease(t, w.alpha, billing.SyncLeasesParams{LeaseID: lease}), customer, set, "quota")
		require.True(t, listed)
		require.Zero(t, h)
		syncLease(t, w.alpha, billing.SyncLeasesParams{LeaseID: lease})
		require.Zero(t, customerLimit(t, w.alpha, customer, "quota").Held)
		require.True(t, admitOne(t, w.alpha, work("fits-now", 10-held+1)).Allowed())

		// The balance the same way.
		usageMeter(t, w.alpha, 100)
		payer := billing.CustomerID(uuid.New())
		fund(t, w.alpha, payer, "funded", 1000)
		set, refusal = wanted(t, syncLease(t, w.alpha, billing.SyncLeasesParams{LeaseID: lease, Wants: []billing.LeaseWant{computeWant(payer, 1)}}))
		require.Nil(t, refusal)
		b, err := w.alpha.GetBalance(t.Context(), payer, billing.GetBalanceParams{Currency: "USD"})
		require.NoError(t, err)
		require.Positive(t, b.HeldAmount)
		requireDenied(t, admitOne(t, w.alpha, reservation(w, payer, "money-if-returned", b.AvailableAmount+1)), billing.DenyCapacityHeld)
		requireDenied(t, admitOne(t, w.alpha, reservation(w, payer, "past-the-balance", b.AvailableAmount+b.HeldAmount+1)), "insufficient_balance")
		h, listed = heldIn(syncLease(t, w.alpha, billing.SyncLeasesParams{LeaseID: lease}), payer, set, "balance")
		require.True(t, listed)
		require.Zero(t, h)
	})
	t.Run("OR-J23-S33/a-spend-window-that-names-no-currency-is-in-its-policys-currency", func(t *testing.T) {
		// ¥100 an hour; at the fixture's rate of 1, $100 of USD requests.
		w := newWorld(t, func(w *world) {
			m := cfgMerchant(w.cfg, "alpha")
			m.BillingPolicies = []billing.BillingPolicy{{Name: "yen", Kind: "window_spend_cap", PolicyCurrency: "JPY",
				SpendWindows: []billing.BudgetWindow{{Key: "hourly", WindowSeconds: 3600, Limit: 1_000_000}}}}
			m.BillingPolicyBindings = []billing.BillingPolicyBinding{{PolicyName: "yen"}}
			setCfgMerchant(&w.cfg, "alpha", m)
		})
		customer := billing.CustomerID(uuid.New())
		fund(t, w.alpha, customer, "funded", 300_000_000)
		require.True(t, admitOne(t, w.alpha, reservation(w, customer, "within", 60_000_000)).Allowed(), "$60 is within ¥100, not past 1,000,000 USD units")
		requireDenied(t, admitOne(t, w.alpha, reservation(w, customer, "past", 50_000_000)), "budget_exceeded")
		balance(t, w.alpha, customer, 240_000_000, 60_000_000, 300_000_000)
	})
	t.Run("OR-J23-S34/a-policys-accrual-rate-cap-is-in-its-policy-currency", func(t *testing.T) {
		// ¥10 an hour; at the fixture's rate of 1, $10 an hour of USD usage.
		w := newWorld(t, func(w *world) {
			m := cfgMerchant(w.cfg, "alpha")
			m.BillingPolicies = []billing.BillingPolicy{{Name: "yen-rate", Kind: "accrual_rate_cap", PolicyCurrency: "JPY", AccrualRateCapPerHour: 100_000}}
			m.BillingPolicyBindings = []billing.BillingPolicyBinding{{PolicyName: "yen-rate"}}
			setCfgMerchant(&w.cfg, "alpha", m)
		})
		customer := billing.CustomerID(uuid.New())
		fund(t, w.alpha, customer, "funded", 100_000_000)
		admit := func(id string, delta int64) billing.AdmissionVerdict {
			t.Helper()
			p := reservation(w, customer, id, 1000)
			p.AccrualRateDeltaPerHour = delta
			return admitOne(t, w.alpha, p)
		}
		require.True(t, admit("within", 4_000_000).Allowed(), "$4 an hour is within ¥10, not past 100,000 USD units")
		requireDenied(t, admit("past", 11_000_000), billing.DenyAccrualRateCapReached)
	})
}
