//go:build integration

package critical_test

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db/gen"
	postgresmigrations "github.com/open-rails/openrails/internal/migrate/postgres"
	"github.com/open-rails/openrails/internal/modules/dashboard"
	"github.com/open-rails/openrails/internal/modules/metrics"
	"github.com/open-rails/openrails/internal/modules/money/credits"
	"github.com/open-rails/openrails/internal/sqlschema"
)

// Revenue on both bases from the same postings: recognized_revenue and
// balance read the general ledger's rollups, cash the payments and the fees
// the books read, over a known history; each equals the books and the
// payments it reads.
func TestRevenueBases(t *testing.T) {
	t.Run("OR-J53-S10/a-subscription-a-purchase-a-refund-and-a-won-dispute-equal-the-books-and-the-payments", subscriptionsOnBothBases)
	t.Run("OR-J53-S11/credits-spent-and-lapsed-and-usage-owed-and-collected-equal-the-books", creditsAndUsageOnBothBases)
	t.Run("OR-J53-S12/the-accounting-basis-leads-the-overview-and-a-balance-names-its-account", accountingBasis)
}

// Every books measure reads the rollups and the live schedules, never the
// transfers, payments or schedules whole, and a past value stays what it
// was read as.
func TestBooksMeasures(t *testing.T) {
	t.Run("OR-J40-S50/every-books-measure-answers-from-rollups-and-a-past-value-stays", booksMeasuresFromRollups)
}

