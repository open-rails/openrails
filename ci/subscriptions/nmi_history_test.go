//go:build e2e && integration

package subscriptions_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/riverqueue/river"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/internal/nmimock"
)

type nmiHistoryPass struct{}

func (nmiHistoryPass) Kind() string { return "openrails.nmi_history" }

// readNMIHistory runs one pass of the NMI history job (#1120).
func (w *world) readNMIHistory() {
	w.t.Helper()
	res, err := w.jobs.Insert(w.t.Context(), nmiHistoryPass{}, &river.InsertOpts{Queue: openrails.QueueBilling})
	require.NoError(w.t, err)
	w.waitJob(res.Job.ID)
}

// historyRow is one stored month's outcome.
type historyRow struct{ Month, Kind, Category, Reason string }

func historyKey(at time.Time, kind, category, reason string) historyRow {
	return historyRow{at.UTC().Format("2006-01"), kind, category, reason}
}

// nmiHistory is the stored months and when the PSP's history was last read.
func (w *world) nmiHistory() (map[historyRow]int64, time.Time) {
	w.t.Helper()
	rows, err := w.pool.Query(w.t.Context(), w.q(`SELECT month_at, kind, category, reason, authorizations FROM billing.nmi_history_months`))
	require.NoError(w.t, err)
	defer rows.Close()
	out := map[historyRow]int64{}
	for rows.Next() {
		var month time.Time
		var kind, category, reason string
		var n int64
		require.NoError(w.t, rows.Scan(&month, &kind, &category, &reason, &n))
		out[historyKey(month, kind, category, reason)] = n
	}
	require.NoError(w.t, rows.Err())
	var readAt time.Time
	require.NoError(w.t, w.pool.QueryRow(w.t.Context(), w.q(`SELECT read_at FROM billing.nmi_history_reads`)).Scan(&readAt))
	return out, readAt
}

// historyQueries records the NMI history read's queries: transaction report
// windows that start on a month's first instant, as no other reader's do.
func (w *world) historyQueries() func() []url.Values {
	var mu sync.Mutex
	var seen []url.Values
	w.nmi.Intercept(func(r *http.Request) bool {
		if r.Body == nil {
			return false
		}
		raw, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(raw))
		form, err := url.ParseQuery(string(raw))
		if err == nil && form.Get("report_type") == "transaction" && strings.HasSuffix(form.Get("start_date"), "01000000") {
			mu.Lock()
			seen = append(seen, form)
			mu.Unlock()
		}
		return false // observed, never intercepted
	}, nil)
	return func() []url.Values {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(seen)
	}
}

func monthOf(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
}

