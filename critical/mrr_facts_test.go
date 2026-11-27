//go:build integration

package critical_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/merchant"
)

// MRR is read from its facts: a cancellation at period end is a movement
// dated at its end, written when it is decided and counted once that instant
// passes, with nothing run then.
func TestMRRFacts(t *testing.T) {
	t.Run("OR-J42-S34/a-cancel-at-period-end-counts-at-its-end-with-no-job-and-a-resume-before-then-removes-it", cancelCountsAtItsEnd)
	t.Run("OR-J42-S35/an-add-on-bought-before-the-end-turns-the-churn-into-contraction", addOnTurnsChurnIntoContraction)
	t.Run("OR-J42-S36/at-risk-mrr-follows-a-price-change-during-a-case", atRiskFollowsAPriceChange)
}

// usdOnly filters a metrics query to dollars.
var usdOnly = map[string][]string{"currency": {"USD"}}

// mrrThrough is a USD measure over the hour before to, read at the clock.
func mrrThrough(t *testing.T, a *authorityWorld, measure string, to time.Time) int64 {
	t.Helper()
	window := &billing.MetricsRange{From: to.Add(-time.Hour).Format(time.RFC3339), To: to.Format(time.RFC3339)}
	return whole(t, only(t, metricTable(t, a.alpha, billing.QueryMetricsParams{Measures: []string{measure}, Range: window, Filters: usdOnly}))[measure])
}

// subscriptionMRRRow is the subscription's MRR row as its writers left it:
// its level and its end.
func subscriptionMRRRow(t *testing.T, a *authorityWorld, sub billing.SubscriptionID) (int64, *time.Time) {
	t.Helper()
	var level int64
	var ends *time.Time
	require.NoError(t, a.pool.QueryRow(t.Context(), "SELECT mrr, ends_at FROM "+pgx.Identifier{a.schema, "subscription_mrr"}.Sanitize()+
		" WHERE merchant_id = $1 AND subscription_id = $2", a.alpha.MerchantID().UUID(), sub.UUID()).Scan(&level, &ends))
	return level, ends
}

func cancelCountsAtItsEnd(t *testing.T) {
	a, _, sub := purchasedMembership(t)
	alice := customerOf(a.alice)
	end := *sub.CurrentPeriodEndsAt
	began := a.clock.Now()
	customerCancel(t, a, sub.ID)

	// The churn is written now, dated at the end; MRR holds until then, even
	// read at an instant past it.
	moves := mrrMoves(t, a.world, a.alpha, alice)
	require.Equal(t, []string{mrrKind("USD", "new", dailyTenMRR), mrrKind("USD", "churn", -dailyTenMRR)}, mrrKinds(moves))
	require.True(t, moves[1].Scheduled)
	require.True(t, moves[1].OccurredAt.Equal(end), "dated at the end, %s", moves[1].OccurredAt)
	require.Equal(t, -1, moves[1].Logo)
	require.NotNil(t, moves[1].Voluntary)
	require.True(t, *moves[1].Voluntary)
	require.EqualValues(t, dailyTenMRR, mrrThrough(t, a, "mrr", a.clock.Now().Add(time.Second)))
	require.EqualValues(t, dailyTenMRR, mrrThrough(t, a, "mrr", end.Add(time.Hour)), "a movement ahead counts once its date passes")
	ahead := &billing.MetricsRange{From: a.clock.Now().Format(time.RFC3339), To: end.Add(time.Second).Format(time.RFC3339)}
	scheduled := split(t, metricTable(t, a.alpha, billing.QueryMetricsParams{Measures: []string{"scheduled_mrr"}, By: []string{"movement"}, Range: ahead, Filters: usdOnly}),
		"movement", "scheduled_mrr")
	require.Equal(t, map[string]int64{"churn": -dailyTenMRR}, scheduled)

	// A resume before the end removes it.
	a.clock.Advance(time.Hour)
	_, err := a.alpha.ResumeSubscription(t.Context(), sub.ID)
	require.NoError(t, err)
	require.Equal(t, []string{mrrKind("USD", "new", dailyTenMRR)}, mrrKinds(mrrMoves(t, a.world, a.alpha, alice)))
	require.EqualValues(t, dailyTenMRR, mrrThrough(t, a, "mrr", end.Add(2*time.Hour)))

	// Canceled again, the end passes with nothing run: MRR and the customer
	// are gone from that instant, while the subscription's row is as the
	// cancel left it.
	a.clock.Advance(time.Hour)
	customerCancel(t, a, sub.ID)
	a.clock.Advance(end.Sub(a.clock.Now()) + time.Hour)
	level, ends := subscriptionMRRRow(t, a, sub.ID)
	require.EqualValues(t, dailyTenMRR, level, "no job touched the subscription")
	require.NotNil(t, ends)
	require.True(t, ends.Equal(end))
	require.Zero(t, mrrThrough(t, a, "mrr", a.clock.Now()))
	require.Zero(t, mrrThrough(t, a, "subscribers", a.clock.Now()))
	require.EqualValues(t, dailyTenMRR, mrrThrough(t, a, "mrr", end), "MRR holds until the end")
	require.Equal(t, billing.SubscriptionMRR{Amount: 0, Currency: "USD"}, mrrOf(t, a.alpha, sub.ID))
	kinds := split(t, metricTable(t, a.alpha, billing.QueryMetricsParams{Measures: []string{"mrr_movements"}, By: []string{"movement"},
		Range: &billing.MetricsRange{From: began.Add(-time.Hour).Format(time.RFC3339), To: a.clock.Now().Format(time.RFC3339)}, Filters: usdOnly}), "movement", "mrr_movements")
	require.Equal(t, map[string]int64{"new": dailyTenMRR, "churn": -dailyTenMRR}, kinds)
}

