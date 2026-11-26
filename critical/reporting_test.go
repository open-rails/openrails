//go:build integration

package critical_test

import (
	"encoding/json"
	"fmt"
	"maps"
	"math"
	"net/http"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/modules/dashboard"
	"github.com/open-rails/openrails/internal/stripemock"
	"github.com/stretchr/testify/require"
)

// The metrics measures as one measure per funnel and the health overview
// built on them, over real purchases, refunds, disputes and renewals.
func TestReporting(t *testing.T) {
	t.Run("OR-J53-S01/funnel-measures-agree-with-the-ones-they-replace-and-an-unknown-name-is-refused", consolidatedMeasures)
	t.Run("OR-J53-S02/the-overview-validates-for-every-profile-answers-for-an-empty-merchant-and-switches-currency", healthOverview)
	t.Run("OR-J53-S03/money-never-sums-across-currencies", moneyPerCurrency)
	t.Run("OR-J53-S04/a-sale-at-2330-in-the-accounting-timezone-counts-on-that-local-day", accountingDays)
	t.Run("OR-J53-S05/a-day-chart-across-a-midnight-clock-change-has-one-row-per-local-day", midnightTransition)
}

// Santiago moved its clocks from midnight to 01:00 on 2026-09-06: that day
// starts at the transition. A sale at 01:30 counts on it, and a day chart
// across it has one row per date, each starting at its first instant.
func midnightTransition(t *testing.T) {
	before, after := time.FixedZone("-04", -4*3600), time.FixedZone("-03", -3*3600)
	sold := time.Date(2026, 9, 6, 1, 30, 0, 0, after)
	a, p := newPaymentWorld(t, func(w *world) {
		w.clock.Advance(sold.Sub(w.clock.Now()))
		m := cfgMerchant(w.cfg, "alpha")
		m.Accounting = &billing.AccountingSettings{Timezone: "America/Santiago"}
		setCfgMerchant(&w.cfg, "alpha", m)
	})
	sellOnce(t, a, p, "after-the-change", "USD", 10_000_000)
	cash := map[string]int64{}
	var days []string
	for _, row := range metricTable(t, a.alpha, billing.QueryMetricsParams{Measures: []string{"cash"}, By: []string{"time"}, Grain: "day",
		Range: &billing.MetricsRange{From: "2026-09-05", To: "2026-09-07"}, Filters: map[string][]string{"currency": {"USD"}}}) {
		day := fmt.Sprint(row["time"])
		days = append(days, day)
		cash[day] = whole(t, row["cash"])
	}
	at := func(day time.Time) string { return day.UTC().Format(time.RFC3339) }
	require.ElementsMatch(t, []string{
		at(time.Date(2026, 9, 5, 0, 0, 0, 0, before)), at(time.Date(2026, 9, 6, 1, 0, 0, 0, after)), at(time.Date(2026, 9, 7, 0, 0, 0, 0, after)),
	}, days, "one row per local day, the changed one from its transition")
	require.Equal(t, int64(10_000_000), cash[at(time.Date(2026, 9, 6, 1, 0, 0, 0, after))])

	// Measures read from rollups kept per UTC date say so.
	schema, err := a.alpha.GetMetricsSchema(t.Context())
	require.NoError(t, err)
	utc := map[string]bool{}
	for _, m := range schema.Measures {
		utc[m.Name] = m.UTCDays
	}
	require.True(t, utc["webhook_rejects"])
	require.True(t, utc["nmi_history_authorizations"])
	require.False(t, utc["cash"])
}

