//go:build e2e && integration

package ci_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"math"
	"os"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/app"
	identity "github.com/open-rails/openrails/internal/billingidentity"
	"github.com/open-rails/openrails/internal/engine"
	postgresmigrations "github.com/open-rails/openrails/internal/migrate/postgres"
	"github.com/open-rails/openrails/internal/modules/money"
)

const usageEvent = "gpu-seconds"

// use records usage of amount for the customer, dated at, or now when at is
// nil.
func (w *chargeWorld) use(customer billing.CustomerID, amount int64, at *time.Time) {
	w.t.Helper()
	_, err := recordUsage(w.ctx, w.client, billing.RecordUsageParams{
		CustomerID: customer, Invoker: customer.String(), Currency: "USD", EventType: usageEvent,
		Dimensions: map[string]int64{"units": 1}, Amount: &amount, Source: "test", SourceID: uuid.NewString(), OccurredAt: at,
	})
	require.NoError(w.t, err)
}

func (w *chargeWorld) hold(customer billing.CustomerID, amount int64) string {
	w.t.Helper()
	requestID := uuid.NewString()
	deadline := w.now().Add(time.Hour)
	verdicts, err := w.client.Admit(w.ctx, []billing.AdmitParams{{RequestID: requestID, CustomerID: customer, Invoker: customer.String(),
		InvokerType: billing.InvokerTypeCustomer, Currency: "USD", EstimatedAmount: amount, ExpiresAt: &deadline}})
	require.NoError(w.t, err)
	require.True(w.t, verdicts[0].Allowed())
	return requestID
}

// notices are the customer's notification events of the application's
// consumer, oldest first.
func (w *chargeWorld) notices(consumer *openrails.NotificationConsumer, customer billing.CustomerID) []billing.NotificationEvent {
	w.t.Helper()
	events, err := consumer.Pending(w.ctx, 100)
	require.NoError(w.t, err)
	var out []billing.NotificationEvent
	for _, e := range events {
		if e.CustomerID == customer {
			out = append(out, e)
		}
	}
	return out
}

func (w *chargeWorld) consumer() *openrails.NotificationConsumer {
	w.t.Helper()
	consumer, err := w.bill.NotificationEvents("balance-" + uuid.NewString()[:8])
	require.NoError(w.t, err)
	require.NoError(w.t, consumer.Register(w.ctx))
	return consumer
}

func noticeData(t *testing.T, e billing.NotificationEvent) billing.NotificationData {
	t.Helper()
	var data billing.NotificationData
	require.NoError(t, json.Unmarshal(e.Data, &data))
	return data
}

func usd(amount int64) *int64 { return &amount }