func addOnTurnsChurnIntoContraction(t *testing.T) {
	a, provider, sub := purchasedMembership(t)
	alice := customerOf(a.alice)
	end := *sub.CurrentPeriodEndsAt
	began := a.clock.Now()
	weekly := mrrPlan(t, a.alpha, "s35-add-on", billing.CreatePriceParams{UnitAmount: 7_000_000, Currency: "USD", BillingIntervalHours: ptr(168), AccessDurationHours: ptr(168)})

	customerCancel(t, a, sub.ID)
	moves := mrrMoves(t, a.world, a.alpha, alice)
	require.Equal(t, []string{mrrKind("USD", "new", dailyTenMRR), mrrKind("USD", "churn", -dailyTenMRR)}, mrrKinds(moves), "alone, its end is churn")
	require.Equal(t, -1, moves[1].CustomerLogo)

	// An add-on bought before the end: the customer keeps MRR past it.
	a.clock.Advance(time.Hour)
	buyMRRPlan(t, a, provider, weekly, 7_000_000, "s35-add-on")
	moves = mrrMoves(t, a.world, a.alpha, alice)
	require.Equal(t, []string{mrrKind("USD", "new", dailyTenMRR), mrrKind("USD", "expansion", weeklyMRR), mrrKind("USD", "contraction", -dailyTenMRR)},
		mrrKinds(moves))
	require.True(t, moves[2].Scheduled)
	require.True(t, moves[2].OccurredAt.Equal(end))
	require.Equal(t, 0, moves[2].Logo)
	require.Equal(t, 0, moves[2].CustomerLogo)
	require.Nil(t, moves[2].Voluntary)
	require.Equal(t, int64(weeklyMRR), moves[2].After)

	// The end passes with nothing run: she stays a customer, at the add-on.
	a.clock.Advance(end.Sub(a.clock.Now()) + time.Hour)
	require.EqualValues(t, weeklyMRR, mrrThrough(t, a, "mrr", a.clock.Now()))
	require.EqualValues(t, 1, mrrThrough(t, a, "subscribers", a.clock.Now()))
	window := &billing.MetricsRange{From: began.Add(-time.Hour).Format(time.RFC3339), To: a.clock.Now().Format(time.RFC3339)}
	kinds := split(t, metricTable(t, a.alpha, billing.QueryMetricsParams{Measures: []string{"mrr_movements"}, By: []string{"movement"}, Range: window, Filters: usdOnly}),
		"movement", "mrr_movements")
	require.Equal(t, map[string]int64{"new": dailyTenMRR, "expansion": weeklyMRR, "contraction": -dailyTenMRR}, kinds, "no churn")
}