// The membership (a day, $10.00), a monthly plan ($10.00) and a lifetime
// purchase ($5.00), all on 15 June 2026 for a merchant keeping its books in
// New York; the membership charged back and the dispute won, $2.00 of the
// purchase refunded. June's revenue is the membership whole, the plan's 378
// of 720 hours to New York's 1 July and the purchase less its refund.
func subscriptionsOnBothBases(t *testing.T) {
	york, err := time.LoadLocation("America/New_York")
	require.NoError(t, err)
	start := time.Date(2026, time.June, 15, 10, 0, 0, 0, time.UTC)
	m := newDisputedMembership(t, atClock(start), inZone("America/New_York"))
	a, w := m.a, m.a.world
	monthly := buy(t, a, m.p, "bases-monthly", []billing.InvoiceLineParams{{PriceID: calendarPrice(t, a, "bases-monthly", 10_000_000, 1)}}, 10_000_000)
	lifetime := buy(t, a, m.p, "bases-lifetime", []billing.InvoiceLineParams{{PriceID: oneOffPrice(t, a, "bases-lifetime", 5_000_000, nil)}}, 5_000_000)
	fleet := quietFleet(t, w)
	require.False(t, runPass(t, fleet))

	// Six hours in the membership is charged back; three days on the
	// dispute is won; five days on $2.00 of the purchase is refunded.
	a.clock.Advance(6 * time.Hour)
	m.notice(t, "evt_bases_dispute", "charge.dispute.created", m.p.core.ChargeDispute("dp_bases", m.charge, "needs_response", "fraudulent"))
	require.False(t, runPass(t, fleet))
	a.clock.Advance(3 * 24 * time.Hour)
	m.notice(t, "evt_bases_won", "charge.dispute.closed", m.p.core.ChargeDispute("dp_bases", m.charge, "won", "fraudulent"))
	require.False(t, runPass(t, fleet))
	a.clock.Advance(2 * 24 * time.Hour)
	refund, err := a.alpha.RefundPayment(t.Context(), "bases-refund", *lifetime.Payment.PaymentID, billing.RefundPaymentParams{Amount: 2_000_000, Reason: "Partial refund"})
	require.NoError(t, err)
	require.Equal(t, billing.PaymentSucceeded, refund.Status)
	require.False(t, runPass(t, fleet))

	// The plan's deferred revenue runs off at New York's 1 July (378 of its
	// 720 hours, dated on 30 June there) and at its end.
	month := func(y int, mo time.Month) string {
		return time.Date(y, mo, 1, 0, 0, 0, 0, york).UTC().Format(time.RFC3339)
	}
	ahead := billing.QueryMetricsParams{Measures: []string{"scheduled_revenue"}, By: []string{"time", "account"}, Grain: "month",
		Range: &billing.MetricsRange{From: "2026-06-20", To: "2026-07-31"}}
	require.Equal(t, map[string]int64{month(2026, time.June) + "|subscription_revenue": 5_250_000, month(2026, time.July) + "|subscription_revenue": 4_750_000},
		keyed(t, metricTable(t, a.alpha, ahead), "scheduled_revenue", "time", "account"))

	// The pass after New York's 1 July posts what the waterfall said.
	july := time.Date(2026, time.July, 1, 0, 0, 0, 0, york)
	a.clock.Advance(july.Add(2 * time.Hour).Sub(a.clock.Now()))
	require.False(t, runPass(t, fleet))
	plan := scheduleOfLine(t, w, monthly.Lines[0].ID.UUID())
	require.EqualValues(t, 5_250_000, plan.Recognized)

	from, to := time.Date(2026, time.June, 1, 0, 0, 0, 0, york), time.Date(2026, time.August, 1, 0, 0, 0, 0, york)
	window := &billing.MetricsRange{From: "2026-06-01", To: "2026-07-31"}
	revenue := keyed(t, metricTable(t, a.alpha, billing.QueryMetricsParams{Measures: []string{"recognized_revenue"}, By: []string{"time", "account"}, Grain: "month", Range: window}),
		"recognized_revenue", "time", "account")
	require.Equal(t, glFlows(t, w, "month", york, from, to, incomeAccounts()...), revenue, "recognised revenue is the books' income-statement accounts")
	june := month(2026, time.June)
	require.Equal(t, map[string]int64{
		june + "|subscription_revenue": 15_250_000, june + "|one_time_revenue": 5_000_000, june + "|refunds": -2_000_000,
	}, revenue, "the membership whole, the plan's June in New York, the purchase less its refund; the chargeback's contra came back")
	net := only(t, metricTable(t, a.alpha, billing.QueryMetricsParams{Measures: []string{"recognized_revenue"}, Range: window, Filters: map[string][]string{"currency": {"USD"}}}))
	require.EqualValues(t, 18_250_000, whole(t, net["recognized_revenue"]), "net revenue")

	// Cash: what came in and went out, the dispute's money back, and the
	// fees Stripe took, as the payments and the books hold them.
	cash := keyed(t, metricTable(t, a.alpha, billing.QueryMetricsParams{Measures: []string{"cash"}, By: []string{"time", "cash_kind"}, Grain: "month", Range: window}),
		"cash", "time", "cash_kind")
	require.Equal(t, cashOf(t, w, york, from, to), cash, "cash is the payments and the fees")
	fees := cash[june+"|fee"]
	require.Negative(t, fees, "Stripe's fees leave the cash")
	require.Equal(t, map[string]int64{june + "|payment": 25_000_000, june + "|chargeback": -10_000_000, june + "|dispute_won": 10_000_000,
		june + "|refund": -2_000_000, june + "|fee": fees}, cash)
	require.Equal(t, map[string]int64{"subscription_revenue": 20_000_000, "one_time_revenue": 3_000_000, "": fees},
		keyed(t, metricTable(t, a.alpha, billing.QueryMetricsParams{Measures: []string{"cash"}, By: []string{"account"}, Range: window}), "cash", "account"),
		"both bases split by the revenue account the money funds")

	// Deferred revenue at each month's start, and now: the plan's July.
	balances := keyed(t, metricTable(t, a.alpha, billing.QueryMetricsParams{Measures: []string{"balance"}, By: []string{"time", "account"}, Grain: "month",
		Range: &billing.MetricsRange{From: "2026-06-01", To: "2026-08-31"}, Filters: map[string][]string{"account": {"deferred_revenue", "unapplied_cash"}}}), "balance", "time", "account")
	for _, edge := range []time.Time{from, july, to} {
		for _, account := range []string{"deferred_revenue", "unapplied_cash"} {
			require.Equal(t, glBalanceAt(t, w, "general", account, edge), balances[edge.UTC().Format(time.RFC3339)+"|"+account], "%s at %s", account, edge)
		}
	}
	require.EqualValues(t, 4_750_000, balances[july.UTC().Format(time.RFC3339)+"|deferred_revenue"])
	now := only(t, metricTable(t, a.alpha, billing.QueryMetricsParams{Measures: []string{"balance"}, Range: &billing.MetricsRange{Last: "1d"},
		Filters: map[string][]string{"account": {"deferred_revenue"}, "currency": {"USD"}}}))
	require.EqualValues(t, 4_750_000, whole(t, now["balance"]))
	require.Equal(t, map[string]int64{month(2026, time.July) + "|subscription_revenue": 4_750_000},
		keyed(t, metricTable(t, a.alpha, ahead), "scheduled_revenue", "time", "account"), "the waterfall keeps what is still to post")
	booksAudited(t, w)
}