// A prepaid customer with no running charge that spends only through usage
// goes under its line: one balance.low, with the usage rate and the runway,
// and one notification event for the application. More usage and idle passes
// add none.
func TestUsageAloneFiresBalanceLowOnce(t *testing.T) {
	w := newChargeWorld(t, lowAmount(2_500_000))
	declareDynamicMeter(t, w.client, usageEvent, "units")
	consumer := w.consumer()
	customer := w.funded(10_000_000)
	t0 := w.now()
	w.watch()
	watch := w.watched(customer)
	require.NotNil(t, watch, "a new prepaid balance is watched")
	require.Nil(t, watch.NextCheckAt, "nothing exact moves it")
	require.True(t, watch.RateSampledAt.Equal(t0))

	// Usage dated before the last sample is spent but left out of the rate.
	before := t0.Add(-10 * time.Minute)
	w.use(customer, 6_800_000, &before)
	w.clock.Advance(time.Hour)
	at := w.now()
	w.use(customer, 800_000, nil)
	require.Empty(t, w.events(billing.HostEventBalanceLow, customer), "until the watcher looks")
	w.watch()
	runway := at.Add(3 * time.Hour)
	require.Equal(t, []billing.BalanceHostEvent{{
		CustomerID: customer, Currency: "USD", AvailableAmount: 2_400_000, OwedAmount: 0, AccrualRatePerHour: 0,
		UsageRatePerHour: 800_000, LowBalanceAmount: 2_500_000, AsOf: at, DepletesAt: nil, RunwayEndsAt: &runway,
	}}, w.events(billing.HostEventBalanceLow, customer))
	notices := w.notices(consumer, customer)
	require.Len(t, notices, 1)
	require.Equal(t, "balance_low", notices[0].EventType)
	require.True(t, notices[0].ActionRequested)
	require.True(t, notices[0].CreatedAt.Equal(at))
	require.Equal(t, billing.NotificationData{Currency: "USD", AvailableAmount: usd(2_400_000), OwedAmount: usd(0),
		LowBalanceAmount: usd(2_500_000), RunwayEndsAt: &runway}, noticeData(t, notices[0]))

	w.clock.Advance(time.Minute)
	w.use(customer, 100_000, nil)
	w.watch()
	for range 3 {
		w.clock.Advance(10 * time.Minute)
		w.watch()
	}
	require.Len(t, w.events(billing.HostEventBalanceLow, customer), 1, "once per crossing")
	require.Empty(t, w.events(billing.HostEventBalanceDepleted, customer))
	require.Len(t, w.notices(consumer, customer), 1)
}

// Usage to capacity zero crosses both lines in one look: both host events
// and one notification event (balance_depleted). A deposit above both lines
// re-arms them without a notice, and the next crossing fires again.
func TestBalanceDepletedFiresOnceAndFundingRearms(t *testing.T) {
	w := newChargeWorld(t, lowAmount(2_500_000))
	declareDynamicMeter(t, w.client, usageEvent, "units")
	consumer := w.consumer()
	customer := w.funded(3_000_000)
	w.watch()

	w.clock.Advance(time.Minute)
	at := w.now()
	w.use(customer, 3_000_000, nil)
	w.watch()
	low, depleted := w.events(billing.HostEventBalanceLow, customer), w.events(billing.HostEventBalanceDepleted, customer)
	require.Len(t, low, 1)
	require.Equal(t, []billing.BalanceHostEvent{{
		CustomerID: customer, Currency: "USD", AvailableAmount: 0, OwedAmount: 0, UsageRatePerHour: 3_000_000,
		LowBalanceAmount: 2_500_000, AsOf: at, DepletesAt: &at, RunwayEndsAt: &at,
	}}, depleted)
	notices := w.notices(consumer, customer)
	require.Len(t, notices, 1, "low and depleted in one look are one notice")
	require.Equal(t, "balance_depleted", notices[0].EventType)
	require.Equal(t, usd(0), noticeData(t, notices[0]).AvailableAmount)

	w.clock.Advance(time.Minute)
	w.watch()
	require.Len(t, w.events(billing.HostEventBalanceDepleted, customer), 1)

	w.fund(customer, 10_000_000, nil)
	require.NotNil(t, w.watched(customer).FundedAt, "a funding stamps the watch")
	w.watch()
	watch := w.watched(customer)
	require.Nil(t, watch.LowNotifiedAt, "re-armed")
	require.Nil(t, watch.DepletedAt, "re-armed")
	require.Len(t, w.events(billing.HostEventBalanceLow, customer), 1, "re-arming sends nothing")
	require.Len(t, w.notices(consumer, customer), 1)

	w.clock.Advance(time.Minute)
	w.use(customer, 10_000_000, nil)
	w.watch()
	require.Len(t, w.events(billing.HostEventBalanceLow, customer), 2, "the next crossing fires again")
	require.Len(t, w.events(billing.HostEventBalanceDepleted, customer), 2)
	require.Len(t, w.notices(consumer, customer), 2)
	require.Nil(t, w.watched(customer).FundedAt, "a notice clears the funding")
}

