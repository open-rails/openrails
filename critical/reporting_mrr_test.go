//go:build integration

package critical_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/open-rails/openrails/billing"
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

// Alice and Bob over four calendar months, M0 to M3, five to two months
// back. M0: Alice's yearly membership and Bob's yearly plan are new. M1: Bob
// adds a larger plan and the merchant ends his first (expansion,
// contraction). M2: Alice charges back her membership and the merchant ends
// Bob's plan (churn by chargeback, voluntary churn). M3: Bob comes back,
// cancels at period end and is resumed.
func recurringMeasures(t *testing.T) {
	now := time.Now().UTC()
	start := time.Date(now.Year(), now.Month()-5, 1, 0, 0, 0, 0, time.UTC)
	month := func(n int) time.Time { return start.AddDate(0, n, 0) }
	on := func(n int) time.Time { return month(n).Add(4*24*time.Hour + 12*time.Hour) }
	bucket := func(n int) string { return month(n).Format(time.RFC3339) }
	a, p, membership := purchasedMembershipWith(t, 8760, func(w *world) { w.clock.Advance(on(0).Sub(w.clock.Now())) })
	effects := p.effects(t)
	paid, err := a.alpha.ListPayments(t.Context(), billing.PaymentListParams{SubscriptionID: membership.ID})
	require.NoError(t, err)
	require.Len(t, paid.Items, 1)
	disputed := &disputedMembership{a: a, p: p, sub: membership, payment: paid.Items[0].ID, charge: effects[0].ID, intent: effects[0].Operation}
	alice := mrrOf(t, a.alpha, membership.ID).Amount
	year := mrrPlan(t, a.alpha, "mrr-year", billing.CreatePriceParams{UnitAmount: 120_000_000, Currency: "USD", BillingIntervalHours: ptr(8760), AccessDurationHours: ptr(8760)})
	large := mrrPlan(t, a.alpha, "mrr-year-large", billing.CreatePriceParams{UnitAmount: 240_000_000, Currency: "USD", BillingIntervalHours: ptr(8760), AccessDurationHours: ptr(8760)})

	first := buyAs(t, a, p, a.bobToken, year, 120_000_000, "bob-year")
	a.clock.Advance(on(1).Sub(a.clock.Now()))
	second := buyAs(t, a, p, a.bobToken, large, 240_000_000, "bob-year-large")
	a.clock.Advance(time.Hour)
	_, err = a.alpha.CancelSubscription(t.Context(), first.ID, billing.CancelSubscriptionParams{Reason: "Merchant ends the smaller plan", RevokeAccess: true})
	require.NoError(t, err)
	a.clock.Advance(on(2).Sub(a.clock.Now()))
	startFleet(t, a.world)
	disputed.notice(t, "evt_mrr_chargeback", "charge.dispute.created", disputed.dispute("dp_mrr", "needs_response", "chargeback", "fraudulent"))
	require.Equal(t, "chargeback", *subscriptionState(t, a, membership.ID, "canceled").CancelType)
	a.clock.Advance(time.Hour)
	_, err = a.alpha.CancelSubscription(t.Context(), second.ID, billing.CancelSubscriptionParams{Reason: "Merchant ends the larger plan", RevokeAccess: true})
	require.NoError(t, err)
	a.clock.Advance(on(3).Sub(a.clock.Now()))
	back := buyAs(t, a, p, a.bobToken, year, 120_000_000, "bob-year-again")

	require.Equal(t, []string{mrrKind("USD", "new", alice), mrrKind("USD", "churn", -alice)}, mrrKinds(mrrMoves(t, a.world, a.alpha, customerOf(a.alice))))
	require.Equal(t, []string{mrrKind("USD", "new", 10_000_000), mrrKind("USD", "expansion", 20_000_000), mrrKind("USD", "contraction", -10_000_000),
		mrrKind("USD", "churn", -20_000_000), mrrKind("USD", "reactivation", 10_000_000)}, mrrKinds(mrrMoves(t, a.world, a.alpha, customerOf(a.bob))))

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
		recorded[time.Date(m.OccurredAt.Year(), m.OccurredAt.Month(), 1, 0, 0, 0, 0, time.UTC).Format(time.RFC3339)+" "+m.Kind] += m.Delta
	}
	require.Equal(t, recorded, amounts)
	require.Equal(t, map[string]int64{bucket(0) + " new": alice + 10_000_000, bucket(1) + " expansion": 20_000_000, bucket(1) + " contraction": -10_000_000,
		bucket(2) + " churn": -alice - 20_000_000, bucket(3) + " reactivation": 10_000_000}, amounts)
	require.Equal(t, map[string]int64{bucket(0) + " new": 2, bucket(2) + " churn": -2, bucket(3) + " reactivation": 1}, logos)

	// Each month's MRR, customers and ratios as it began.
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
		bucket(2): {mrr: alice + 20_000_000, subscribers: 2, arpa: half(alice + 20_000_000), ltv: half(alice + 20_000_000), churn: 1.0, nrr: 0.0, grr: 0.0},
		bucket(3): {},
	}, began, "M0 and M3 begin with no MRR: no ratio")

	// M2's churn split by its cause.
	byType := map[string]float64{}
	for _, row := range metricTable(t, a.alpha, billing.QueryMetricsParams{Measures: []string{"subscriber_churn_rate"}, By: []string{"time", "churn_type"},
		Grain: "month", Range: months, Filters: usd}) {
		if fmt.Sprint(row["time"]) == bucket(2) {
			byType[fmt.Sprint(row["churn_type"])] = fraction(t, row["subscriber_churn_rate"])
		}
	}
	require.Equal(t, map[string]float64{"voluntary": 0.5, "chargeback": 0.5, "involuntary": 0}, byType)

	// One cohort, M0, at the end of each month since: Bob's return counts in
	// it again.
	cohort := map[string]float64{}
	for _, row := range metricTable(t, a.alpha, billing.QueryMetricsParams{Measures: []string{"cohort_retention"}, By: []string{"cohort", "months_since_start", "basis"},
		Range: months, Filters: usd}) {
		cohort[fmt.Sprint(row["basis"], " ", row["cohort"], " +", row["months_since_start"])] = fraction(t, row["cohort_retention"])
	}
	label := month(0).Format("2006-01")
	require.Equal(t, map[string]float64{
		"subscribers " + label + " +0": 1, "subscribers " + label + " +1": 1, "subscribers " + label + " +2": 0, "subscribers " + label + " +3": 0.5,
		"mrr " + label + " +0": 1, "mrr " + label + " +1": ratio(alice+20_000_000, alice+10_000_000), "mrr " + label + " +2": 0,
		"mrr " + label + " +3": ratio(10_000_000, alice+10_000_000),
	}, cohort)

	// MRR by product at the range's end is Bob's plan alone.
	require.Equal(t, map[string]int64{productOf(t, a, year): 10_000_000},
		split(t, metricTable(t, a.alpha, billing.QueryMetricsParams{Measures: []string{"mrr"}, By: []string{"product_id"}, Range: months, Filters: usd}), "product_id", "mrr"))

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

	// A cancel is dated when OpenRails records it, on the database's clock:
	// these were recorded today.
	cancels := map[string]int64{}
	for _, row := range metricTable(t, a.alpha, billing.QueryMetricsParams{Measures: []string{"cancellations"}, By: []string{"cancel_type", "reversed"},
		Range: &billing.MetricsRange{From: month(0).Format(time.DateOnly), To: now.AddDate(0, 0, 1).Format(time.DateOnly)}}) {
		if n := whole(t, row["cancellations"]); n != 0 {
			cancels[fmt.Sprint(row["cancel_type"], " ", row["reversed"])] = n
		}
	}
	require.Equal(t, map[string]int64{"merchant false": 2, "chargeback false": 1, "user true": 1}, cancels)

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
// monitor keeps them: its chargeback ratio tile is Visa's VAMP ratio, and
// Payment health draws each program's ratio against its own levels.
func programLevels(t *testing.T) {
	a, p := newPaymentWorld(t)
	sellOnce(t, a, p, "one-off", "USD", 10_000_000)
	board, err := a.alpha.GetDashboard(t.Context())
	require.NoError(t, err)
	require.NotNil(t, board.Profile)
	require.True(t, board.Profile.CardPayments)
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
	lines := map[string][]billing.MetricsThreshold{}
	for _, m := range schema.Measures {
		lines[m.Name] = m.Thresholds
	}
	require.NotEmpty(t, lines["vamp_ratio"])
	require.Equal(t, disputerules.ProgramLines(billing.ProgramVisaVAMP), lines["vamp_ratio"])
	require.Equal(t, disputerules.ProgramLines(billing.ProgramMastercardECP), lines["ecm_ratio"])
	overviewAnswers(t, a.alpha, board.Widgets, "")
}