// A pack of 11 credits for $10.00 spent one credit and lapsed with ten left;
// usage of $30.00 beyond a balance owed and remitted a day later.
func creditsAndUsageOnBothBases(t *testing.T) {
	t.Run("credits", func(t *testing.T) {
		s := newCreditShop(t, func(w *world) { w.clock.Advance(-50 * time.Hour) })
		a, w := s.a, s.a.world
		bought := a.clock.Now()
		_, lot := s.buy(t, s.pack(t, "bases-pack", 11_000_000, 10_000_000, 1), 10_000_000, "bases-pack")
		s.spend(t, 1_000_000, "bases-one")
		// A lot's money is dated when the lot and its spends were stored,
		// on the database's clock.
		heldAt := time.Now().Add(time.Hour)
		held := billing.QueryMetricsParams{Measures: []string{"balance"}, By: []string{"account"},
			Range:   &billing.MetricsRange{From: bought.Add(-time.Hour).Format(time.RFC3339), To: heldAt.Format(time.RFC3339)},
			Filters: map[string][]string{"account": {"deferred_revenue_credit"}}}
		require.EqualValues(t, 9_090_910, glBalanceAt(t, w, "general", "deferred_revenue_credit", heldAt))
		require.Equal(t, map[string]int64{"deferred_revenue_credit": 9_090_910}, keyed(t, metricTable(t, a.alpha, held), "balance", "account"),
			"credit held, in money: ten credits at the pack's rate")

		a.clock.Advance(49 * time.Hour)
		expireLapsed(t, s)
		end := time.Now().Add(2 * time.Hour)
		span := &billing.MetricsRange{From: bought.Add(-time.Hour).Format(time.RFC3339), To: end.Format(time.RFC3339)}
		revenue := keyed(t, metricTable(t, a.alpha, billing.QueryMetricsParams{Measures: []string{"recognized_revenue"}, By: []string{"account", "transfer_type"}, Range: span}),
			"recognized_revenue", "account", "transfer_type")
		require.Equal(t, map[string]int64{"usage_revenue|credit_released": 909_090, "breakage_revenue|credit_breakage": 9_090_910}, revenue,
			"the credit spent is usage revenue, the ten lapsed breakage")
		require.Equal(t, map[string]int64{"usage_revenue": 909_090, "breakage_revenue": 9_090_910}, glFlows(t, w, "", time.UTC, bought.Add(-time.Hour), end, incomeAccounts()...))
		require.Equal(t, map[string]int64{"USD": 10_000_000}, split(t, metricTable(t, a.alpha, billing.QueryMetricsParams{Measures: []string{"credits_sold"}, Range: span}), "currency", "credits_sold"),
			"credits sold is the pack's price")
		deferred, _ := lotMoney(t, w, lot)
		require.EqualValues(t, 10_000_000, deferred)
		require.Equal(t, map[string]int64{"usage_revenue": 10_000_000}, keyed(t, metricTable(t, a.alpha, billing.QueryMetricsParams{Measures: []string{"cash"}, By: []string{"account"},
			Range: span, Filters: map[string][]string{"cash_kind": {"payment"}}}), "cash", "account"), "a pack's money funds usage")

		// Nothing is held once it lapsed, in either book.
		at := &billing.MetricsRange{From: bought.Format(time.RFC3339), To: end.Format(time.RFC3339)}
		now := keyed(t, metricTable(t, a.alpha, billing.QueryMetricsParams{Measures: []string{"balance"}, By: []string{"account"}, Range: at,
			Filters: map[string][]string{"account": {"deferred_revenue_credit", "customer_units"}}}), "balance", "account")
		require.Empty(t, now)
		require.Zero(t, glBalanceAt(t, w, "general", "deferred_revenue_credit", end))
		require.Zero(t, glBalanceAt(t, w, "units", "customer_units", end))
		booksAudited(t, w)
	})

	t.Run("usage", func(t *testing.T) {
		a, customer, invoice := openUsageInvoice(t)
		w := a.world
		owed := a.clock.Now()
		require.Equal(t, map[string]int64{"usage_receivable": 30_000_000},
			keyed(t, metricTable(t, a.alpha, billing.QueryMetricsParams{Measures: []string{"balance"}, By: []string{"account"}, Range: &billing.MetricsRange{Last: "1d"},
				Filters: map[string][]string{"account": {"usage_receivable"}}}), "balance", "account"), "usage owed is receivable")
		owedIsReceivable(t, a, customer, 30_000_000)

		a.clock.Advance(24 * time.Hour)
		remit(t, a, invoice, 30_000_000, "bases-remit")
		span := &billing.MetricsRange{From: owed.AddDate(0, -1, 0).Format(time.RFC3339), To: a.clock.Now().Add(time.Hour).Format(time.RFC3339)}
		require.Equal(t, map[string]int64{"usage_revenue|usage_accrued": 30_000_000},
			keyed(t, metricTable(t, a.alpha, billing.QueryMetricsParams{Measures: []string{"recognized_revenue"}, By: []string{"account", "transfer_type"}, Range: span}),
				"recognized_revenue", "account", "transfer_type"), "usage owed is revenue as it accrues")
		require.Equal(t, map[string]int64{"usage_revenue": 30_000_000},
			keyed(t, metricTable(t, a.alpha, billing.QueryMetricsParams{Measures: []string{"cash"}, By: []string{"account"}, Range: span}), "cash", "account"),
			"and cash once collected")
		days := keyed(t, metricTable(t, a.alpha, billing.QueryMetricsParams{Measures: []string{"balance"}, By: []string{"time"}, Grain: "day",
			Range: &billing.MetricsRange{Last: "3d"}, Filters: map[string][]string{"account": {"usage_receivable"}, "currency": {"USD"}}}), "balance", "time")
		for at, value := range days {
			edge, err := time.Parse(time.RFC3339, at)
			require.NoError(t, err)
			require.Equal(t, glBalanceAt(t, w, "general", "usage_receivable", edge), value, at)
		}
		require.Contains(t, days, owed.Truncate(24*time.Hour).Add(24*time.Hour).UTC().Format(time.RFC3339), "owed the day after it accrued")
		require.Empty(t, keyed(t, metricTable(t, a.alpha, billing.QueryMetricsParams{Measures: []string{"balance"}, By: []string{"account"}, Range: &billing.MetricsRange{Last: "1d"},
			Filters: map[string][]string{"account": {"usage_receivable"}}}), "balance", "account"), "collected, nothing is owed")
	})
}