func atRiskFollowsAPriceChange(t *testing.T) {
	a, _, sub := purchasedMembershipWith(t, 7*24)
	twenty, err := a.alpha.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: sub.ProductID, Key: "s36-twenty", UnitAmount: 20_000_000, Currency: "USD",
		BillingIntervalHours: ptr(168), AccessDurationHours: ptr(168)})
	require.NoError(t, err)
	var twentyID string
	require.NoError(t, a.pool.QueryRow(t.Context(), "SELECT id::text FROM "+pgx.Identifier{a.schema, "prices"}.Sanitize()+" WHERE merchant_id = $1 AND public_id = $2",
		a.alpha.MerchantID().UUID(), twenty.ID.String()).Scan(&twentyID))
	// 20.00 a week, 52 over 12 months, rounded once.
	const weeklyTwentyMRR = 86_666_667
	began := a.clock.Now()
	declined, repriced, recovered := began.Add(time.Hour), began.Add(2*time.Hour), began.Add(3*time.Hour)

	// The subscription's own writes, each as its writer dates it: past due,
	// moved to the larger price while past due, then paid.
	database, err := producerDB(a.pool, a.schema)
	require.NoError(t, err)
	write := func(at time.Time, set string, args ...any) {
		t.Helper()
		require.NoError(t, database.MerchantTx(merchant.WithID(t.Context(), a.alpha.MerchantID()), func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, "UPDATE billing.subscriptions SET "+set+", lifecycle_rev = lifecycle_rev + 1, updated_at = $3 WHERE merchant_id = $1 AND id = $2",
				append([]any{a.alpha.MerchantID().UUID(), sub.ID.UUID(), at}, args...)...)
			return err
		}))
	}
	write(declined, "status = 'past_due'")
	write(repriced, "price_id = $4::uuid", twentyID)
	write(recovered, "status = 'active'")
	a.clock.Advance(4 * time.Hour)

	at := func(edge time.Time, by ...string) map[string]int64 {
		t.Helper()
		return atRiskMRR(t, a.alpha, riskWindow(began, edge), by...)
	}
	require.Equal(t, map[string]int64{"USD": weeklyTenMRR}, at(declined.Add(30*time.Minute)))
	require.Equal(t, map[string]int64{"USD": weeklyTwentyMRR}, at(repriced.Add(30*time.Minute)), "at risk at the new price")
	require.Equal(t, map[string]int64{"USD past_due": weeklyTwentyMRR}, at(repriced.Add(30*time.Minute), "risk_reason"))
	require.Empty(t, at(recovered.Add(30*time.Minute)))
	require.Equal(t, []string{"past_due recovered [past_due past_due]"}, riskCases(t, a.world, a.alpha, sub.ID), "the change ends its episode at the old amount and begins one at the new")
	rows, err := a.pool.Query(t.Context(), "SELECT mrr_at_entry FROM "+pgx.Identifier{a.schema, "mrr_risk_episodes"}.Sanitize()+
		" WHERE merchant_id = $1 AND subscription_id = $2 AND reason = 'any' ORDER BY entered_at", a.alpha.MerchantID().UUID(), sub.ID.UUID())
	require.NoError(t, err)
	amounts, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	require.NoError(t, err)
	require.Equal(t, []int64{weeklyTenMRR, weeklyTwentyMRR}, amounts, "the case's MRR whatever its reasons, at each amount")
	require.Equal(t, map[string]float64{"USD": 1}, recoveryRate(t, a.alpha, riskWindow(began, a.clock.Now())))
}