// Every MRR measure, by each of its cuts, plans on the rollups: with
// sequential scans off, no plan reads mrr_movements and none scans
// subscriptions whole.
func mrrOnRollups(t *testing.T) {
	a, _, _ := purchasedMembership(t)
	window := &billing.MetricsRange{From: a.clock.Now().AddDate(0, -3, 0).Format(time.DateOnly), To: a.clock.Now().Format(time.DateOnly)}
	queries := []billing.QueryMetricsParams{
		{Measures: []string{"mrr", "subscribers", "arpa"}},
		{Measures: []string{"mrr", "subscribers", "arpa"}, By: []string{"time"}, Grain: "day"},
		{Measures: []string{"mrr", "subscribers"}, By: []string{"product_id"}},
		{Measures: []string{"mrr", "subscribers"}, By: []string{"time", "product_id"}, Grain: "week"},
		{Measures: []string{"mrr_movements", "subscriber_movements"}, By: []string{"time", "movement"}, Grain: "month"},
		{Measures: []string{"subscriber_churn_rate", "ltv"}, By: []string{"time"}, Grain: "month"},
		{Measures: []string{"subscriber_churn_rate"}, By: []string{"churn_type"}},
		{Measures: []string{"nrr", "grr"}, By: []string{"time"}, Grain: "month"},
		{Measures: []string{"nrr", "grr"}},
		{Measures: []string{"cohort_retention"}, By: []string{"cohort", "months_since_start", "basis"}},
		{Measures: []string{"scheduled_mrr"}, By: []string{"time", "movement"}, Grain: "week", Range: &billing.MetricsRange{Next: "12w"}},
		{Measures: []string{"cancellations"}, By: []string{"time", "cancel_type", "reversed"}, Grain: "day"},
		{Measures: []string{"mrr_at_risk"}, By: []string{"time", "risk_reason"}, Grain: "day"},
		{Measures: []string{"mrr_at_risk", "at_risk_recovery_rate"}},
	}
	covered := map[string]bool{}
	for _, q := range queries {
		if q.Range == nil {
			q.Range = window
		}
		plan, refused := metrics.ValidateIn(&q, a.clock.Now(), time.UTC)
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
			for _, node := range plans[0].Plan.all() {
				require.NotEqual(t, "mrr_movements", node.Relation, "%v reads the movements: %s", q.Measures, explained)
				require.False(t, node.Type == "Seq Scan" && node.Relation == "subscriptions", "%v scans subscriptions: %s", q.Measures, explained)
			}
		}
		for _, m := range q.Measures {
			covered[m] = true
		}
	}
	for _, m := range []string{"mrr", "subscribers", "arpa", "ltv", "mrr_movements", "subscriber_movements", "scheduled_mrr", "cancellations",
		"subscriber_churn_rate", "nrr", "grr", "cohort_retention", "mrr_at_risk", "at_risk_recovery_rate"} {
		require.True(t, covered[m], m)
	}
}

// planNode is a node of an EXPLAIN (FORMAT JSON) plan.
type planNode struct {
	Type     string     `json:"Node Type"`
	Relation string     `json:"Relation Name"`
	Plans    []planNode `json:"Plans"`
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