// A sale at 23:30 in Honolulu, ten hours behind UTC all year, falls on the
// next UTC day; a merchant keeping its books there sees it on its own day.
func accountingDays(t *testing.T) {
	honolulu, err := time.LoadLocation("Pacific/Honolulu")
	require.NoError(t, err)
	before := time.Now().In(honolulu).AddDate(0, 0, -2)
	sold := time.Date(before.Year(), before.Month(), before.Day(), 23, 30, 0, 0, honolulu)
	a, p := newPaymentWorld(t, func(w *world) {
		w.clock.Advance(sold.Sub(w.clock.Now()))
		m := cfgMerchant(w.cfg, "alpha")
		m.Accounting = &billing.AccountingSettings{Timezone: "Pacific/Honolulu"}
		setCfgMerchant(&w.cfg, "alpha", m)
	})
	sellOnce(t, a, p, "late-night", "USD", 10_000_000)
	day := time.Date(sold.Year(), sold.Month(), sold.Day(), 0, 0, 0, 0, honolulu)
	q := billing.QueryMetricsParams{Measures: []string{"cash"}, By: []string{"time"}, Grain: "day",
		Range:   &billing.MetricsRange{From: day.Format("2006-01-02"), To: day.AddDate(0, 0, 1).Format("2006-01-02")},
		Filters: map[string][]string{"currency": {"USD"}}}
	result, err := a.alpha.QueryMetrics(t.Context(), q)
	require.NoError(t, err)
	require.Equal(t, "Pacific/Honolulu", result.Timezone)
	require.True(t, result.Range.From.Equal(day), "the range starts at the local midnight, %s", result.Range.From)
	cash := map[string]int64{}
	for _, row := range metricTable(t, a.alpha, q) {
		cash[fmt.Sprint(row["time"])] = whole(t, row["cash"])
	}
	require.Equal(t, map[string]int64{
		day.UTC().Format(time.RFC3339):                  10_000_000,
		day.AddDate(0, 0, 1).UTC().Format(time.RFC3339): 0,
	}, cash, "the sale counts on its local day")

	// Usage at the same instant folds into the same local day.
	pricedMeter(t, a.alphaRemote, catalog.RatePrice{Model: catalog.ModelPerUnit, PerUnit: &catalog.PerUnitPrice{UnitAmount: 100, DivideBy: 1}})
	settleUsage(t, a.alphaRemote, newLease(), numbered(1, usageInput(billing.CustomerID(uuid.MustParse(a.alice.ID)), "late-night-work", 2))...)
	units := map[string]int64{}
	for _, row := range metricTable(t, a.alpha, billing.QueryMetricsParams{Measures: []string{"usage_units"}, By: []string{"time"}, Grain: "day", Range: q.Range}) {
		units[fmt.Sprint(row["time"])] = whole(t, row["usage_units"])
	}
	require.Equal(t, map[string]int64{
		day.UTC().Format(time.RFC3339):                  1,
		day.AddDate(0, 0, 1).UTC().Format(time.RFC3339): 0,
	}, units, "usage at 23:30 counts on its local day")
}

