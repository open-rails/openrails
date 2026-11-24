//go:build integration

package critical_test

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/disputerules"
	"github.com/open-rails/openrails/internal/modules/metrics"
	"github.com/open-rails/openrails/internal/sqlschema"
	"github.com/open-rails/openrails/internal/stripemock"
	"github.com/stretchr/testify/require"
)

// Recurring revenue reporting: the MRR measures over four months of two
// customers, the dispute programs' levels on the overview, and the rollups
// that answer every MRR measure.
func TestReportingMRR(t *testing.T) {
	t.Run("OR-J53-S06/recurring-measures-over-four-months-equal-the-movements", recurringMeasures)
	t.Run("OR-J53-S07/the-chargeback-ratio-tile-and-charts-carry-the-dispute-programs-levels", programLevels)
	t.Run("OR-J42-S30/every-mrr-measure-plans-on-rollups-without-a-sequential-scan-of-movements-or-subscriptions", mrrOnRollups)
}

// Alice and Bob over four calendar months, M0 to M3, five to two months back:
//   - M0: Alice's yearly membership and Bob's yearly plan are new.
//   - M1: Alice adds a plan in EUR; Bob adds a larger plan and the merchant
//     ends his first (expansion, contraction).
//   - M2: Alice charges back her membership: churn by chargeback in USD, but
//     she keeps MRR in EUR.
//   - M3: the merchant ends Bob's plan (churn of the opening base); Bob comes
//     back, is ended again (churn outside the base) and comes back again,
//     cancels at period end and is resumed.
func recurringMeasures(t *testing.T) {
	now := time.Now().UTC()
	start := time.Date(now.Year(), now.Month()-5, 1, 0, 0, 0, 0, time.UTC)
	month := func(n int) time.Time { return start.AddDate(0, n, 0) }
	on := func(n int) time.Time { return month(n).Add(4*24*time.Hour + 12*time.Hour) }
	days := func(n int) int { return month(n+1).AddDate(0, 0, -1).Day() }
	bucket := func(n int) string { return month(n).Format(time.RFC3339) }
	a, p, membership := purchasedMembershipWith(t, 8760, func(w *world) { w.clock.Advance(on(0).Sub(w.clock.Now())) })
	effects := p.effects(t)
	paid, err := a.alpha.ListPayments(t.Context(), billing.PaymentListParams{SubscriptionID: membership.ID})
	require.NoError(t, err)
	require.Len(t, paid.Items, 1)
	disputed := &disputedMembership{a: a, p: p, sub: membership, payment: paid.Items[0].ID, charge: effects[0].ID, intent: effects[0].Operation}
	alice := mrrOf(t, a.alpha, membership.ID).Amount
	yearly := func(key string, amount int64, currency catalog.Currency) billing.PriceID {
		return mrrPlan(t, a.alpha, key, billing.CreatePriceParams{UnitAmount: amount, Currency: currency, BillingIntervalHours: ptr(8760), AccessDurationHours: ptr(8760)})
	}
	year, large, euro := yearly("mrr-year", 120_000_000, "USD"), yearly("mrr-year-large", 240_000_000, "USD"), yearly("mrr-year-eur", 120_000_000, "EUR")
	merchantEnds := func(id billing.SubscriptionID) {
		t.Helper()
		_, err := a.alpha.CancelSubscription(t.Context(), id, billing.CancelSubscriptionParams{Reason: "Merchant ends the plan", RevokeAccess: true})
		require.NoError(t, err)
	}
	at := func(when time.Time) { a.clock.Advance(when.Sub(a.clock.Now())) }

	first := buyAs(t, a, p, a.bobToken, year, 120_000_000, "bob-year")
	at(on(1))
	buyAs(t, a, p, a.aliceToken, euro, 120_000_000, "alice-year-eur")
	second := buyAs(t, a, p, a.bobToken, large, 240_000_000, "bob-year-large")
	a.clock.Advance(time.Hour)
	merchantEnds(first.ID)
	at(on(2))
	startFleet(t, a.world)
	disputed.notice(t, "evt_mrr_chargeback", "charge.dispute.created", disputed.dispute("dp_mrr", "needs_response", "chargeback", "fraudulent"))
	require.Equal(t, "chargeback", *subscriptionState(t, a, membership.ID, "canceled").CancelType)
	ended := month(3).Add(19*24*time.Hour + 12*time.Hour)
	at(ended)
	merchantEnds(second.ID)
	at(ended.Add(24 * time.Hour))
	again := buyAs(t, a, p, a.bobToken, year, 120_000_000, "bob-year-again")
	at(ended.Add(48 * time.Hour))
	merchantEnds(again.ID)
	at(ended.Add(72 * time.Hour))
	back := buyAs(t, a, p, a.bobToken, year, 120_000_000, "bob-year-back")

	require.Equal(t, []string{mrrKind("USD", "new", alice), mrrKind("EUR", "new", 10_000_000), mrrKind("USD", "churn", -alice)},
		mrrKinds(mrrMoves(t, a.world, a.alpha, customerOf(a.alice))))
	require.Equal(t, []string{mrrKind("USD", "new", 10_000_000), mrrKind("USD", "expansion", 20_000_000), mrrKind("USD", "contraction", -10_000_000),
		mrrKind("USD", "churn", -20_000_000), mrrKind("USD", "reactivation", 10_000_000), mrrKind("USD", "churn", -10_000_000),
		mrrKind("USD", "reactivation", 10_000_000)}, mrrKinds(mrrMoves(t, a.world, a.alpha, customerOf(a.bob))))

	usd := map[string][]string{"currency": {"USD"}}
	months := &billing.MetricsRange{From: month(0).Format(time.DateOnly), To: month(4).AddDate(0, 0, -1).Format(time.DateOnly)}

	// Movements by month are the movements recorded, from the flows.
	amounts, logos := map[string]int64{}, map[string]int64{}
	for _, row := range metricTable(t, a.alpha, billing.QueryMetricsParams{Measures: []string{"mrr_movements", "subscriber_movements"},
		By: []string{"time", "movement"}, Grain: "month", Range: months, Filters: usd}) {
		key := fmt.Sprint(row["time"], " ", row["movement"])
		if n := whole(t, row["mrr_movements"]); n != 0 {
			amounts[key] = n
		}
		if n := whole(t, row["subscriber_movements"]); n != 0 {
			logos[key] = n
		}
	}
	recorded := map[string]int64{}
	for _, m := range append(mrrMoves(t, a.world, a.alpha, customerOf(a.alice)), mrrMoves(t, a.world, a.alpha, customerOf(a.bob))...) {
		if m.Currency == "USD" {
			recorded[time.Date(m.OccurredAt.Year(), m.OccurredAt.Month(), 1, 0, 0, 0, 0, time.UTC).Format(time.RFC3339)+" "+m.Kind] += m.Delta
		}
	}
	require.Equal(t, recorded, amounts)
	require.Equal(t, map[string]int64{bucket(0) + " new": alice + 10_000_000, bucket(1) + " expansion": 20_000_000, bucket(1) + " contraction": -10_000_000,
		bucket(2) + " churn": -alice, bucket(3) + " churn": -30_000_000, bucket(3) + " reactivation": 20_000_000}, amounts)
	require.Equal(t, map[string]int64{bucket(0) + " new": 2, bucket(2) + " churn": -1, bucket(3) + " churn": -2, bucket(3) + " reactivation": 2}, logos)

	// Each month's MRR, customers and ratios as it began. A month's churn
	// counts only its opening base: Bob's second loss in M3 is not churn of
	// the base he began it in.
	type opening struct {
		mrr, subscribers int64
		arpa, ltv        any
		churn, nrr, grr  any
	}
	half := func(n int64) int64 { return (n + 1) / 2 }
	ratio := func(n, d int64) float64 { return float64(n) / float64(d) }
	began := map[string]opening{}
	for _, row := range metricTable(t, a.alpha, billing.QueryMetricsParams{Measures: []string{"mrr", "subscribers", "arpa", "ltv", "subscriber_churn_rate", "nrr", "grr"},
		By: []string{"time"}, Grain: "month", Range: months, Filters: usd}) {
		s := opening{mrr: whole(t, row["mrr"]), subscribers: whole(t, row["subscribers"])}
		if row["arpa"] != nil {
			s.arpa = whole(t, row["arpa"])
		}
		if row["ltv"] != nil {
			s.ltv = whole(t, row["ltv"])
		}
		for _, r := range []struct {
			to   *any
			name string
		}{{&s.churn, "subscriber_churn_rate"}, {&s.nrr, "nrr"}, {&s.grr, "grr"}} {
			if row[r.name] != nil {
				*r.to = fraction(t, row[r.name])
			}
		}
		began[fmt.Sprint(row["time"])] = s
	}
	require.Equal(t, map[string]opening{
		bucket(0): {},
		bucket(1): {mrr: alice + 10_000_000, subscribers: 2, arpa: half(alice + 10_000_000), churn: 0.0,
			nrr: ratio(alice+20_000_000, alice+10_000_000), grr: ratio(alice, alice+10_000_000)},
		bucket(2): {mrr: alice + 20_000_000, subscribers: 2, arpa: half(alice + 20_000_000), ltv: alice + 20_000_000, churn: 0.5,
			nrr: ratio(20_000_000, alice+20_000_000), grr: ratio(20_000_000, alice+20_000_000)},
		bucket(3): {mrr: 20_000_000, subscribers: 1, arpa: int64(20_000_000), ltv: int64(20_000_000), churn: 1.0, nrr: 0.0, grr: 0.0},
	}, began, "M0 begins with no MRR: no ratio")

	// Churn split by its cause.
	byType := map[string]float64{}
	for _, row := range metricTable(t, a.alpha, billing.QueryMetricsParams{Measures: []string{"subscriber_churn_rate"}, By: []string{"time", "churn_type"},
		Grain: "month", Range: months, Filters: usd}) {
		if when := fmt.Sprint(row["time"]); when == bucket(2) || when == bucket(3) {
			byType[when+" "+fmt.Sprint(row["churn_type"])] = fraction(t, row["subscriber_churn_rate"])
		}
	}
	require.Equal(t, map[string]float64{bucket(2) + " chargeback": 0.5, bucket(2) + " voluntary": 0, bucket(2) + " involuntary": 0,
		bucket(3) + " chargeback": 0, bucket(3) + " voluntary": 1, bucket(3) + " involuntary": 0}, byType)

	// Churn reads monthly at any grain: the day Alice charged back lost half
	// its base, a monthly rate of 1 - 0.5^(days in M2); 90 days from M0's
	// 6th lost half of theirs over their calendar months.
	for _, row := range metricTable(t, a.alpha, billing.QueryMetricsParams{Measures: []string{"subscriber_churn_rate"}, By: []string{"time"}, Grain: "day",
		Range: &billing.MetricsRange{From: month(2).Format(time.DateOnly), To: month(3).AddDate(0, 0, -1).Format(time.DateOnly)}, Filters: usd}) {
		if fmt.Sprint(row["time"]) == on(2).Truncate(24*time.Hour).Format(time.RFC3339) {
			require.InDelta(t, 1-math.Pow(0.5, float64(days(2))), fraction(t, row["subscriber_churn_rate"]), 1e-12)
		}
	}
	from := month(0).AddDate(0, 0, 5)
	quarter := only(t, metricTable(t, a.alpha, billing.QueryMetricsParams{Measures: []string{"subscriber_churn_rate", "arpa", "ltv"},
		Range: &billing.MetricsRange{From: from.Format(time.DateOnly), To: from.AddDate(0, 0, 89).Format(time.DateOnly)}, Filters: usd}))
	spanned := float64(days(0)-5)/float64(days(0)) + 2 + float64(90-(days(0)-5)-days(1)-days(2))/float64(days(3))
	monthly := 1 - math.Pow(0.5, 1/spanned)
	require.InDelta(t, monthly, fraction(t, quarter["subscriber_churn_rate"]), 1e-12)
	require.InEpsilon(t, float64(whole(t, quarter["arpa"]))/monthly, float64(whole(t, quarter["ltv"])), 1e-9, "LTV is ARPA over the monthly churn")

	// One USD cohort, M0, of the two customers it acquired, at the end of
	// each month since: Bob's return counts in it again.
	cohort := map[string]float64{}
	for _, row := range metricTable(t, a.alpha, billing.QueryMetricsParams{Measures: []string{"cohort_retention"}, By: []string{"cohort", "months_since_start", "basis"},
		Range: months, Filters: usd}) {
		cohort[fmt.Sprint(row["basis"], " ", row["cohort"], " +", row["months_since_start"])] = fraction(t, row["cohort_retention"])
	}
	label := month(0).Format("2006-01")
	require.Equal(t, map[string]float64{
		"subscribers " + label + " +0": 1, "subscribers " + label + " +1": 1, "subscribers " + label + " +2": 0.5, "subscribers " + label + " +3": 0.5,
		"mrr " + label + " +0": 1, "mrr " + label + " +1": ratio(alice+20_000_000, alice+10_000_000), "mrr " + label + " +2": ratio(20_000_000, alice+10_000_000),
		"mrr " + label + " +3": ratio(10_000_000, alice+10_000_000),
	}, cohort)

	// MRR by product: as M2 began, Alice's membership and Bob's larger plan;
	// at the range's end, Bob's plan alone.
	byProduct := func(to time.Time) map[string]int64 {
		return split(t, metricTable(t, a.alpha, billing.QueryMetricsParams{Measures: []string{"mrr"}, By: []string{"product_id"},
			Range: &billing.MetricsRange{From: month(0).Format(time.DateOnly), To: to.AddDate(0, 0, -1).Format(time.DateOnly)}, Filters: usd}), "product_id", "mrr")
	}
	require.Equal(t, map[string]int64{membership.ProductID.String(): alice, productOf(t, a, large): 20_000_000}, byProduct(month(2)))
	require.Equal(t, map[string]int64{productOf(t, a, year): 10_000_000}, byProduct(month(4)))

	// Without a currency each customer counts once: as M2 began Alice had
	// MRR in USD and EUR.
	for _, c := range []struct {
		to       time.Time
		distinct int64
		split    map[string]int64
	}{
		{month(2), 2, map[string]int64{"USD": 2, "EUR": 1}},
		{month(3), 2, map[string]int64{"USD": 1, "EUR": 1}},
	} {
		window := &billing.MetricsRange{From: month(0).Format(time.DateOnly), To: c.to.AddDate(0, 0, -1).Format(time.DateOnly)}
		require.EqualValues(t, c.distinct, whole(t, only(t, metricTable(t, a.alpha, billing.QueryMetricsParams{Measures: []string{"subscribers"}, Range: window}))["subscribers"]))
		require.Equal(t, c.split, split(t, metricTable(t, a.alpha, billing.QueryMetricsParams{Measures: []string{"subscribers"}, By: []string{"currency"}, Range: window}), "currency", "subscribers"))
	}

	// Bob's cancel at period end schedules its churn; the resume takes it back.
	status, raw := a.request(t, http.MethodPost, "/v1/me/subscriptions/"+back.ID.String()+"/cancel", a.bobToken, map[string]any{"reason": "Bob stops renewing"})
	require.Equal(t, http.StatusOK, status, string(raw))
	ahead := &billing.MetricsRange{From: a.clock.Now().Format(time.RFC3339), To: back.CurrentPeriodEndsAt.Add(time.Second).Format(time.RFC3339)}
	scheduled := func() map[string]int64 {
		return split(t, metricTable(t, a.alpha, billing.QueryMetricsParams{Measures: []string{"scheduled_mrr"}, By: []string{"movement"}, Range: ahead, Filters: usd}),
			"movement", "scheduled_mrr")
	}
	require.Equal(t, map[string]int64{"churn": -10_000_000}, scheduled())
	_, err = a.alpha.ResumeSubscription(t.Context(), back.ID)
	require.NoError(t, err)
	require.Empty(t, scheduled(), "a resume takes the scheduled churn back")

	// Each cancel decision counts in the month it was made, by its type and
	// whether a resume took it back.
	cancels := map[string]int64{}
	for _, row := range metricTable(t, a.alpha, billing.QueryMetricsParams{Measures: []string{"cancellations"}, By: []string{"time", "cancel_type", "reversed"},
		Grain: "month", Range: months}) {
		if n := whole(t, row["cancellations"]); n != 0 {
			cancels[fmt.Sprint(row["time"], " ", row["cancel_type"], " ", row["reversed"])] = n
		}
	}
	require.Equal(t, map[string]int64{bucket(1) + " merchant false": 1, bucket(2) + " chargeback false": 1, bucket(3) + " merchant false": 2,
		bucket(3) + " user true": 1}, cancels)

	// The overview reads the same: MRR and subscribers now, and every widget
	// answers.
	board, err := a.alpha.GetDashboard(t.Context())
	require.NoError(t, err)
	answers := overviewAnswers(t, a.alpha, board.Widgets, "USD")
	require.EqualValues(t, 10_000_000, whole(t, only(t, answers["kpi-mrr"])["mrr"]))
	require.EqualValues(t, 1, whole(t, only(t, answers["kpi-subscribers"])["subscribers"]))
	require.NotEmpty(t, answers["cohort-retention"])
}