// A merchant keeping its books on the cash basis leads with cash collected,
// one on the accrual basis (the default) with recognised revenue; both show
// both. A balance read across accounts is refused.
func accountingBasis(t *testing.T) {
	a, p := newPaymentWorld(t, func(w *world) {
		m := cfgMerchant(w.cfg, "alpha")
		m.Accounting = &billing.AccountingSettings{Basis: billing.AccountingCash}
		setCfgMerchant(&w.cfg, "alpha", m)
	})
	w := a.world
	sellOnce(t, a, p, "bases-basis", "USD", 10_000_000)

	leads := func(board *billing.Dashboard) (string, []string) {
		var tile string
		var chart []string
		for _, w := range board.Widgets {
			if w.ID == "kpi-cash" || w.ID == "kpi-revenue" {
				require.Empty(t, tile, "one revenue tile")
				tile = w.Query.Measures[0]
			}
			if w.ID == "revenue-vs-cash" {
				chart = w.Query.Measures
			}
		}
		return tile, chart
	}
	cash, err := a.alpha.GetDashboard(t.Context())
	require.NoError(t, err)
	tile, chart := leads(cash)
	require.Equal(t, "cash", tile)
	require.Equal(t, []string{"cash", "recognized_revenue"}, chart)
	require.Equal(t, len(dashboard.DefaultWidgets(*cash.Profile, billing.AccountingCash)), len(cash.Widgets))
	accrual, err := a.beta.GetDashboard(t.Context())
	require.NoError(t, err)
	tile, _ = leads(accrual)
	require.Equal(t, "recognized_revenue", tile, "accrual is the default")

	// Both bases answer on either merchant.
	both := metricTable(t, a.alpha, billing.QueryMetricsParams{Measures: []string{"recognized_revenue", "cash"}, Range: &billing.MetricsRange{Last: "1d"},
		Filters: map[string][]string{"currency": {"USD"}}})
	require.EqualValues(t, 10_000_000, whole(t, only(t, both)["cash"]))
	require.EqualValues(t, 10_000_000, whole(t, only(t, both)["recognized_revenue"]), "a consumable is earned when paid")

	_, err = a.alpha.QueryMetrics(t.Context(), billing.QueryMetricsParams{Measures: []string{"balance"}, Range: &billing.MetricsRange{Last: "1d"}})
	refused := metricRefusals(t, err)
	require.Equal(t, "dimension_required", refused["by"]["code"], "%v", refused)
	_, err = a.alpha.QueryMetrics(t.Context(), billing.QueryMetricsParams{Measures: []string{"balance"}, Range: &billing.MetricsRange{Last: "1d"},
		Filters: map[string][]string{"account": {"deferred_revenue", "usage_receivable"}}})
	require.Equal(t, "dimension_required", metricRefusals(t, err)["by"]["code"])
	schema, err := a.alpha.GetMetricsSchema(t.Context())
	require.NoError(t, err)
	per := map[string]string{}
	for _, m := range schema.Measures {
		per[m.Name] = m.Per
	}
	require.Equal(t, "account", per["balance"])
	require.NotContains(t, per, "outstanding_credit_liability")
	require.NotContains(t, per, "outstanding_owed")

	// The migration drops saved widgets naming the measures balance
	// replaced, stream, or credits_sold by customer; a layout it empties
	// goes, and its merchant gets the overview again.
	widget := func(id string, q billing.QueryMetricsParams) billing.DashboardWidget {
		q.Range = &billing.MetricsRange{Last: "30d"}
		return billing.DashboardWidget{ID: id, Title: id, Viz: "line", Query: q, Grid: billing.DashboardGrid{W: 6, H: 4}}
	}
	kept := widget("kept", billing.QueryMetricsParams{Measures: []string{"cash"}, By: []string{"time"}})
	stale := []billing.DashboardWidget{
		widget("owed", billing.QueryMetricsParams{Measures: []string{"outstanding_owed"}}),
		widget("liability", billing.QueryMetricsParams{Measures: []string{"outstanding_credit_liability"}}),
		widget("by-stream", billing.QueryMetricsParams{Measures: []string{"cash"}, By: []string{"stream"}}),
		widget("one-stream", billing.QueryMetricsParams{Measures: []string{"cash"}, Filters: map[string][]string{"stream": {"one_time"}}}),
		widget("sold-by-customer", billing.QueryMetricsParams{Measures: []string{"credits_sold"}, By: []string{"customer"}}),
	}
	save := func(merchant billing.MerchantID, widgets []billing.DashboardWidget) {
		layout, err := json.Marshal(widgets)
		require.NoError(t, err)
		_, err = w.pool.Exec(t.Context(), "INSERT INTO "+booksTable(w, "dashboard_configs")+" (merchant_id, layout) VALUES ($1, $2)"+
			" ON CONFLICT (merchant_id) DO UPDATE SET layout = EXCLUDED.layout", merchant.UUID(), layout)
		require.NoError(t, err)
	}
	save(a.alpha.MerchantID(), append([]billing.DashboardWidget{kept}, stale...))
	save(a.beta.MerchantID(), stale)
	migration, err := postgresmigrations.FS.ReadFile("0014_books_measures.up.sql")
	require.NoError(t, err)
	_, err = w.pool.Exec(t.Context(), mustRewrite(t, string(migration), w.schema))
	require.NoError(t, err)
	saved, err := a.alpha.GetDashboard(t.Context())
	require.NoError(t, err)
	require.False(t, saved.IsDefault)
	require.Len(t, saved.Widgets, 1)
	require.Equal(t, "kept", saved.Widgets[0].ID)
	emptied, err := a.beta.GetDashboard(t.Context())
	require.NoError(t, err)
	require.True(t, emptied.IsDefault, "a layout left empty is gone")
}