// Alice buys a $10 daily membership and four $100 invoices: one refunded $30,
// one charged back and the dispute won, one declined then paid by another
// card, one declined and left. Nothing renews the membership, so its renewal
// is missed.
func consolidatedMeasures(t *testing.T) {
	m := newMixedSale(t)
	a, p := m.a, m.p
	sub := buyMembership(t, a, a.alpha, p, 24)
	refunded := m.buy(t, "j53-refunded")
	refund, err := a.alpha.RefundPayment(t.Context(), "j53-refund", refunded, billing.RefundPaymentParams{Amount: 30_000_000, Reason: "Part back"})
	require.NoError(t, err)
	require.Equal(t, billing.PaymentSucceeded, refund.Status)
	disputed := m.buy(t, "j53-disputed")
	charge, intent := m.charge(t, disputed)
	dispute := stripemock.Object{"id": "dp_j53", "object": "dispute", "amount": 10_000, "currency": "usd", "charge": charge, "payment_intent": intent, "status": "needs_response", "reason": "fraudulent"}
	m.notice(t, "evt_j53_dispute", "charge.dispute.created", dispute)
	dispute["status"] = "won"
	m.notice(t, "evt_j53_dispute_won", "charge.dispute.closed", dispute)
	read, err := a.alpha.GetPayment(t.Context(), disputed)
	require.NoError(t, err)
	require.Len(t, read.Refunds, 2, "the chargeback and the won dispute are recorded")
	recovered := m.decline(t, "j53-recovered")
	pay := billing.PayInvoiceParams{Payment: billing.InvoicePaymentParams{Token: p.core.NewPaymentMethod(stripemock.Card{Brand: "visa", Last4: "4242"})}, ExpectedTotal: 100_000_000, Reusable: ptr(true)}
	status, raw := a.request(t, http.MethodPost, "/v1/me/invoices/"+recovered.String()+"/pay", a.aliceToken, pay, map[string]string{"Idempotency-Key": "j53-recovered-pay"})
	require.Contains(t, []int{http.StatusOK, http.StatusCreated}, status, string(raw))
	paid, err := a.alpha.GetInvoice(t.Context(), recovered)
	require.NoError(t, err)
	require.Equal(t, billing.InvoiceComplete, paid.Status)
	m.decline(t, "j53-failed")

	from := a.clock.Now().Add(-time.Hour)
	due := *sub.CurrentPeriodEndsAt
	// As of two hours past due the renewal is missed and still in dunning;
	// sixteen days past, its dunning window has closed it uncollected.
	inDunning, closed := due.Add(2*time.Hour), due.Add(16*24*time.Hour)
	query := func(measures []string, to time.Time, by ...string) []map[string]any {
		return metricTable(t, a.alpha, billing.QueryMetricsParams{Measures: measures, By: by, Filters: map[string][]string{"currency": {"USD"}},
			Range: &billing.MetricsRange{From: from.Format(time.RFC3339), To: to.Format(time.RFC3339)}})
	}

	// cash is net_revenue in total, and by kind gross_revenue, refunds and
	// chargebacks (net of the won dispute), signed.
	require.Equal(t, int64(280_000_000), whole(t, only(t, query([]string{"cash"}, closed))["cash"]))
	require.Equal(t, map[string]int64{"payment": 310_000_000, "refund": -30_000_000, "chargeback": -100_000_000, "dispute_won": 100_000_000},
		split(t, query([]string{"cash"}, closed, "cash_kind"), "cash_kind", "cash"))
	// payments by kind are payment_count, refund_count and chargeback_count.
	require.Equal(t, map[string]int64{"sale": 4, "refund": 1, "chargeback": 1, "dispute_won": 1},
		split(t, query([]string{"payments"}, closed, "payment_kind"), "payment_kind", "payments"))
	rates := only(t, query([]string{"refund_rate", "chargeback_rate"}, closed))
	require.InDelta(t, 0.25, fraction(t, rates["refund_rate"]), 1e-12)
	require.InDelta(t, 0.25, fraction(t, rates["chargeback_rate"]), 1e-12)

	// attempts by outcome are approved_attempts and failed_attempts: what
	// the attempt list reads.
	listed, err := a.alpha.ListPaymentAttempts(t.Context(), billing.PaymentAttemptListParams{PageRequest: billing.PageRequest{Limit: 100}, CustomerID: m.customer})
	require.NoError(t, err)
	require.Empty(t, listed.Next)
	answered := map[string]int64{}
	for _, attempt := range listed.Items {
		answered[string(attempt.Outcome)]++
	}
	require.Equal(t, int64(2), int64(len(listed.Items))-answered["approved"])
	attempts := query([]string{"attempts"}, closed, "outcome")
	require.Equal(t, answered, split(t, attempts, "outcome", "attempts"))
	rates = only(t, query([]string{"authorization_rate"}, closed))
	require.InDelta(t, float64(answered["approved"])/float64(answered["approved"]+answered["declined"]), fraction(t, rates["authorization_rate"]), 1e-12)

	// purchases by outcome are what failed_purchases, purchase_recovery_rate
	// and settled_purchases counted: one recovered, one failed once quiet an
	// hour, none open.
	purchases := split(t, query([]string{"purchases"}, closed, "outcome"), "outcome", "purchases")
	require.Equal(t, int64(1), purchases["recovered"])
	require.Equal(t, int64(1), purchases["failed"])
	require.Zero(t, purchases["open"])
	require.GreaterOrEqual(t, purchases["approved"], int64(3), "the membership and two invoices were approved at once")
	total := whole(t, only(t, query([]string{"purchases"}, closed))["purchases"])
	require.Equal(t, purchases["approved"]+2, total)

	// rebills is rebills_due; by outcome and first_outcome it is
	// rebills_open, rebills_missed and rebill_first_failures.
	require.Equal(t, map[string]int64{"open": 1}, split(t, query([]string{"rebills"}, inDunning, "outcome"), "outcome", "rebills"))
	require.Equal(t, map[string]int64{"missed": 1}, split(t, query([]string{"rebills"}, inDunning, "first_outcome"), "first_outcome", "rebills"))
	open := only(t, query([]string{"rebills", "rebill_rate", "first_try_rebill_approval_rate"}, inDunning))
	require.Nil(t, open["rebill_rate"], "no renewal has closed")
	require.Nil(t, open["first_try_rebill_approval_rate"], "a missed renewal had no first attempt")
	require.Equal(t, map[string]int64{"lost": 1}, split(t, query([]string{"rebills"}, closed, "outcome"), "outcome", "rebills"))
	lost := only(t, query([]string{"rebill_rate", "dunning_recovery_rate"}, closed))
	require.InDelta(t, 0.0, fraction(t, lost["rebill_rate"]), 1e-12)
	require.InDelta(t, 0.0, fraction(t, lost["dunning_recovery_rate"]), 1e-12)

	// A measure or dimension the registry does not serve is refused.
	_, err = a.alpha.QueryMetrics(t.Context(), billing.QueryMetricsParams{Measures: []string{"net_revenue"},
		Filters: map[string][]string{"attempt_kind": {"renewal"}}, Range: &billing.MetricsRange{Last: "30d"}})
	refusals := metricRefusals(t, err)
	require.Equal(t, "unknown_measure", refusals["measures[0]"]["code"])
	require.Equal(t, "unknown_filter_dimension", refusals["filters.attempt_kind"]["code"])
}

