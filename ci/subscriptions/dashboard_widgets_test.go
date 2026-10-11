//go:build e2e && integration

package subscriptions_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
)

// metricsTable is a metrics answer by column name: its rows and, when
// compared, the previous period's.
type metricsTable struct {
	rows, compare []map[string]any
}

// metricsQuery runs one metrics query as staff.
func (w *world) metricsQuery(query any) metricsTable {
	w.t.Helper()
	status, res := w.staffJSON(http.MethodPost, "/v1/metrics/query", query)
	require.Equal(w.t, http.StatusOK, status, "%v: %v", query, res)
	var names []string
	for _, col := range res["columns"].([]any) {
		names = append(names, col.(map[string]any)["name"].(string))
	}
	byName := func(raw any) []map[string]any {
		list, _ := raw.([]any)
		out := make([]map[string]any, 0, len(list))
		for _, r := range list {
			row := map[string]any{}
			for i, cell := range r.([]any) {
				row[names[i]] = cell
			}
			out = append(out, row)
		}
		return out
	}
	return metricsTable{rows: byName(res["rows"]), compare: byName(res["compare_rows"])}
}

// byDay sums measure per bucket date of a time series.
func byDay(t *testing.T, rows []map[string]any, measure string) map[string]int64 {
	t.Helper()
	out := map[string]int64{}
	for _, r := range rows {
		out[r["time"].(string)[:10]] += number(t, r[measure])
	}
	return out
}

// toUTCDay moves the world's clock to an hour past the next UTC midnight, so a
// test's days are whole calendar days.
func (w *world) toUTCDay(days int) time.Time {
	w.t.Helper()
	now := w.clock.Now().UTC()
	at := time.Date(now.Year(), now.Month(), now.Day()+days, 1, 0, 0, 0, time.UTC)
	w.advanceHealthyTo(at)
	return w.clock.Now().UTC()
}

// The default dashboard's widgets each answer from real subscriptions and
// payments, bucketed where they happened: two members join yesterday, one
// today, one cancels today and one purchase declines today. The today row
// keeps its own range and compares with yesterday; every other widget runs
// over the range a viewer picks.
func TestDefaultDashboardWidgetsQuery(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	yesterday := w.toUTCDay(1)
	price := w.membership("content:dashboard", 9_990_000)
	var subs []billing.SubscriptionID
	for range 2 {
		c := w.newCustomer()
		subs = append(subs, c.subscribe(embedded, "stripe", price.ID.String(), "content:dashboard", c.saveCard("stripe", visa)))
	}
	w.advanceHealthyTo(w.clock.Now().Add(24 * time.Hour))
	today := w.clock.Now().UTC()
	c := w.newCustomer()
	subs = append(subs, c.subscribe(embedded, "stripe", price.ID.String(), "content:dashboard", c.saveCard("stripe", visa)))
	canceled, err := w.client[embedded].CancelSubscription(t.Context(), subs[0], billing.CancelSubscriptionParams{Reason: "moving on"})
	require.NoError(t, err)
	require.NotNil(t, canceled.CancelType)
	declined := hostedPay{w: w, c: w.newCustomer(), tp: embedded, price: w.membership("content:declined", 4_990_000).ID.String()}
	paid, err := declined.pay("pay-declined", w.nmi.Tokenize(card{Brand: "visa", Last4: "0002", Decline: "202"}))
	declinedPay(t, paid, err)
	w.settle()

	status, res := w.staffJSON(http.MethodGet, "/v1/metrics/dashboard", nil)
	require.Equal(t, http.StatusOK, status, "%v", res)
	require.Equal(t, true, res["is_default"])
	picked := map[string]string{"from": yesterday.Format(time.DateOnly), "to": today.Format(time.DateOnly)}
	day0, day1 := yesterday.Format(time.DateOnly), today.Format(time.DateOnly)
	answers := map[string]metricsTable{}
	for _, raw := range res["widgets"].([]any) {
		widget := raw.(map[string]any)
		query := widget["query"].(map[string]any)
		if widget["fixed_range"] != true {
			query["range"] = picked
		}
		answer := w.metricsQuery(query)
		require.NotEmpty(t, answer.rows, "widget %s answers rows", widget["id"])
		answers[widget["id"].(string)] = answer
	}

	single := func(id, measure string) (int64, int64) {
		t.Helper()
		a := answers[id]
		require.Len(t, a.rows, 1, id)
		require.Len(t, a.compare, 1, id)
		return number(t, a.rows[0][measure]), number(t, a.compare[0][measure])
	}
	for _, tc := range []struct {
		id, measure      string
		today, yesterday int64
	}{
		{"today-new", "new_subscriptions", 1, 2},
		{"today-renewals", "payment_count", 0, 0},
		{"today-cancellations", "cancellations", 1, 0},
		{"today-failed-purchases", "failed_attempts", 1, 0},
		{"today-net", "net_new_subscriptions", 0, 2},
	} {
		now, before := single(tc.id, tc.measure)
		require.Equal(t, [2]int64{tc.today, tc.yesterday}, [2]int64{now, before}, tc.id)
	}

	page, err := w.client[embedded].ListSubscriptions(t.Context(), billing.SubscriptionListParams{Status: billing.SubscriptionActive})
	require.NoError(t, err)
	activeNow, _ := single("active-subscribers", "subscriptions")
	require.Equal(t, int64(len(page.Items)), activeNow)
	netNew, _ := single("net-new", "net_new_subscriptions")
	require.Equal(t, int64(2), netNew, "three joined, one canceled")

	flows := answers["new-vs-canceled"].rows
	require.Equal(t, map[string]int64{day0: 2, day1: 1}, byDay(t, flows, "new_subscriptions"))
	require.Equal(t, map[string]int64{day0: 0, day1: 1}, byDay(t, flows, "cancellations"))

	byType := map[string]int64{}
	for _, r := range answers["cancellations-by-type"].rows {
		byType[r["cancel_type"].(string)] += number(t, r["cancellations"])
	}
	require.Equal(t, map[string]int64{*canceled.CancelType: 1}, byType)

	byRail := map[string]int64{}
	for _, r := range answers["revenue-by-rail"].rows {
		byRail[r["rail"].(string)] += number(t, r["net_revenue"])
	}
	require.Equal(t, map[string]int64{"stripe": 3 * 9_990_000}, byRail)

	byStream := map[string]int64{}
	for _, r := range answers["revenue-by-stream"].rows {
		byStream[r["stream"].(string)] += number(t, r["net_revenue"])
	}
	require.Equal(t, int64(3*9_990_000), byStream["subscription"])

	byKind := map[string]int64{}
	for _, r := range answers["new-subscriptions"].rows {
		byKind[r["subscriber_type"].(string)] += number(t, r["new_subscriptions"])
	}
	require.Equal(t, int64(3), byKind["first_time"])
}