// An admission's hold takes the balance under the line and is noticed at the
// next pass. Its release lifts the balance back above, but only a funding
// re-arms the notice: a new hold sends no second one until a deposit has.
func TestReleasedHoldDoesNotRearm(t *testing.T) {
	w := newChargeWorld(t, lowAmount(2_500_000))
	declareDynamicMeter(t, w.client, usageEvent, "units")
	customer := w.funded(5_000_000)
	w.watch()

	first := w.hold(customer, 3_000_000)
	require.False(t, w.watched(customer).NextCheckAt.After(w.now()), "the admission marks the watch due")
	w.watch()
	low := w.events(billing.HostEventBalanceLow, customer)
	require.Len(t, low, 1, "the hold crossed the line")
	require.EqualValues(t, 2_000_000, low[0].AvailableAmount)

	_, err := releaseAdmission(w.ctx, w.client, first)
	require.NoError(t, err)
	w.watch()
	require.NotNil(t, w.watched(customer).LowNotifiedAt, "a release is not a funding")
	w.hold(customer, 3_000_000)
	w.watch()
	require.Len(t, w.events(billing.HostEventBalanceLow, customer), 1, "hovering at the line is told once")

	w.fund(customer, 1_000_000, nil)
	w.watch()
	require.Nil(t, w.watched(customer).LowNotifiedAt, "3 USD of 6 is free: re-armed")
	// A crossing is keyed by the instant it is seen.
	w.clock.Advance(time.Minute)
	w.hold(customer, 1_000_000)
	w.watch()
	require.Len(t, w.events(billing.HostEventBalanceLow, customer), 2)
}

// A running charge and usage burn together: the line is the burn rate times
// the runway, the look is scheduled at the instant the accrual takes the
// balance under it, and balance.low carries the rate and the runway as the
// formula gives them.
func TestRunwayLineFollowsTheBurnRate(t *testing.T) {
	two := 2
	w := newChargeWorld(t, billing.BalanceNoticeSettings{RunwayHours: &two})
	declareDynamicMeter(t, w.client, usageEvent, "units")
	customer := w.funded(20_000_000)
	t0 := w.now()
	w.start(customer, 3_600_000)
	// 7.2 USD at 3.6 an hour: under it once 12.800001 USD has accrued.
	require.True(t, w.watched(customer).NextCheckAt.Equal(t0.Add(12_800_001*time.Millisecond)), "%s", w.watched(customer).NextCheckAt)

	w.clock.Advance(30 * time.Minute)
	t1 := w.now()
	w.use(customer, 1_200_000, nil)
	w.watch()
	watch := w.watched(customer)
	require.EqualValues(t, 1_200_000, watch.UsageRatePerHour, "the last hour's usage, sampled for the first time")
	// 17 USD available, 9.6 the line at 4.8 an hour: the accrual alone takes
	// 7.400001 USD to reach under it.
	require.True(t, watch.NextCheckAt.Equal(t1.Add(7_400_001*time.Millisecond)), "%s", watch.NextCheckAt)
	require.Empty(t, w.events(billing.HostEventBalanceLow, customer))

	w.clock.Advance(time.Hour)
	t2 := w.now()
	w.use(customer, 6_000_000, nil)
	w.watch()
	rate := int64(math.Round(1_200_000*math.Exp(-1))) + 6_000_000
	burn := 3_600_000 + rate
	runway := t2.Add(time.Duration((7_400_000*3_600_000_000+burn-1)/burn) * time.Microsecond)
	runsOut := t2.Add(7400 * time.Second)
	require.Equal(t, []billing.BalanceHostEvent{{
		CustomerID: customer, Currency: "USD", AvailableAmount: 7_400_000, OwedAmount: 0, AccrualRatePerHour: 3_600_000,
		UsageRatePerHour: rate, LowBalanceAmount: 2 * burn, AsOf: t2, DepletesAt: &runsOut, RunwayEndsAt: &runway,
	}}, w.events(billing.HostEventBalanceLow, customer))
	require.EqualValues(t, rate, w.watched(customer).UsageRatePerHour)
}