func mustRewrite(t *testing.T, sql, schema string) string {
	t.Helper()
	out, err := sqlschema.Rewrite(sql, schema)
	require.NoError(t, err)
	return out
}

// A merchant with a plan, a purchase and a pack: every books measure, by
// each of its dimensions, by time and not, in two zones, plans with no
// sequential scan and no read of the transfers; the live schedules are read
// by their index. A month and a balance read in the past read the same after
// the books move on.
func booksMeasuresFromRollups(t *testing.T) {
	start := time.Date(2026, time.June, 15, 10, 0, 0, 0, time.UTC)
	s := newCreditShop(t, atClock(start))
	a, w := s.a, s.a.world
	buy(t, a, s.p, "s50-monthly", []billing.InvoiceLineParams{{PriceID: calendarPrice(t, a, "s50-monthly", 10_000_000, 1)}}, 10_000_000)
	buy(t, a, s.p, "s50-pass", []billing.InvoiceLineParams{{PriceID: oneOffPrice(t, a, "s50-pass", 6_000_000, ptr(240))}}, 6_000_000)
	s.buy(t, s.pack(t, "s50-pack", 11_000_000, 10_000_000, 30), 10_000_000, "s50-pack")
	s.spend(t, 2_000_000, "s50-spend")
	fleet := quietFleet(t, w)
	require.False(t, runPass(t, fleet))

	york, err := time.LoadLocation("America/New_York")
	require.NoError(t, err)
	mid := a.alpha.MerchantID().UUID()
	planned := 0
	for _, zone := range []*time.Location{time.UTC, york} {
		for _, name := range []string{"recognized_revenue", "balance", "credits_sold", "cash", "scheduled_revenue"} {
			i := slices.IndexFunc(metrics.Measures, func(m metrics.Measure) bool { return m.Name == name && !m.Internal })
			require.GreaterOrEqual(t, i, 0, name)
			measure := metrics.Measures[i]
			var variants [][]string
			for _, d := range measure.Dims {
				variants = append(variants, []string{d})
			}
			variants = append(variants, nil)
			for _, by := range variants {
				for _, timed := range []bool{false, true} {
					q := billing.QueryMetricsParams{Measures: []string{name}, By: by, Range: &billing.MetricsRange{From: "2026-06-01", To: "2026-07-31"}}
					if measure.Per != "" && !slices.Contains(by, measure.Per) {
						q.Filters = map[string][]string{measure.Per: {"deferred_revenue"}}
					}
					if timed {
						q.By, q.Grain = append([]string{"time"}, q.By...), "week"
					}
					plan, refused := metrics.ValidateIn(&q, a.clock.Now(), zone)
					require.Nil(t, refused, "%+v", q)
					statements, err := metrics.Statements(plan, mid)
					require.NoError(t, err)
					if name == "scheduled_revenue" {
						require.Empty(t, statements, "the waterfall runs the live schedules in Go")
						statements = []metrics.Statement{{Family: "runoff", SQL: metrics.LiveSchedules, Args: []any{mid}}}
					}
					for _, st := range statements {
						sql, err := sqlschema.Rewrite(st.SQL, w.schema)
						require.NoError(t, err)
						explained := explainWithoutSeqScan(t, w.pool, sql, st.Args, "%s: %s", st.Family, sql)
						var plans []struct {
							Plan planNode `json:"Plan"`
						}
						require.NoError(t, json.Unmarshal([]byte(explained), &plans))
						for _, node := range plans[0].Plan.all() {
							where := fmt.Sprintf("%s by %v in %s, %s", name, q.By, zone, node.Relation)
							require.NotEqual(t, "ledger_transfers", node.Relation, "%s reads the transfers", where)
							if slices.Contains([]string{"payments", "revenue_schedules", "ledger_daily", "ledger_type_totals"}, node.Relation) {
								require.NotEqual(t, "Seq Scan", node.Type, "%s is scanned whole: %s", where, explained)
							}
						}
						planned++
					}
				}
			}
		}
	}
	require.Greater(t, planned, 40)

	// June and the balances on 25 June, read on 25 June after its pass, read
	// the same once July's boundary and a later sale have posted.
	a.clock.Advance(10*24*time.Hour + time.Hour)
	require.False(t, runPass(t, fleet))
	june := billing.QueryMetricsParams{Measures: []string{"recognized_revenue", "credits_sold", "cash"}, Range: &billing.MetricsRange{From: "2026-06-01", To: "2026-06-25"}}
	held := billing.QueryMetricsParams{Measures: []string{"balance"}, By: []string{"account"}, Range: &billing.MetricsRange{From: "2026-06-01", To: "2026-06-25"}}
	read := func(q billing.QueryMetricsParams) []map[string]any { return metricTable(t, a.alpha, q) }
	before, heldBefore := read(june), read(held)
	require.NotEmpty(t, keyed(t, heldBefore, "balance", "account"))

	a.clock.Advance(time.Date(2026, time.July, 2, 0, 0, 0, 0, time.UTC).Sub(a.clock.Now()))
	require.False(t, runPass(t, fleet))
	s.buy(t, s.pack(t, "s50-later", 11_000_000, 10_000_000, 30), 10_000_000, "s50-later")
	s.spend(t, 1_000_000, "s50-later-spend")
	require.ElementsMatch(t, before, read(june), "a past month reads what it read then")
	require.ElementsMatch(t, heldBefore, read(held), "a past balance reads what it read then")
	booksAudited(t, w)
}