// decline posts an invoice the issuer declines; the 402 names the invoice it
// left open.
func (m *mixedSale) decline(t *testing.T, key string) billing.InvoiceID {
	t.Helper()
	input := billing.CreateMyInvoiceParams{Lines: m.lines, ExpectedTotal: ptr(int64(100_000_000)), Reusable: ptr(true),
		Payment: &billing.InvoicePaymentParams{Token: m.p.core.NewPaymentMethod(stripemock.Card{Brand: "visa", Last4: "0002", Decline: "insufficient_funds"})}}
	status, raw := m.a.request(t, http.MethodPost, "/v1/me/invoices", m.a.aliceToken, input, map[string]string{"Idempotency-Key": key})
	require.Equal(t, http.StatusPaymentRequired, status, string(raw))
	var refused struct {
		Error billing.ErrorDetails `json:"error"`
	}
	require.NoError(t, json.Unmarshal(raw, &refused))
	require.NotNil(t, refused.Error.Invoice, string(raw))
	return refused.Error.Invoice.ID
}

// Alpha sells a membership by card; beta has nothing yet.
func healthOverview(t *testing.T) {
	a, p := newPaymentWorld(t)
	buyMembership(t, a, a.alpha, p, 24)
	sellOnce(t, a, p, "euro", "EUR", 50_000_000)

	board, err := a.alpha.GetDashboard(t.Context())
	require.NoError(t, err)
	require.True(t, board.IsDefault)
	require.Equal(t, &billing.DashboardProfile{Subscriptions: true, CardPayments: true, Checkouts: true}, board.Profile)
	sections := map[string]bool{}
	for _, w := range board.Widgets {
		if w.Viz == dashboard.Heading {
			sections[w.Title] = true
		}
	}
	require.Equal(t, map[string]bool{"Revenue and cash": true, "Recurring": true, "Retention": true, "Payment health": true, "Checkout and customers": true}, sections,
		"no credits or usage section without credits or usage")
	require.Equal(t, "kpi", board.Widgets[0].Viz)
	require.Equal(t, []string{"mrr"}, board.Widgets[0].Query.Measures, "a subscription business leads with MRR")

	// The currency switch lists each currency with cash, apart, and filters
	// every widget to one: no widget's money ever adds the other currency.
	window := &billing.MetricsRange{Last: "30d"}
	require.Equal(t, map[string]int64{"USD": 10_000_000, "EUR": 50_000_000},
		split(t, metricTable(t, a.alpha, billing.QueryMetricsParams{Measures: []string{"cash"}, By: []string{"currency"}, Range: window}), "currency", "cash"))
	for currency, cash := range map[string]int64{"USD": 10_000_000, "EUR": 50_000_000} {
		for id, rows := range overviewAnswers(t, a.alpha, board.Widgets, currency) {
			var sum int64
			for _, row := range rows {
				require.NotContains(t, row, "currency", "%s under %s keeps no other currency", id, currency)
				if value, ok := row["cash"]; ok {
					sum += whole(t, value)
				}
			}
			if slices.Contains(cashWidgets, id) {
				require.Equal(t, cash, sum, "%s under %s", id, currency)
			}
		}
	}

	// An empty merchant's overview is the tiles and nothing else, and every
	// one of them answers.
	empty, err := a.beta.GetDashboard(t.Context())
	require.NoError(t, err)
	require.True(t, empty.IsDefault)
	require.Equal(t, &billing.DashboardProfile{}, empty.Profile)
	for _, w := range empty.Widgets {
		require.Equal(t, "kpi", w.Viz, w.ID)
	}
	overviewAnswers(t, a.beta, empty.Widgets, "")

	// Every profile's overview is a layout the merchant may save.
	all := billing.DashboardProfile{Subscriptions: true, CardPayments: true, Usage: true, Credits: true, Checkouts: true}
	for bits := range 32 {
		profile := billing.DashboardProfile{Subscriptions: bits&1 != 0, CardPayments: bits&2 != 0, Usage: bits&4 != 0, Credits: bits&8 != 0, Checkouts: bits&16 != 0}
		saved, err := a.beta.SetDashboard(t.Context(), billing.SetDashboardParams{Widgets: dashboard.DefaultWidgets(profile)})
		require.NoError(t, err, "%+v", profile)
		require.False(t, saved.IsDefault)
	}
	saved, err := a.beta.GetDashboard(t.Context())
	require.NoError(t, err)
	require.Nil(t, saved.Profile)
	require.Len(t, saved.Widgets, len(dashboard.DefaultWidgets(all)))
	overviewAnswers(t, a.beta, saved.Widgets, "")

	// Nothing saves a removed measure.
	stale := billing.DashboardWidget{ID: "net", Title: "Net revenue", Viz: "stat", Grid: billing.DashboardGrid{W: 3, H: 2},
		Query: billing.QueryMetricsParams{Measures: []string{"net_revenue"}, Range: &billing.MetricsRange{Last: "30d"}}}
	_, err = a.beta.SetDashboard(t.Context(), billing.SetDashboardParams{Widgets: []billing.DashboardWidget{stale}})
	var refusal *billing.StatusError
	require.ErrorAs(t, err, &refusal)
	require.Equal(t, billing.CodeDashboardInvalid, refusal.Code)
}