// A credit lot whose lapse takes the balance under a line is looked at when
// it lapses, with nothing spent.
func TestLapsingLotCrossingIsScheduled(t *testing.T) {
	w := newChargeWorld(t, lowAmount(2_500_000))
	consumer := w.consumer()
	t0 := w.now()
	lapses := t0.Add(2 * time.Hour)

	kept := w.funded(0)
	w.fund(kept, 2_000_000, nil)
	w.fund(kept, 5_000_000, &lapses)
	gone := w.funded(0)
	w.fund(gone, 5_000_000, &lapses)
	w.watch()
	for _, customer := range []billing.CustomerID{kept, gone} {
		require.True(t, w.watched(customer).NextCheckAt.Equal(lapses), "%s", w.watched(customer).NextCheckAt)
	}
	require.Nil(t, w.watched(kept).DepletesAt, "2 USD never lapses")
	require.True(t, w.watched(gone).DepletesAt.Equal(lapses))

	w.clock.Advance(2*time.Hour - time.Second)
	w.watch()
	require.Empty(t, w.events(billing.HostEventBalanceLow, kept))

	w.clock.Advance(time.Second)
	w.watch()
	low := w.events(billing.HostEventBalanceLow, kept)
	require.Len(t, low, 1)
	require.EqualValues(t, 2_000_000, low[0].AvailableAmount, "the lapsed lot is expired first")
	require.Nil(t, low[0].DepletesAt)
	require.Empty(t, w.events(billing.HostEventBalanceDepleted, kept))
	require.Len(t, w.events(billing.HostEventBalanceLow, gone), 1)
	require.Len(t, w.events(billing.HostEventBalanceDepleted, gone), 1)
	gotNotices := w.notices(consumer, gone)
	require.Len(t, gotNotices, 1)
	require.Equal(t, "balance_depleted", gotNotices[0].EventType)
	require.Nil(t, w.watched(kept).NextCheckAt, "nothing left to schedule")
}

// A customer's own line overrides the merchant's, part by part: set by staff,
// 0 turns a part off, null inherits, and a change re-arms the low notice.
// balance.depleted cannot be turned off.
func TestCustomerLineOverridesMerchantDefault(t *testing.T) {
	w := newChargeWorld(t, lowAmount(2_500_000))
	declareDynamicMeter(t, w.client, usageEvent, "units")
	customer := w.funded(4_000_000)
	w.watch()
	require.Empty(t, w.events(billing.HostEventBalanceLow, customer), "4 USD is above the merchant's 2.5")

	update := func(lines ...billing.LowBalanceLine) *billing.CustomerSettingsResult {
		t.Helper()
		out, err := w.client.UpdateCustomer(w.ctx, customer, billing.UpdateCustomerParams{LowBalanceLines: lines})
		require.NoError(t, err)
		return out
	}
	got := update(billing.LowBalanceLine{Currency: "usd", Amount: usd(5_000_000)})
	require.Equal(t, []billing.LowBalanceLine{{Currency: "USD", Amount: usd(5_000_000)}}, got.Settings.LowBalanceLines)
	w.watch()
	low := w.events(billing.HostEventBalanceLow, customer)
	require.Len(t, low, 1)
	require.EqualValues(t, 5_000_000, low[0].LowBalanceAmount)

	// The same line again changes nothing and re-arms nothing.
	w.clock.Advance(time.Minute)
	update(billing.LowBalanceLine{Currency: "USD", Amount: usd(5_000_000)})
	require.NotNil(t, w.watched(customer).LowNotifiedAt, "an unchanged line keeps the notice")
	w.watch()
	require.Len(t, w.events(billing.HostEventBalanceLow, customer), 1)

	update(billing.LowBalanceLine{Currency: "USD", Amount: usd(3_000_000)})
	require.Nil(t, w.watched(customer).LowNotifiedAt, "a new line re-arms the notice")
	w.watch()
	require.Len(t, w.events(billing.HostEventBalanceLow, customer), 1, "4 USD is above 3")

	// Null inherits the merchant's 2.5; then 0 turns the amount off.
	update(billing.LowBalanceLine{Currency: "USD"})
	lines, err := engine.Graph(w.client).Runtime.MoneyService.LowBalanceLines(w.ctx, identity.CustomerID(customer), []string{"USD"})
	require.NoError(t, err)
	require.Equal(t, []billing.EffectiveLowBalanceLine{{Currency: "USD", Amount: 2_500_000, RunwayHours: 0, Source: billing.LowBalanceLineMerchant}}, lines)
	zero := 0
	update(billing.LowBalanceLine{Currency: "USD", Amount: usd(0), RunwayHours: &zero})
	w.clock.Advance(time.Minute)
	w.use(customer, 4_000_000, nil)
	w.watch()
	require.Len(t, w.events(billing.HostEventBalanceLow, customer), 1, "no low line")
	require.Len(t, w.events(billing.HostEventBalanceDepleted, customer), 1, "depleted always fires")

	_, err = w.client.UpdateCustomer(w.ctx, customer, billing.UpdateCustomerParams{LowBalanceLines: []billing.LowBalanceLine{{Currency: "USD", RunwayHours: new(billing.MaxLowBalanceRunwayHours + 1)}}})
	var se *billing.StatusError
	require.ErrorAs(t, err, &se)
	require.Equal(t, billing.CodeInvalidParam, se.Code)
	require.Equal(t, "low_balance_lines[0].runway_hours", *se.Param)
}