// A card merchant's overview reads the dispute programs' levels where the
// monitor keeps them: its chargeback ratio tile is Visa's VAMP ratio, which
// names its program, and Payment health draws each program's ratio against
// the levels in force each month: VAMP's excessive level fell from 2.2% to
// 1.5% on 1 April 2026. One chargeback of one Visa sale is a ratio past every
// level, yet under VAMP's minimum count no level is reached: the tile reads
// the monitor's level, not the ratio's.
func programLevels(t *testing.T) {
	m := newDisputedMembership(t)
	a := m.a
	startFleet(t, a.world)
	m.notice(t, "evt_levels_chargeback", "charge.dispute.created", m.dispute("dp_levels", "needs_response", "chargeback", "fraudulent"))
	waitDisputeJobs(t, a)

	board, err := a.alpha.GetDashboard(t.Context())
	require.NoError(t, err)
	shown := map[string]billing.DashboardWidget{}
	for _, w := range board.Widgets {
		shown[w.ID] = w
	}
	for id, measure := range map[string]string{"kpi-chargeback-ratio": "vamp_ratio", "vamp-ratio": "vamp_ratio", "ecm-ratio": "ecm_ratio"} {
		require.Equal(t, []string{measure}, shown[id].Query.Measures, id)
	}
	require.Equal(t, "kpi", shown["kpi-chargeback-ratio"].Viz)
	require.Equal(t, "line", shown["vamp-ratio"].Viz)
	require.Equal(t, "line", shown["ecm-ratio"].Viz)

	schema, err := a.alpha.GetMetricsSchema(t.Context())
	require.NoError(t, err)
	measures := map[string]billing.MetricsMeasure{}
	for _, measure := range schema.Measures {
		measures[measure.Name] = measure
	}
	require.Equal(t, billing.ProgramVisaVAMP, measures["vamp_ratio"].Program)
	require.Equal(t, billing.ProgramMastercardECP, measures["ecm_ratio"].Program)
	require.Equal(t, disputerules.ProgramLines(billing.ProgramMastercardECP), measures["ecm_ratio"].Thresholds)
	inForce := func(month time.Time) map[float64]string {
		out := map[float64]string{}
		for _, l := range measures["vamp_ratio"].Thresholds {
			if (l.From == nil || !month.Before(*l.From)) && (l.Until == nil || month.Before(*l.Until)) {
				out[l.Value] = l.Label
			}
		}
		return out
	}
	require.Equal(t, map[float64]string{0.005: "warning", 0.022: "excessive"}, inForce(time.Date(2026, time.March, 1, 0, 0, 0, 0, time.UTC)))
	require.Equal(t, map[float64]string{0.005: "warning", 0.015: "excessive"}, inForce(time.Date(2026, time.April, 1, 0, 0, 0, 0, time.UTC)))

	day := a.clock.Now()
	window := &billing.MetricsRange{From: day.AddDate(0, 0, -1).Format(time.DateOnly), To: day.Format(time.DateOnly)}
	ratio := fraction(t, only(t, metricTable(t, a.alpha, billing.QueryMetricsParams{Measures: []string{"vamp_ratio"}, Range: window}))["vamp_ratio"])
	require.InDelta(t, 1, ratio, 1e-12, "one chargeback of one Visa sale")
	vamp := standing(t, a, billing.ProgramVisaVAMP, day)
	require.EqualValues(t, 1, vamp.Count)
	require.Equal(t, billing.DisputeLevelNone, vamp.Level, "under VAMP's minimum count no level is reached")
	overviewAnswers(t, a.alpha, board.Widgets, "")
}