// cashWidgets are the overview's widgets over cash alone.
var cashWidgets = []string{"kpi-cash", "cash-by-kind", "cash-by-psp"}

// overviewAnswers runs every widget as the console draws it: under the
// page's currency where all its measures take one, a kpi with its sparkline
// by time. It returns each widget's rows.
func overviewAnswers(t *testing.T, client *openrails.Client, widgets []billing.DashboardWidget, currency string) map[string][]map[string]any {
	t.Helper()
	schema, err := client.GetMetricsSchema(t.Context())
	require.NoError(t, err)
	dims := map[string][]string{}
	for _, m := range schema.Measures {
		dims[m.Name] = m.Dims
	}
	out := map[string][]map[string]any{}
	for _, w := range widgets {
		if dashboard.Queryless(w.Viz) {
			require.Empty(t, w.Query.Measures, w.ID)
			continue
		}
		q := w.Query
		switched := currency != ""
		for _, measure := range q.Measures {
			switched = switched && slices.Contains(dims[measure], "currency")
		}
		if switched {
			q.Filters = maps.Clone(q.Filters)
			if q.Filters == nil {
				q.Filters = map[string][]string{}
			}
			q.Filters["currency"] = []string{currency}
		}
		out[w.ID] = metricTable(t, client, q)
		if w.Viz == "kpi" {
			spark := q
			spark.By, spark.Grain, spark.Compare = append([]string{"time"}, q.By...), "auto", ""
			metricTable(t, client, spark)
		}
	}
	return out
}