// An arrears customer with a running charge may spend what is available plus
// the credit line left: its line and its run-out are judged against that.
func TestArrearsRunningChargeLineCountsTheCreditLine(t *testing.T) {
	w := newChargeWorld(t, lowAmount(2_500_000))
	customer := billing.CustomerID(uuid.New())
	_, err := w.client.UpdateCustomer(w.ctx, customer, billing.UpdateCustomerParams{CreditLimits: []billing.CreditLimit{{Currency: "USD", Amount: 10_000_000}}})
	require.NoError(t, err)
	t0 := w.now()
	w.start(customer, 3_600_000)
	watch := w.watched(customer)
	require.NotNil(t, watch, "watched while a charge runs")
	// 10 USD of credit line at 3.6 an hour: under 2.5 once 7.500001 USD has
	// accrued, at zero once 10 USD has.
	require.True(t, watch.NextCheckAt.Equal(t0.Add(7_500_001*time.Millisecond)), "%s", watch.NextCheckAt)
	runsOut := t0.Add(10_000 * time.Second)
	require.True(t, watch.DepletesAt.Equal(runsOut))

	// A charge accrues by the whole second: 7,501 seconds are 7.501 USD.
	w.clock.Advance(7501 * time.Second)
	at := w.now()
	w.watch()
	require.Equal(t, []billing.BalanceHostEvent{{
		CustomerID: customer, Currency: "USD", AvailableAmount: -7_501_000, OwedAmount: 0, AccrualRatePerHour: 3_600_000,
		LowBalanceAmount: 2_500_000, AsOf: at, DepletesAt: &runsOut, RunwayEndsAt: &runsOut,
	}}, w.events(billing.HostEventBalanceLow, customer))
	require.Empty(t, w.events(billing.HostEventBalanceDepleted, customer))
	w.clock.Advance(runsOut.Sub(at))
	w.watch()
	require.Len(t, w.events(billing.HostEventBalanceDepleted, customer), 1)
}