// NMI's own history is kept monthly per PSP: the first read backfills 25
// months, a daily read replaces the months it covers, and a failed read keeps
// what was stored. The metrics split it like the decline report, which reads
// the same numbers (#1120).
func TestNMIDeclineHistory(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.waive("recorded", "NMI's history before OpenRails: sales an earlier billing system made")
	start := w.clock.Now()
	month := func(back int) time.Time { return monthOf(start).AddDate(0, -back, 0) }
	vault := w.nmi.AddVault(visa)
	sale := func(at time.Time, declined, schedule string) {
		w.nmi.AddSale(nmimock.Sale{Vault: vault, Amount: "9.99", Declined: declined, ScheduleID: schedule, At: at})
	}
	sale(month(26).Add(day), "", "") // older than 25 months: never read
	sale(month(3).Add(day), "", "")
	sale(month(3).Add(2*day), "", "")
	sale(month(3).Add(3*day), "202", "")
	sale(month(2).Add(day), "", "legacy-schedule")
	sale(month(2).Add(2*day), "201", "legacy-schedule")
	w.newCustomer().saveCard("nmi", visa) // this month's verification, which OpenRails records too
	w.advance(time.Minute)

	queries := w.historyQueries()
	w.readNMIHistory()
	backfill := queries()
	require.Len(t, backfill, 25, "one query per month backfilled")
	for i, q := range backfill {
		from, err := time.Parse("20060102150405", q.Get("start_date"))
		require.NoError(t, err)
		require.Equal(t, monthOf(w.clock.Now()).AddDate(0, i-24, 0), from, "oldest first")
		require.Equal(t, []string{"1000", ""}, []string{q.Get("result_limit"), q.Get("page_number")}, "first Query API page is the omitted default zero")
	}
	want := map[historyRow]int64{
		historyKey(month(3), "one_off_sale", "approved", ""):                      2,
		historyKey(month(3), "one_off_sale", "issuer_soft", "insufficient_funds"): 1,
		historyKey(month(2), "scheduled_rebill", "approved", ""):                  1,
		historyKey(month(2), "scheduled_rebill", "issuer_soft", "do_not_honor"):   1,
		historyKey(start, "verification", "approved", ""):                         1,
	}
	stored, readAt := w.nmiHistory()
	require.Equal(t, want, stored)
	require.True(t, readAt.Equal(w.clock.Now()), "%s", readAt)

	// The measures, split like the report: by month, kind and PSP.
	rates := map[string][3]float64{}
	for _, r := range w.metricRows(month(24), []string{"nmi_history_authorizations", "nmi_history_refused", "nmi_history_refusal_rate"}, []string{"time", "nmi_kind", "psp"}, nil) {
		if n := decimal(t, r["nmi_history_authorizations"]); n > 0 {
			key := r["time"].(string)[:7] + " " + r["nmi_kind"].(string) + " " + r["psp"].(string)
			rates[key] = [3]float64{n, decimal(t, r["nmi_history_refused"]), decimal(t, r["nmi_history_refusal_rate"])}
		}
	}
	psp := w.psp["nmi"].String()
	require.Equal(t, map[string][3]float64{
		month(3).Format("2006-01") + " one_off_sale " + psp:     {3, 1, 1.0 / 3},
		month(2).Format("2006-01") + " scheduled_rebill " + psp: {2, 1, 0.5},
		start.UTC().Format("2006-01") + " verification " + psp:  {1, 0, 0},
	}, rates)
	reasons := map[string]float64{}
	for _, r := range w.metricRows(month(24), []string{"nmi_history_refused"}, []string{"nmi_kind", "category", "reason"}, map[string][]string{"category": {"issuer_soft"}}) {
		reasons[r["nmi_kind"].(string)+" "+r["reason"].(string)] = decimal(t, r["nmi_history_refused"])
	}
	require.Equal(t, map[string]float64{"one_off_sale insufficient_funds": 1, "scheduled_rebill do_not_honor": 1}, reasons)

	// The CLI reads through the same function and reports the same months.
	var report struct {
		Months []struct {
			Month, Kind       string
			Attempts, Refused int64
		}
	}
	require.NoError(t, json.Unmarshal([]byte(w.declineReport(month(24), "json")), &report))
	fromCLI, fromTable := map[[2]string][2]int64{}, map[[2]string][2]int64{}
	for _, m := range report.Months {
		fromCLI[[2]string{m.Month, m.Kind}] = [2]int64{m.Attempts, m.Refused}
	}
	for row, n := range stored {
		v := fromTable[[2]string{row.Month, row.Kind}]
		v[0] += n
		if row.Category != "approved" {
			v[1] += n
		}
		fromTable[[2]string{row.Month, row.Kind}] = v
	}
	require.Equal(t, fromTable, fromCLI)

	// Read once a day.
	w.advance(time.Hour)
	w.readNMIHistory()
	_, notAgain := w.nmiHistory()
	require.True(t, notAgain.Equal(readAt), "not due again within the day")

	// A day later the recent months are read again: a rerun changes nothing.
	w.advance(day)
	before := len(queries())
	w.readNMIHistory()
	require.LessOrEqual(t, len(queries())-before, 3, "only the recent months")
	again, readAgain := w.nmiHistory()
	require.Equal(t, want, again)
	require.True(t, readAgain.After(readAt))

	// A new charge replaces its month's count rather than adding a row.
	late := w.clock.Now().Add(-time.Hour)
	sale(late, "", "")
	w.advance(day)
	w.readNMIHistory()
	want[historyKey(late, "one_off_sale", "approved", "")]++
	stored, readAt = w.nmiHistory()
	require.Equal(t, want, stored)

	// A failed read keeps the stored months and the last read; the next pass
	// tries again.
	w.advance(day)
	w.nmi.QueryUnavailable(true)
	later := w.clock.Now().Add(-time.Hour)
	sale(later, "202", "")
	w.readNMIHistory()
	kept, keptAt := w.nmiHistory()
	require.Equal(t, want, kept)
	require.True(t, keptAt.Equal(readAt))
	w.nmi.QueryUnavailable(false)
	w.readNMIHistory()
	want[historyKey(later, "one_off_sale", "issuer_soft", "insufficient_funds")]++
	stored, _ = w.nmiHistory()
	require.Equal(t, want, stored)
}