// inZone keeps alpha's books in zone.
func inZone(zone string) func(*world) {
	return func(w *world) {
		m := cfgMerchant(w.cfg, "alpha")
		m.Accounting = &billing.AccountingSettings{Timezone: zone}
		setCfgMerchant(&w.cfg, "alpha", m)
	}
}

// expireLapsed expires the shop's customer's lapsed credit, as the balance
// watch does.
func expireLapsed(t *testing.T, s *creditShop) {
	t.Helper()
	w := s.a.world
	require.NoError(t, booksTx(t, w, func(ctx context.Context, q *gen.Queries) error {
		_, err := credits.New(q, w.alpha.MerchantID().UUID(), w.clock.Now).ExpireLapsed(ctx, s.customer.UUID(), "USD")
		return err
	}))
}

// keyed totals a measure's rows by the named columns, joined by "|"; a key
// with nothing is absent.
func keyed(t *testing.T, rows []map[string]any, measure string, cols ...string) map[string]int64 {
	t.Helper()
	out := map[string]int64{}
	for _, row := range rows {
		parts := make([]string, len(cols))
		for i, c := range cols {
			parts[i] = fmt.Sprint(row[c])
		}
		out[strings.Join(parts, "|")] += whole(t, row[measure])
	}
	for k, v := range out {
		if v == 0 {
			delete(out, k)
		}
	}
	return out
}