// With no line set anywhere, the built-in one is a 24-hour runway at the
// burn rate.
func TestDefaultRunwayIsADay(t *testing.T) {
	w := newChargeWorld(t, billing.BalanceNoticeSettings{})
	customer := w.funded(30_000_000)
	t0 := w.now()
	w.start(customer, 1_000_000)
	lines, err := engine.Graph(w.client).Runtime.MoneyService.LowBalanceLines(w.ctx, identity.CustomerID(customer), []string{"USD"})
	require.NoError(t, err)
	require.Equal(t, []billing.EffectiveLowBalanceLine{{Currency: "USD", Amount: 0, RunwayHours: 24, Source: billing.LowBalanceLineDefault}}, lines)
	// 24 USD at 1 an hour: under it once 6.000001 USD has accrued.
	lowAt := t0.Add(21_600_003_600 * time.Microsecond)
	require.True(t, w.watched(customer).NextCheckAt.Equal(lowAt), "%s", w.watched(customer).NextCheckAt)

	// A charge accrues by the whole second: 6 USD at 21,600 seconds is at the
	// line, 21,601 under it.
	w.clock.Advance(21_600 * time.Second)
	w.watch()
	require.Empty(t, w.events(billing.HostEventBalanceLow, customer))
	w.clock.Advance(time.Second)
	w.watch()
	low := w.events(billing.HostEventBalanceLow, customer)
	require.Len(t, low, 1)
	require.EqualValues(t, 24_000_000, low[0].LowBalanceAmount)
}

// Spends in a burst write the watch once per pass: a row already due is not
// written again. With a thousand balances watched and one spending, a pass
// looks at one, and an idle pass reads no row.
func TestSpendBurstWritesTheWatchOncePerPass(t *testing.T) {
	w := newChargeWorld(t, lowAmount(2_500_000))
	declareDynamicMeter(t, w.client, usageEvent, "units")
	customer := w.funded(50_000_000)
	w.watch()
	table := pgx.Identifier{w.f.schema, "balance_watches"}.Sanitize()
	xmin := func(c billing.CustomerID) string {
		t.Helper()
		var out string
		require.NoError(t, w.f.pool.QueryRow(t.Context(), "SELECT xmin::text FROM "+table+" WHERE merchant_id=$1 AND customer_id=$2",
			w.client.MerchantID().UUID(), c.UUID()).Scan(&out))
		return out
	}

	w.use(customer, 1_000_000, nil)
	marked := xmin(customer)
	w.use(customer, 1_000_000, nil)
	request := w.hold(customer, 2_000_000)
	_, err := captureAdmission(w.ctx, w.client, request, 1_500_000, billing.CaptureUsage{EventType: usageEvent, Dimensions: map[string]int64{"units": 1}})
	require.NoError(t, err)
	require.Equal(t, marked, xmin(customer), "the burst after the first spend writes nothing more")

	// A thousand other watched balances, none due.
	_, err = w.f.pool.Exec(t.Context(), fmt.Sprintf(`WITH c AS (
		INSERT INTO %s (merchant_id, id) SELECT $1, gen_random_uuid() FROM generate_series(1, 1000) RETURNING merchant_id, id)
		INSERT INTO %s (merchant_id, customer_id, currency, next_check_at, usage_rate_per_hour) SELECT merchant_id, id, 'USD', NULL, 0 FROM c`,
		pgx.Identifier{w.f.schema, "customers"}.Sanitize(), table), w.client.MerchantID().UUID())
	require.NoError(t, err)
	snapshot := func() map[string]string {
		t.Helper()
		rows, err := w.f.pool.Query(t.Context(), "SELECT customer_id::text, xmin::text FROM "+table+" WHERE merchant_id=$1", w.client.MerchantID().UUID())
		require.NoError(t, err)
		out := map[string]string{}
		for rows.Next() {
			var id, x string
			require.NoError(t, rows.Scan(&id, &x))
			out[id] = x
		}
		require.NoError(t, rows.Err())
		return out
	}
	before := snapshot()
	require.Len(t, before, 1001)
	w.watch()
	after := snapshot()
	var looked []string
	for id, x := range after {
		if before[id] != x {
			looked = append(looked, id)
		}
	}
	require.Equal(t, []string{customer.UUID().String()}, looked, "one look for the one that spent")

	_, err = w.f.pool.Exec(t.Context(), "ANALYZE "+table)
	require.NoError(t, err)
	var plan []map[string]any
	require.NoError(t, w.f.pool.QueryRow(t.Context(), `EXPLAIN (ANALYZE, FORMAT JSON) SELECT merchant_id, customer_id, currency, next_check_at FROM `+table+`
		WHERE next_check_at <= $1 ORDER BY next_check_at, merchant_id, customer_id, currency LIMIT 200`, w.now()).Scan(&plan))
	var read float64
	var indexes []string
	var walk func(map[string]any)
	walk = func(node map[string]any) {
		if index, ok := node["Index Name"].(string); ok {
			indexes = append(indexes, index)
		}
		if rel, ok := node["Relation Name"]; ok && rel == "balance_watches" {
			read += node["Actual Rows"].(float64)
			if removed, ok := node["Rows Removed by Filter"].(float64); ok {
				read += removed
			}
		}
		children, _ := node["Plans"].([]any)
		for _, child := range children {
			walk(child.(map[string]any))
		}
	}
	walk(plan[0]["Plan"].(map[string]any))
	require.Equal(t, []string{"balance_watches_next_check_at_idx"}, indexes, "the due read is indexed")
	require.Zero(t, read, "an idle pass reads no row")
}