// Every MRR measure, by itself and by each dimension it declares, over time
// and not, in UTC and in New York, plans on indexes: with sequential scans
// off no relation is scanned whole, every scan's index condition names the
// merchant, and a rollup kept by time is read by its time.
func mrrOnRollups(t *testing.T) {
	a, _, _ := purchasedMembership(t)
	york, err := time.LoadLocation("America/New_York")
	require.NoError(t, err)
	byTime := map[string]string{"mrr_flows": "bucket_at", "mrr_product_flows": "bucket_at", "mrr_customer_flows": "bucket_at", "mrr_streak_flows": "bucket_at",
		"mrr_cohort_flows": "cohort_at", "mrr_risk_flows": "bucket_at", "mrr_scheduled": "effective_at", "subscription_cancellations": "decided_at",
		"customer_mrr": "first_active_at"}
	names := []string{"mrr", "subscribers", "arpa", "ltv", "mrr_movements", "subscriber_movements", "scheduled_mrr", "cancellations",
		"subscriber_churn_rate", "nrr", "grr", "cohort_retention", "mrr_at_risk", "at_risk_recovery_rate"}
	window := &billing.MetricsRange{From: a.clock.Now().AddDate(0, -3, 0).Format(time.DateOnly), To: a.clock.Now().Format(time.DateOnly)}
	planned := 0
	for _, zone := range []*time.Location{time.UTC, york} {
		for _, name := range names {
			i := slices.IndexFunc(metrics.Measures, func(m metrics.Measure) bool { return m.Name == name && !m.Internal })
			require.GreaterOrEqual(t, i, 0, name)
			measure := metrics.Measures[i]
			cuts := [][]string{nil}
			for _, d := range measure.Dims {
				cuts = append(cuts, []string{d})
			}
			for _, cut := range cuts {
				for _, timed := range []bool{false, true} {
					q := billing.QueryMetricsParams{Measures: []string{name}, By: cut, Range: window}
					if name == "cohort_retention" {
						if timed {
							continue
						}
						q.By = []string{"cohort", "months_since_start"}
						if len(cut) > 0 && !slices.Contains(q.By, cut[0]) {
							q.By = append(q.By, cut...)
						}
					}
					if name == "scheduled_mrr" {
						q.Range = &billing.MetricsRange{Next: "12w"}
					}
					if timed {
						q.By, q.Grain = append([]string{"time"}, q.By...), "week"
					}
					plan, refused := metrics.ValidateIn(&q, a.clock.Now(), zone)
					require.Nil(t, refused, "%+v", q)
					statements, err := metrics.Statements(plan, a.alpha.MerchantID().UUID())
					require.NoError(t, err)
					require.NotEmpty(t, statements)
					for _, st := range statements {
						sql, err := sqlschema.Rewrite(st.SQL, a.schema)
						require.NoError(t, err)
						tx, err := a.pool.Begin(t.Context())
						require.NoError(t, err)
						_, err = tx.Exec(t.Context(), "SET LOCAL enable_seqscan = off")
						require.NoError(t, err)
						var explained string
						require.NoError(t, tx.QueryRow(t.Context(), "EXPLAIN (FORMAT JSON) "+sql, st.Args...).Scan(&explained), "%s: %s", st.Family, sql)
						require.NoError(t, tx.Rollback(t.Context()))
						var plans []struct {
							Plan planNode `json:"Plan"`
						}
						require.NoError(t, json.Unmarshal([]byte(explained), &plans))
						nodes := plans[0].Plan.all()
						for k, node := range nodes {
							if node.Relation == "" {
								continue
							}
							where := fmt.Sprintf("%s by %v in %s, %s", name, q.By, zone, node.Relation)
							require.NotEqual(t, "Seq Scan", node.Type, "%s is scanned whole: %s", where, explained)
							require.NotEqual(t, "mrr_movements", node.Relation, "%s reads the movements", where)
							cond := node.IndexCond + node.RecheckCond
							if node.Type == "Bitmap Heap Scan" && k+1 < len(nodes) {
								cond += nodes[k+1].IndexCond
							}
							require.Contains(t, cond, "merchant_id", "%s: %s", where, explained)
							if column, ok := byTime[node.Relation]; ok {
								require.Contains(t, cond, column, "%s: %s", where, explained)
							}
						}
						planned++
					}
				}
			}
		}
	}
	require.Greater(t, planned, 100)
}