// incomeAccounts are recognized_revenue's accounts.
func incomeAccounts() []string {
	return []string{"subscription_revenue", "one_time_revenue", "usage_revenue", "breakage_revenue", "refunds", "chargebacks", "revenue_adjustments", "promotional_credit", "bad_debt"}
}

// glFlows is what the general ledger's transfers moved through accounts in
// [from, to), credits less debits, read from the transfers themselves: by
// bucket of grain in zone and account ("time|account"), or by account alone
// when grain is empty.
func glFlows(t *testing.T, w *world, grain string, zone *time.Location, from, to time.Time, accounts ...string) map[string]int64 {
	t.Helper()
	bucket := "NULL::timestamptz"
	if grain != "" {
		bucket = "date_trunc($5, t.effective_at, $6)"
	}
	args := []any{w.alpha.MerchantID().UUID(), accounts, from, to}
	if grain != "" {
		args = append(args, grain, zone.String())
	}
	rows, err := w.pool.Query(t.Context(), "SELECT "+bucket+", a.account_type, sum(CASE WHEN a.id = t.credit_account_id THEN t.amount ELSE -t.amount END)::bigint"+
		" FROM "+booksTable(w, "ledger_transfers")+" t JOIN "+booksTable(w, "ledger_accounts")+" a ON a.merchant_id = t.merchant_id AND a.id IN (t.debit_account_id, t.credit_account_id)"+
		" WHERE t.merchant_id = $1 AND t.book = 'general' AND a.account_type = ANY($2) AND t.effective_at >= $3 AND t.effective_at < $4 GROUP BY 1, 2", args...)
	require.NoError(t, err)
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var at *time.Time
		var account string
		var amount int64
		require.NoError(t, rows.Scan(&at, &account, &amount))
		key := account
		if at != nil {
			key = at.UTC().Format(time.RFC3339) + "|" + account
		}
		if amount != 0 {
			out[key] = amount
		}
	}
	require.NoError(t, rows.Err())
	return out
}