// Every prepaid balance is watched and an arrears one is not; a billing-mode
// change adds and removes the watch.
func TestEveryPrepaidBalanceIsWatched(t *testing.T) {
	w := newChargeWorld(t)
	prepaid := w.funded(1_000_000)
	require.NotNil(t, w.watched(prepaid))

	rt := engine.Graph(w.client).Runtime
	mode := func(customer billing.CustomerID, m string) {
		t.Helper()
		require.NoError(t, rt.DB.RunInMerchantConn(w.ctx, func(ctx context.Context) error {
			_, err := rt.MoneyService.UpsertAccountSettings(ctx, identity.CustomerID(customer), "USD", money.AccountSettingsInput{BillingMode: &m})
			return err
		}))
	}
	arrears := billing.CustomerID(uuid.New())
	_, err := w.client.UpdateCustomer(w.ctx, arrears, billing.UpdateCustomerParams{})
	require.NoError(t, err)
	mode(arrears, money.BillingModeArrears)
	w.fund(arrears, 1_000_000, nil)
	require.Nil(t, w.watched(arrears), "an arrears balance is not watched")

	mode(arrears, money.BillingModePrepaid)
	require.False(t, w.watched(arrears).NextCheckAt.After(w.now()), "watched, and due")
	w.watch()
	require.NotNil(t, w.watched(arrears))

	mode(prepaid, money.BillingModeArrears)
	require.NotNil(t, w.watched(prepaid), "until the next pass")
	w.watch()
	require.Nil(t, w.watched(prepaid), "an arrears customer with no running charge")

	// With a running charge it is watched whatever its mode.
	w.start(prepaid, 3_600_000)
	require.NotNil(t, w.watched(prepaid))
}