// The Reports page's daily and monthly tables bucket by UTC calendar day and
// month: two members join on the 5th, one on the 7th; a month later the two
// renew and the third cancels.
func TestMetricsReportGrains(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	now := w.clock.Now().UTC()
	first := time.Date(now.Year(), now.Month()+1, 5, 1, 0, 0, 0, time.UTC)
	w.advance(first.Sub(now))
	price := w.membership("content:reports", 9_990_000)
	var renewing []billing.SubscriptionID
	for range 2 {
		c := w.newCustomer()
		renewing = append(renewing, c.subscribe(embedded, "stripe", price.ID.String(), "content:reports", c.saveCard("stripe", visa)))
	}
	w.advance(48 * time.Hour)
	c := w.newCustomer()
	leaving := c.subscribe(embedded, "stripe", price.ID.String(), "content:reports", c.saveCard("stripe", visa))

	end := w.subscription(embedded, renewing[0]).CurrentPeriodEndsAt
	w.advanceHealthyTo(end.Add(time.Second))
	w.runRenewals()
	for _, id := range renewing {
		require.True(t, w.subscription(embedded, id).CurrentPeriodEndsAt.After(*end), "renewed")
	}
	_, err := w.client[embedded].CancelSubscription(t.Context(), leaving, billing.CancelSubscriptionParams{Reason: "done"})
	require.NoError(t, err)
	canceledOn := w.clock.Now().UTC().Format(time.DateOnly)
	w.settle()

	monthOne := time.Date(first.Year(), first.Month(), 1, 0, 0, 0, 0, time.UTC)
	monthTwo := monthOne.AddDate(0, 1, 0)
	span := map[string]string{"from": monthOne.Format(time.DateOnly), "to": monthTwo.AddDate(0, 1, -1).Format(time.DateOnly)}

	daily := w.metricsQuery(map[string]any{
		"measures": []string{"new_subscriptions", "cancellations", "net_new_subscriptions"},
		"by":       []string{"time"}, "grain": "day", "range": span,
	}).rows
	require.Len(t, daily, int(monthOne.AddDate(0, 2, 0).Sub(monthOne).Hours()/24), "every day of both months, zero-filled")
	joined := byDay(t, daily, "new_subscriptions")
	for day, n := range joined {
		switch day {
		case first.Format(time.DateOnly):
			require.Equal(t, int64(2), n, day)
		case first.AddDate(0, 0, 2).Format(time.DateOnly):
			require.Equal(t, int64(1), n, day)
		default:
			require.Zero(t, n, day)
		}
	}
	for day, n := range byDay(t, daily, "cancellations") {
		if day == canceledOn {
			require.Equal(t, int64(1), n, day)
		} else {
			require.Zero(t, n, day)
		}
	}
	require.Equal(t, int64(2), byDay(t, daily, "net_new_subscriptions")[first.Format(time.DateOnly)])

	monthly := w.metricsQuery(map[string]any{
		"measures": []string{"new_subscriptions", "cancellations", "churn_rate"},
		"by":       []string{"time"}, "grain": "month", "range": span,
	}).rows
	require.Len(t, monthly, 2)
	m1, m2 := monthOne.Format(time.DateOnly), monthTwo.Format(time.DateOnly)
	require.Equal(t, map[string]int64{m1: 3, m2: 0}, byDay(t, monthly, "new_subscriptions"))
	require.Equal(t, map[string]int64{m1: 0, m2: 1}, byDay(t, monthly, "cancellations"))
	require.InDelta(t, 1.0/3, monthly[1]["churn_rate"], 1e-9, "one of the three at the month's start")

	renewals := w.metricsQuery(map[string]any{
		"measures": []string{"payment_count"}, "by": []string{"time"}, "grain": "month", "range": span,
		"filters": map[string][]string{"attempt_kind": {"renewal"}},
	}).rows
	require.Equal(t, map[string]int64{m1: 0, m2: 2}, byDay(t, renewals, "payment_count"))
}