// planNode is a node of an EXPLAIN (FORMAT JSON) plan.
type planNode struct {
	Type        string     `json:"Node Type"`
	Relation    string     `json:"Relation Name"`
	IndexCond   string     `json:"Index Cond"`
	RecheckCond string     `json:"Recheck Cond"`
	Plans       []planNode `json:"Plans"`
}

// all is the node and every node under it, subplans included.
func (n planNode) all() []planNode {
	out := []planNode{n}
	for _, c := range n.Plans {
		out = append(out, c.all()...)
	}
	return out
}

// buyAs is the customer signed in with token buying price with a new card;
// it answers the subscription.
func buyAs(t *testing.T, a *authorityWorld, provider *providerLedger, token string, price billing.PriceID, total int64, key string) *billing.Subscription {
	t.Helper()
	card := provider.core.NewPaymentMethod(stripemock.Card{Brand: "visa", Last4: "4242"})
	input := billing.CreateMyOrderParams{Lines: []billing.OrderLineParams{{PriceID: price}}, ExpectedTotal: ptr(total), Reusable: ptr(true), Payment: &billing.OrderPaymentParams{Token: card}}
	status, raw := a.request(t, http.MethodPost, "/v1/me/orders", token, input, map[string]string{"Idempotency-Key": key})
	require.Equal(t, http.StatusCreated, status, string(raw))
	var order billing.Order
	require.NoError(t, json.Unmarshal(raw, &order))
	require.Equal(t, billing.OrderComplete, order.Status, string(raw))
	require.NotNil(t, order.Lines[0].SubscriptionID)
	sub, err := a.alpha.GetSubscription(t.Context(), *order.Lines[0].SubscriptionID)
	require.NoError(t, err)
	return sub
}

// productOf is the prefixed id of price's product.
func productOf(t *testing.T, a *authorityWorld, price billing.PriceID) string {
	t.Helper()
	read, err := a.alpha.GetPrice(t.Context(), price)
	require.NoError(t, err)
	return read.ProductID.String()
}