// The migration watches every existing prepaid balance, nothing due, and an
// arrears one not; it drops the running charge's low line.
func TestBalanceNoticesMigrationBackfillsPrepaidWatches(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("OPENRAILS_E2E_DSN"))
	require.NotEmpty(t, dsn)
	pool := openPool(t, dsn)
	f := &fixture{pool: pool, schema: "e2e_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:16]}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+pgx.Identifier{f.schema}.Sanitize()+" CASCADE")
	})
	files, err := fs.Glob(postgresmigrations.FS, "*.up.sql")
	require.NoError(t, err)
	before := fstest.MapFS{}
	for _, name := range files {
		if strings.HasSuffix(name, "_balance_notices.up.sql") {
			break
		}
		body, err := fs.ReadFile(postgresmigrations.FS, name)
		require.NoError(t, err)
		before[name] = &fstest.MapFile{Data: body}
	}
	require.Less(t, len(before), len(files))
	cfg := f.config()
	cfg.DB = &openrails.DBConfig{URL: dsn}
	application, err := app.BootstrapWithOptions(t.Context(), &cfg, &app.BootstrapOptions{PGXPool: pool, Migrations: before})
	require.NoError(t, err)
	require.NoError(t, application.Close(t.Context()))
	table := func(name string) string { return pgx.Identifier{f.schema, name}.Sanitize() }
	exec := func(sql string, args ...any) {
		t.Helper()
		_, err := pool.Exec(t.Context(), sql, args...)
		require.NoError(t, err)
	}
	mid := uuid.New()
	prepaid, arrears, revoked, charged := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	exec(`INSERT INTO `+table("merchants")+` (id, slug, status) VALUES ($1, 'balance-notices-backfill', 'active')`, mid)
	for _, c := range []uuid.UUID{prepaid, arrears, revoked, charged} {
		exec(`INSERT INTO `+table("customers")+` (merchant_id, id) VALUES ($1, $2)`, mid, c)
		exec(`INSERT INTO `+table("ledger_accounts")+` (merchant_id, customer_id, account_type, currency, debits_must_not_exceed_credits, credits_must_not_exceed_debits)
			VALUES ($1, $2, 'customer_balance', 'USD', true, false)`, mid, c)
	}
	exec(`INSERT INTO `+table("money_settings")+` (merchant_id, customer_id, currency, billing_mode) VALUES ($1, $2, 'USD', 'arrears')`, mid, arrears)
	exec(`INSERT INTO `+table("money_settings")+` (merchant_id, customer_id, currency, billing_mode, collection_revoked_at) VALUES ($1, $2, 'USD', 'arrears', now())`, mid, revoked)
	exec(`INSERT INTO `+table("money_settings")+` (merchant_id, customer_id, currency, billing_mode) VALUES ($1, $2, 'USD', 'arrears')`, mid, charged)
	due := time.Now().UTC().Truncate(time.Microsecond)
	exec(`INSERT INTO `+table("balance_watches")+` (merchant_id, customer_id, currency, next_check_at) VALUES ($1, $2, 'USD', $3)`, mid, charged, due)

	bill, err := openrails.New(t.Context(), f.config(), openrails.Deps{FXTransport: testFX.Transport(), Postgres: pool})
	require.NoError(t, err)
	require.NoError(t, bill.Close(t.Context()))
	rows, err := pool.Query(t.Context(), `SELECT customer_id, next_check_at, usage_rate_per_hour FROM `+table("balance_watches")+` WHERE merchant_id=$1`, mid)
	require.NoError(t, err)
	watched := map[uuid.UUID]*time.Time{}
	for rows.Next() {
		var id uuid.UUID
		var next *time.Time
		var rate int64
		require.NoError(t, rows.Scan(&id, &next, &rate))
		require.Zero(t, rate)
		watched[id] = next
	}
	require.NoError(t, rows.Err())
	require.Len(t, watched, 3)
	require.Contains(t, watched, prepaid)
	require.Contains(t, watched, revoked, "a revoked agreement spends only its balance")
	require.NotContains(t, watched, arrears)
	require.Nil(t, watched[prepaid], "not due")
	require.True(t, watched[charged].Equal(due), "a running charge's watch is kept")
	var dropped bool
	require.NoError(t, pool.QueryRow(t.Context(), `SELECT NOT EXISTS (SELECT 1 FROM information_schema.columns
		WHERE table_schema=$1 AND table_name='running_charges' AND column_name='low_balance_amount')`, f.schema).Scan(&dropped))
	require.True(t, dropped)
}