// sellOnce is alice buying one consumable in currency, paid by card.
func sellOnce(t *testing.T, a *authorityWorld, p *providerLedger, key string, currency catalog.Currency, amount int64) {
	t.Helper()
	product, err := a.alpha.CreateProduct(t.Context(), billing.CreateProductParams{Key: key, DisplayName: key, Ownership: catalog.OwnershipConsumable, Entitlements: []string{key + ":owned"}})
	require.NoError(t, err)
	price, err := a.alpha.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: product.ID, Key: key, UnitAmount: amount, Currency: currency})
	require.NoError(t, err)
	input := billing.CreateMyInvoiceParams{Lines: []billing.InvoiceLineParams{{PriceID: price.ID}}, ExpectedTotal: ptr(amount), Reusable: ptr(true),
		Payment: &billing.InvoicePaymentParams{Token: p.core.NewPaymentMethod(stripemock.Card{Brand: "visa", Last4: "4242"})}}
	status, raw := a.request(t, http.MethodPost, "/v1/me/invoices", a.aliceToken, input, map[string]string{"Idempotency-Key": key})
	require.Equal(t, http.StatusCreated, status, string(raw))
	var invoice billing.Invoice
	require.NoError(t, json.Unmarshal(raw, &invoice))
	require.Equal(t, billing.InvoiceComplete, invoice.Status)
}

// A USD membership and a EUR sale, and prepaid credit held in both.
func moneyPerCurrency(t *testing.T) {
	a, p := newPaymentWorld(t)
	sub := buyMembership(t, a, a.alpha, p, 24)
	sellOnce(t, a, p, "euro", "EUR", 50_000_000)
	_, err := a.alpha.CreateCreditGrants(t.Context(), []billing.CreateCreditGrantParams{
		{CustomerID: sub.CustomerID, Currency: "USD", Amount: 2_000_000, Source: "critical", SourceID: "usd-credit"},
		{CustomerID: sub.CustomerID, Currency: "EUR", Amount: 3_000_000, Source: "critical", SourceID: "eur-credit"},
	})
	require.NoError(t, err)
	window := &billing.MetricsRange{From: a.clock.Now().Add(-time.Hour).Format(time.RFC3339), To: a.clock.Now().Add(time.Hour).Format(time.RFC3339)}

	// Without one currency named, every money measure is split by currency.
	schema, err := a.alpha.GetMetricsSchema(t.Context())
	require.NoError(t, err)
	var money []string
	for _, m := range schema.Measures {
		if m.Money {
			money = append(money, m.Name)
			result, err := a.alpha.QueryMetrics(t.Context(), billing.QueryMetricsParams{Measures: []string{m.Name}, Range: window})
			require.NoError(t, err, m.Name)
			require.Contains(t, result.Columns, billing.MetricsColumn{Name: "currency", Kind: "dimension"}, m.Name)
		}
	}
	require.Subset(t, money, []string{"cash", "mrr", "credits_sold", "usage_charged", "outstanding_credit_liability", "outstanding_owed"})
	perCurrency := map[string]int64{"USD": 10_000_000, "EUR": 50_000_000}
	require.Equal(t, perCurrency, split(t, metricTable(t, a.alpha, billing.QueryMetricsParams{Measures: []string{"cash"}, Range: window}), "currency", "cash"))
	require.Equal(t, perCurrency, split(t, metricTable(t, a.alpha, billing.QueryMetricsParams{Measures: []string{"cash"}, Range: window,
		Filters: map[string][]string{"currency": {"usd", "EUR"}}}), "currency", "cash"), "two currencies named stay apart")
	require.Equal(t, map[string]int64{"USD": 2_000_000, "EUR": 3_000_000},
		split(t, metricTable(t, a.alpha, billing.QueryMetricsParams{Measures: []string{"outstanding_credit_liability"}, Range: window}), "currency", "outstanding_credit_liability"))
	mrr := split(t, metricTable(t, a.alpha, billing.QueryMetricsParams{Measures: []string{"mrr"}, Range: window}), "currency", "mrr")
	require.Equal(t, []string{"USD"}, slices.Collect(maps.Keys(mrr)))
	// Grouped by PSP or time, each row is one currency's.
	for _, by := range [][]string{{"psp"}, {"time", "cash_kind"}} {
		rows := metricTable(t, a.alpha, billing.QueryMetricsParams{Measures: []string{"cash"}, By: by, Grain: "auto", Range: window})
		require.Equal(t, perCurrency, split(t, rows, "currency", "cash"), "%v", by)
	}
	// One currency named answers it alone.
	euro := only(t, metricTable(t, a.alpha, billing.QueryMetricsParams{Measures: []string{"cash"}, Range: window, Filters: map[string][]string{"currency": {"EUR"}}}))
	require.NotContains(t, euro, "currency")
	require.Equal(t, int64(50_000_000), whole(t, euro["cash"]))
	// Money beside a measure with no currency is refused, never summed.
	_, err = a.alpha.QueryMetrics(t.Context(), billing.QueryMetricsParams{Measures: []string{"cash", "customers_at_depletion_risk"}, Range: window})
	require.Contains(t, metricRefusals(t, err), "measures")
}

