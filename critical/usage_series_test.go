//go:build integration

package critical_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
)

// seriesTotal sums a series' amounts, events and discount.
func seriesTotal(s *billing.UsageSeries) (amount, events, discount int64) {
	for _, b := range s.Buckets {
		discount += b.Discount
		for _, r := range b.Rows {
			amount, events = amount+r.Amount, events+r.EventCount
		}
	}
	return amount, events, discount
}

// nonEmpty are a series' buckets with rows, by local start.
func nonEmpty(s *billing.UsageSeries) map[string][]billing.UsageRow {
	out := map[string][]billing.UsageRow{}
	for _, b := range s.Buckets {
		if len(b.Rows) > 0 {
			out[b.StartsAt.UTC().Format(time.RFC3339)] = b.Rows
		}
	}
	return out
}

func compute(events, amount int64) []billing.UsageRow {
	return []billing.UsageRow{{Key: "compute", EventCount: events, Amount: amount}}
}

func zoneOf(t *testing.T, name string) *time.Location {
	t.Helper()
	zone, err := time.LoadLocation(name)
	require.NoError(t, err)
	return zone
}

func TestUsageSeries(t *testing.T) {
	t.Run("OR-J24-S76/an-hourly-series-across-a-clock-change-has-each-of-the-days-hours", func(t *testing.T) {
		newYork := zoneOf(t, "America/New_York")
		// The next New York day of 25 hours, when its clocks fall back.
		day := calendarDayStart(time.Now().Add(24*time.Hour), newYork)
		for day.AddDate(0, 0, 1).Sub(day) != 25*time.Hour {
			day = day.AddDate(0, 0, 1)
		}
		next := day.AddDate(0, 0, 1)
		w := newWorld(t)
		w.clock.Advance(next.Add(time.Hour).Sub(w.clock.Now()))
		usageMeter(t, w.alpha, 100)
		customer, _ := fundedCustomer(t, w)
		// 01:30 before the clocks fall back and 01:30 after, an hour apart.
		before, after := usageInput(customer, "before", 2), usageInput(customer, "after", 3)
		before.OccurredAt, after.OccurredAt = day.Add(90*time.Minute), day.Add(150*time.Minute)
		settleUsage(t, w.alpha, leaseMintedAt(w.clock.Now()), numbered(1, before, after)...)

		series, err := w.alpha.GetUsageSeries(t.Context(), customer, billing.GetUsageSeriesParams{Currency: "USD", Interval: billing.UsageByHour,
			Timezone: "America/New_York", From: day, To: next})
		require.NoError(t, err)
		require.Equal(t, "America/New_York", series.Timezone)
		require.Len(t, series.Buckets, 25, "the day has 25 hours")
		require.True(t, series.From.Equal(day) && series.To.Equal(next), "the series spans the local day: %s to %s", series.From, series.To)
		for i, b := range series.Buckets {
			require.True(t, b.StartsAt.Equal(day.Add(time.Duration(i)*time.Hour)), "hour %d starts at %s", i, b.StartsAt)
		}
		require.Equal(t, "01:00", series.Buckets[1].StartsAt.In(newYork).Format("15:04"))
		require.Equal(t, "01:00", series.Buckets[2].StartsAt.In(newYork).Format("15:04"), "the repeated hour is its own bucket")
		require.Equal(t, map[string][]billing.UsageRow{
			day.Add(time.Hour).UTC().Format(time.RFC3339):     compute(1, 200),
			day.Add(2 * time.Hour).UTC().Format(time.RFC3339): compute(1, 300),
		}, nonEmpty(series))

		// By day the same window is one bucket, the sum of its hours.
		days, err := w.alpha.GetUsageSeries(t.Context(), customer, billing.GetUsageSeriesParams{Currency: "USD", Interval: billing.UsageByDay,
			Timezone: "America/New_York", From: day, To: next})
		require.NoError(t, err)
		require.Len(t, days.Buckets, 1)
		require.Equal(t, compute(2, 500), days.Buckets[0].Rows)
	})

	t.Run("OR-J24-S77/a-kolkata-day-series-cuts-its-days-at-the-half-hour", func(t *testing.T) {
		w := newWorld(t)
		usageMeter(t, w.alpha, 100)
		customer, _ := fundedCustomer(t, w)
		// 18:15 UTC is 23:45 in Kolkata, 18:45 UTC 00:15 the next day: one
		// UTC hour, two Kolkata days.
		base := w.clock.Now().UTC().Truncate(24 * time.Hour).Add(-24 * time.Hour)
		late, early := usageInput(customer, "late-evening", 2), usageInput(customer, "after-midnight", 3)
		late.OccurredAt, early.OccurredAt = base.Add(18*time.Hour+15*time.Minute), base.Add(18*time.Hour+45*time.Minute)
		settleUsage(t, w.alpha, newLease(), numbered(1, late, early)...)
		require.EqualValues(t, 2, storeCount(t, w, "SELECT count() FROM {db}.usage_history WHERE merchant_id = {m} AND meter = ''"), "two quarter hours")

		params := billing.GetUsageSeriesParams{Currency: "USD", Interval: billing.UsageByDay, Timezone: "Asia/Kolkata",
			From: late.OccurredAt.Add(-time.Hour), To: early.OccurredAt.Add(time.Hour)}
		series, err := w.alpha.GetUsageSeries(t.Context(), customer, params)
		require.NoError(t, err)
		kolkataDay := base.Add(-24*time.Hour + 18*time.Hour + 30*time.Minute)
		require.Len(t, series.Buckets, 2)
		require.Equal(t, map[string][]billing.UsageRow{
			kolkataDay.Format(time.RFC3339):                     compute(1, 200),
			kolkataDay.Add(24 * time.Hour).Format(time.RFC3339): compute(1, 300),
		}, nonEmpty(series), "23:45 is the day before 00:15 in Kolkata")

		// The report on Kolkata's days agrees; on the merchant's UTC days both
		// are one day.
		report, err := w.alpha.GetUsage(t.Context(), customer, billing.GetUsageParams{Currency: "USD", Timezone: "Asia/Kolkata",
			From: late.OccurredAt, To: late.OccurredAt.Add(time.Minute)})
		require.NoError(t, err)
		require.Equal(t, "Asia/Kolkata", report.Timezone)
		require.Equal(t, compute(1, 200), report.Rows)
		params.Timezone = ""
		utc, err := w.alpha.GetUsageSeries(t.Context(), customer, params)
		require.NoError(t, err)
		require.Equal(t, "UTC", utc.Timezone, "the merchant's accounting zone by default")
		require.Equal(t, map[string][]billing.UsageRow{base.Format(time.RFC3339): compute(2, 500)}, nonEmpty(utc))
	})

	t.Run("OR-J24-S78/usage-older-than-its-raw-events-stays-in-the-series-and-by-invoker-is-refused", func(t *testing.T) {
		// The world runs 120 days behind the event store's clock, so the
		// raw events' 90-day TTL has passed when the world catches up.
		w := newWorld(t, func(w *world) {
			w.clock = clockwork.NewFakeClockAt(time.Now().UTC().Add(-120 * 24 * time.Hour))
			w.deps.Clock = w.clock
		})
		usageMeter(t, w.alpha, 100)
		customer, _ := fundedCustomer(t, w)
		used := w.clock.Now().UTC()
		app, phone := usageInput(customer, "app", 2), usageInput(customer, "phone", 3)
		app.Invoker, phone.Invoker = "app", "phone"
		settleUsage(t, w.alpha, leaseMintedAt(used), numbered(1, app, phone)...)
		w.clock.Advance(121 * 24 * time.Hour)
		require.NoError(t, w.deps.ClickHouse.Exec(t.Context(), "ALTER TABLE `"+w.schema+"`.usage_events MATERIALIZE TTL SETTINGS mutations_sync = 2"))
		require.Zero(t, storeCount(t, w, "SELECT count() FROM {db}.usage_events FINAL WHERE merchant_id = {m}"), "the raw events expired")
		require.EqualValues(t, 2, storeCount(t, w, "SELECT toUInt64(sum(requests)) FROM {db}.usage_history WHERE merchant_id = {m} AND meter = ''"), "history has no TTL")

		params := billing.GetUsageSeriesParams{Currency: "USD", Interval: billing.UsageByDay, From: used.Add(-time.Hour), To: w.clock.Now()}
		series, err := w.alpha.GetUsageSeries(t.Context(), customer, params)
		require.NoError(t, err)
		amount, events, _ := seriesTotal(series)
		require.Equal(t, [2]int64{500, 2}, [2]int64{amount, events})
		require.Equal(t, map[string][]billing.UsageRow{calendarDayStart(used, time.UTC).Format(time.RFC3339): compute(2, 500)}, nonEmpty(series))

		// By invoker the series reads raw events: refused before them, and
		// within them it answers.
		params.GroupBy = billing.UsageByInvoker
		_, err = w.alpha.GetUsageSeries(t.Context(), customer, params)
		code, ok := billing.ErrorCodeFrom(err)
		require.True(t, ok, "%v", err)
		require.Equal(t, billing.CodeUsageEventsExpired, code)
		_, err = w.alpha.GetUsage(t.Context(), customer, billing.GetUsageParams{Currency: "USD", GroupBy: billing.UsageByInvoker, From: params.From, To: params.To})
		code, _ = billing.ErrorCodeFrom(err)
		require.Equal(t, billing.CodeUsageEventsExpired, code, "the report too")
		params.From = w.clock.Now().Add(-30 * 24 * time.Hour)
		recent, err := w.alpha.GetUsageSeries(t.Context(), customer, params)
		require.NoError(t, err)
		require.Empty(t, nonEmpty(recent))
	})

	t.Run("OR-J24-S79/a-series-past-its-bucket-cap-is-refused-and-a-month-by-hour-is-not", func(t *testing.T) {
		w := newWorld(t)
		usageMeter(t, w.alpha, 100)
		customer, _ := fundedCustomer(t, w)
		now := w.clock.Now()
		_, err := w.alpha.GetUsageSeries(t.Context(), customer, billing.GetUsageSeriesParams{Currency: "USD", Interval: billing.UsageByHour,
			From: now.AddDate(0, 0, -60), To: now})
		var failure *billing.StatusError
		require.ErrorAs(t, err, &failure)
		require.Equal(t, billing.CodeTooManyBuckets, failure.Code, failure.Message)
		require.Equal(t, http.StatusBadRequest, failure.Status)
		require.NotNil(t, failure.Param)
		require.Equal(t, "interval", *failure.Param)

		month, err := w.alpha.GetUsageSeries(t.Context(), customer, billing.GetUsageSeriesParams{Currency: "USD", Interval: billing.UsageByHour,
			From: now.AddDate(0, 0, -31), To: now})
		require.NoError(t, err)
		require.GreaterOrEqual(t, len(month.Buckets), 31*24)
		require.LessOrEqual(t, len(month.Buckets), billing.MaxUsageBuckets)
		_, err = w.alpha.GetUsageSeries(t.Context(), customer, billing.GetUsageSeriesParams{Currency: "USD", Interval: "minute"})
		code, _ := billing.ErrorCodeFrom(err)
		require.Equal(t, billing.CodeInvalidQuery, code)
	})

	t.Run("OR-J24-S80/a-customer-reads-only-its-own-usage-over-time", func(t *testing.T) {
		a := newAuthorityWorld(t)
		usageMeter(t, a.alphaRemote, 100)
		alice, bob := billing.CustomerID(uuid.MustParse(a.alice.ID)), billing.CustomerID(uuid.MustParse(a.bob.ID))
		settleUsage(t, a.alphaRemote, newLease(), numbered(1, usageInput(alice, "alice-own", 2), usageInput(bob, "bob-own", 3))...)
		read := func(token string, extra url.Values) billing.UsageSeries {
			t.Helper()
			q := url.Values{"currency": {"USD"}, "interval": {"day"}}
			for k, v := range extra {
				q[k] = v
			}
			status, raw := a.request(t, http.MethodGet, "/v1/me/usage/series?"+q.Encode(), token, nil)
			require.Equal(t, http.StatusOK, status, string(raw))
			var series billing.UsageSeries
			require.NoError(t, json.Unmarshal(raw, &series))
			return series
		}
		own := read(a.aliceToken, nil)
		require.Equal(t, alice, own.CustomerID)
		amount, events, _ := seriesTotal(&own)
		require.Equal(t, [2]int64{200, 1}, [2]int64{amount, events})
		named := read(a.aliceToken, url.Values{"customer_id": {bob.String()}})
		require.Equal(t, alice, named.CustomerID, "a request names no other customer")
		require.Equal(t, own.Buckets, named.Buckets)
		theirs := read(a.bobToken, nil)
		amount, events, _ = seriesTotal(&theirs)
		require.Equal(t, [2]int64{300, 1}, [2]int64{amount, events})

		// Staff read the customer's series as the customer does; a customer
		// cannot read staff's.
		staff, err := a.alphaRemote.GetUsageSeries(t.Context(), bob, billing.GetUsageSeriesParams{Currency: "USD", Interval: billing.UsageByDay})
		require.NoError(t, err)
		require.Equal(t, theirs.Buckets, staff.Buckets)
		status, raw := a.request(t, http.MethodGet, "/v1/admin/customers/"+bob.String()+"/usage/series?currency=USD&interval=day", a.aliceToken, nil)
		require.Contains(t, []int{http.StatusUnauthorized, http.StatusForbidden}, status, string(raw))
	})

	t.Run("OR-J24-S81/a-volume-tiers-fall-is-its-own-discount-and-totals-equal-what-posted", func(t *testing.T) {
		w := newWorld(t, earlyInMonth)
		pricedMeter(t, w.alpha, tiers(catalog.TierModeVolume, 10, 5, 10))
		customer, _ := fundedCustomer(t, w)
		lease := newLease()
		day := w.clock.Now().UTC()
		settleUsage(t, w.alpha, lease, numbered(1, usageInput(customer, "first", 8))...)
		fleet := startFleet(t, w)
		nextDay(w)
		postDays(t, fleet)
		// 12 units cost 60 at the cheaper band, 20 less than the 80 posted:
		// the second item is taken at no price and a discount of 20.
		second := usageInput(customer, "second", 4)
		second.OccurredAt = day.Add(time.Hour)
		settleUsage(t, w.alpha, lease, numbered(2, second)...)
		nextDay(w)
		postDays(t, fleet)
		w.clock.Advance(70 * 24 * time.Hour)
		postDays(t, fleet)
		var posted int64
		for _, d := range usageDeductions(t, w.alpha, customer) {
			posted += d.Usage.Amount
		}
		require.EqualValues(t, 60, posted, "the period posts the price of its total")

		from, to := calendarDayStart(day, time.UTC), calendarDayStart(day, time.UTC).AddDate(0, 0, 1)
		report, err := w.alpha.GetUsage(t.Context(), customer, billing.GetUsageParams{Currency: "USD", From: from, To: to})
		require.NoError(t, err)
		require.Equal(t, compute(2, 80), report.Rows, "each item at the price it was taken at")
		require.EqualValues(t, 20, report.Discount, "the fall is its own line")
		require.Equal(t, posted, report.Rows[0].Amount-report.Discount, "the report less its discount is what posted")
		series, err := w.alpha.GetUsageSeries(t.Context(), customer, billing.GetUsageSeriesParams{Currency: "USD", Interval: billing.UsageByHour, From: from, To: to})
		require.NoError(t, err)
		amount, _, discount := seriesTotal(series)
		require.Equal(t, posted, amount-discount, "so is the series'")
		for _, b := range series.Buckets {
			if b.Discount != 0 {
				require.True(t, b.StartsAt.Equal(second.OccurredAt.Truncate(time.Hour)), "the discount is in the hour of the usage that crossed the band")
			}
		}
	})

	t.Run("OR-J24-S82/failed-usage-is-counted-on-its-event-row-not-a-row-of-its-own", func(t *testing.T) {
		w := newWorld(t)
		usageMeter(t, w.alpha, 100)
		customer, _ := fundedCustomer(t, w)
		failed := usageInput(customer, "failed", 1)
		failed.Outcome, failed.Amount = billing.UsageFailed, ptr(int64(70))
		settleUsage(t, w.alpha, newLease(), numbered(1, usageInput(customer, "ok", 2), failed)...)
		require.EqualValues(t, 1, storeCount(t, w, "SELECT count() FROM {db}.usage_history WHERE merchant_id = {m} AND meter = ''"), "one row for the bucket's events")
		require.EqualValues(t, 2, storeCount(t, w, "SELECT toUInt64(sum(requests)) FROM {db}.usage_history WHERE merchant_id = {m} AND meter = ''"))
		require.EqualValues(t, 1, storeCount(t, w, "SELECT toUInt64(sum(failed_requests)) FROM {db}.usage_history WHERE merchant_id = {m}"))
		require.EqualValues(t, 70, storeCount(t, w, "SELECT toUInt64(sum(forgiven)) FROM {db}.usage_history WHERE merchant_id = {m}"), "no grace window: forgiven")

		now := w.clock.Now()
		report, err := w.alpha.GetUsage(t.Context(), customer, billing.GetUsageParams{Currency: "USD", From: now.Add(-time.Hour), To: now.Add(time.Hour)})
		require.NoError(t, err)
		require.Equal(t, compute(2, 200), report.Rows, "both items count; the failure charged nothing")
		units := map[string]int64{}
		for _, row := range metricTable(t, w.alpha, billing.QueryMetricsParams{Measures: []string{"usage_units"}, By: []string{"outcome"},
			Range: &billing.MetricsRange{From: now.AddDate(0, 0, -1).Format(time.DateOnly), To: now.Format(time.DateOnly)}}) {
			units[fmt.Sprint(row["outcome"])] += whole(t, row["usage_units"])
		}
		require.Equal(t, map[string]int64{"succeeded": 1, "failed": 1}, units, "metrics still split them by outcome")
	})

	t.Run("OR-J24-S83/a-late-item-whose-limit-window-ended-settles-and-counts-toward-no-window", func(t *testing.T) {
		w := newWorld(t, spendCap(billing.BudgetWindow{Key: "hourly", WindowSeconds: 3600, Limit: 1_000_000, Currency: "USD"}))
		usageMeter(t, w.alpha, 100)
		customer, _ := fundedCustomer(t, w)
		// A gate's item admitted two days ago, its first sync lost, sent again
		// now: its hourly spend window is long over.
		admitted := w.clock.Now().Add(-48 * time.Hour)
		late := usageInput(customer, "late", 2)
		late.OccurredAt, late.AdmittedAt = admitted, ptr(admitted)
		settleUsage(t, w.alpha, leaseMintedAt(admitted), numbered(1, late)...)
		windows := func(where string) uint64 {
			return storeCount(t, w, "SELECT count() FROM {db}.limit_usage WHERE merchant_id = {m} AND "+where)
		}
		require.Zero(t, windows("1"), "the ended window takes nothing and nothing opens")
		var rows int
		require.NoError(t, w.pool.QueryRow(t.Context(), "SELECT count(*) FROM "+fmt.Sprintf("%q", w.schema)+".limit_windows WHERE customer_id = $1", customer.UUID()).Scan(&rows))
		require.Zero(t, rows)

		// Usage admitted now counts toward the window it is in.
		settleUsage(t, w.alpha, newLease(), numbered(1, usageInput(customer, "now", 1))...)
		require.EqualValues(t, 1, windows("1"))
		require.Zero(t, windows(fmt.Sprintf("window_ends_at <= toDateTime64(%d, 6, 'UTC')", w.clock.Now().Unix())))
		balance(t, w.alpha, customer, 700, 300, 1000)
	})
}