// glBalanceAt is one account type's balance before at, on its normal side,
// read from the transfers.
func glBalanceAt(t *testing.T, w *world, book, account string, at time.Time) int64 {
	t.Helper()
	var credit int64
	require.NoError(t, w.pool.QueryRow(t.Context(), "SELECT COALESCE(sum(CASE WHEN a.id = t.credit_account_id THEN t.amount ELSE -t.amount END), 0)::bigint"+
		" FROM "+booksTable(w, "ledger_transfers")+" t JOIN "+booksTable(w, "ledger_accounts")+" a ON a.merchant_id = t.merchant_id AND a.id IN (t.debit_account_id, t.credit_account_id)"+
		" WHERE t.merchant_id = $1 AND t.book = $2 AND a.account_type = $3 AND t.effective_at < $4", w.alpha.MerchantID().UUID(), book, account, at).Scan(&credit))
	if account == "usage_receivable" {
		return -credit
	}
	return credit
}

// cashOf is the settled payments and reversals by month in zone and kind,
// and the fees the books took from cash clearing.
func cashOf(t *testing.T, w *world, zone *time.Location, from, to time.Time) map[string]int64 {
	t.Helper()
	rows, err := w.pool.Query(t.Context(), "SELECT date_trunc('month', p.purchased_at, $2), CASE WHEN p.refunded_payment_id IS NULL THEN 'payment'"+
		" WHEN p.reversal_kind = 'refund' THEN 'refund' WHEN p.reversal_kind = 'chargeback' THEN 'chargeback' ELSE 'dispute_won' END, sum(p.amount)::bigint"+
		" FROM "+booksTable(w, "payments")+" p WHERE p.merchant_id = $1 AND p.status IN ('succeeded', 'refunded') AND p.purchased_at >= $3 AND p.purchased_at < $4 GROUP BY 1, 2"+
		" UNION ALL SELECT date_trunc('month', t.effective_at, $2), 'fee', sum(CASE WHEN a.id = t.debit_account_id THEN t.amount ELSE -t.amount END)::bigint"+
		" FROM "+booksTable(w, "ledger_transfers")+" t JOIN "+booksTable(w, "ledger_accounts")+" a ON a.merchant_id = t.merchant_id AND a.id IN (t.debit_account_id, t.credit_account_id)"+
		" WHERE t.merchant_id = $1 AND t.book = 'general' AND a.account_type = 'cash_clearing' AND t.transfer_type IN ('processor_fee', 'chargeback_fee')"+
		" AND t.effective_at >= $3 AND t.effective_at < $4 GROUP BY 1, 2", w.alpha.MerchantID().UUID(), zone.String(), from, to)
	require.NoError(t, err)
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var at time.Time
		var kind string
		var amount int64
		require.NoError(t, rows.Scan(&at, &kind, &amount))
		if amount != 0 {
			out[at.UTC().Format(time.RFC3339)+"|"+kind] += amount
		}
	}
	require.NoError(t, rows.Err())
	return out
}