// metricTable runs q and keys each row's cells by column name.
func metricTable(t *testing.T, client *openrails.Client, q billing.QueryMetricsParams) []map[string]any {
	t.Helper()
	result, err := client.QueryMetrics(t.Context(), q)
	require.NoError(t, err, "%+v", q)
	rows := make([]map[string]any, 0, len(result.Rows))
	for _, row := range result.Rows {
		cells := map[string]any{}
		for i, column := range result.Columns {
			cells[column.Name] = row[i]
		}
		rows = append(rows, cells)
	}
	return rows
}

// only is a result's one row.
func only(t *testing.T, rows []map[string]any) map[string]any {
	t.Helper()
	require.Len(t, rows, 1)
	return rows[0]
}

// split totals measure per value of dim; a value with nothing is absent.
func split(t *testing.T, rows []map[string]any, dim, measure string) map[string]int64 {
	t.Helper()
	out := map[string]int64{}
	for _, row := range rows {
		if n := whole(t, row[measure]); n != 0 {
			out[fmt.Sprint(row[dim])] += n
		}
	}
	return out
}

// whole reads a count or an amount exactly.
func whole(t *testing.T, cell any) int64 {
	t.Helper()
	switch v := cell.(type) {
	case string:
		n, err := strconv.ParseInt(v, 10, 64)
		require.NoError(t, err)
		return n
	case json.Number:
		n, err := v.Int64()
		require.NoError(t, err)
		return n
	case float64:
		require.Equal(t, math.Trunc(v), v, "a whole cell")
		return int64(v)
	}
	t.Fatalf("not a count or an amount: %#v", cell)
	return 0
}

// fraction reads a ratio; nil (nothing to divide by) fails.
func fraction(t *testing.T, cell any) float64 {
	t.Helper()
	switch v := cell.(type) {
	case float64:
		return v
	case json.Number:
		f, err := v.Float64()
		require.NoError(t, err)
		return f
	}
	t.Fatalf("not a ratio: %#v", cell)
	return 0
}

// metricRefusals are a refused query's field errors, by param.
func metricRefusals(t *testing.T, err error) map[string]map[string]any {
	t.Helper()
	var refusal *billing.StatusError
	require.ErrorAs(t, err, &refusal)
	require.Equal(t, http.StatusBadRequest, refusal.Status)
	require.Equal(t, billing.CodeMetricsQueryInvalid, refusal.Code)
	listed, ok := refusal.Metadata["errors"].([]any)
	require.True(t, ok, "%v", refusal.Metadata)
	out := map[string]map[string]any{}
	for _, item := range listed {
		fe, ok := item.(map[string]any)
		require.True(t, ok)
		out[fmt.Sprint(fe["param"])] = fe
	}
	return out
}
