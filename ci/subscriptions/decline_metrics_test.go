//go:build e2e && integration

package subscriptions_test

import (
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
)

// metricRows runs one metrics query over [since, now] and returns its rows by
// column name.
func (w *world) metricRows(since time.Time, measures, by []string, filters map[string][]string) []map[string]any {
	w.t.Helper()
	body := map[string]any{"measures": measures, "range": map[string]string{
		"from": since.UTC().Format(time.RFC3339), "to": w.clock.Now().Add(time.Second).UTC().Format(time.RFC3339)}}
	if len(by) > 0 {
		body["by"] = by
		if slices.Contains(by, "time") {
			body["grain"] = "month"
		}
	}
	if filters != nil {
		body["filters"] = filters
	}
	status, res := w.staffJSON(http.MethodPost, "/v1/merchant/metrics/query", body)
	require.Equal(w.t, http.StatusOK, status, "%v %v: %v", measures, by, res)
	var names []string
	for _, col := range res["columns"].([]any) {
		names = append(names, col.(map[string]any)["name"].(string))
	}
	var out []map[string]any
	for _, r := range res["rows"].([]any) {
		row := map[string]any{}
		for i, cell := range r.([]any) {
			row[names[i]] = cell
		}
		out = append(out, row)
	}
	return out
}

// metricTotal is one measure over the whole range.
func (w *world) metricTotal(since time.Time, measure string, filters map[string][]string) float64 {
	w.t.Helper()
	rows := w.metricRows(since, []string{measure}, nil, filters)
	require.Len(w.t, rows, 1)
	return decimal(w.t, rows[0][measure])
}

func decimal(t *testing.T, v any) float64 {
	t.Helper()
	switch n := v.(type) {
	case float64:
		return n
	case string:
		var out float64
		_, err := fmt.Sscan(n, &out)
		require.NoError(t, err)
		return out
	case nil:
		return 0
	}
	t.Fatalf("not a number: %T %v", v, v)
	return 0
}

// A buyer mistypes the security code, then pays; the membership's first
// renewal declines twice and is collected on the second retry. The decline
// metrics count it all: attempts, the checkout and the rebill cycle (#1116).
func TestDeclineMetrics(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	since := w.clock.Now().Add(-time.Hour)
	price := w.membership("content:members", 9_990_000)
	h := hostedPay{w: w, c: w.newCustomer(), tp: embedded, price: price.ID}
	_, err := h.pay("pay-cvc", openrails.CheckoutPaymentOptions{PaymentToken: w.nmi.Tokenize(card{Brand: "visa", Last4: "0005", Decline: "200", CVV: "N"})})
	require.ErrorIs(t, err, openrails.ErrPaymentRefused)
	session, err := h.pay("pay-fixed", openrails.CheckoutPaymentOptions{PaymentToken: w.nmi.Tokenize(visa)})
	require.NoError(t, err)
	w.settle()

	subs, err := w.client[embedded].ListSubscriptions(t.Context(), openrails.SubscriptionFilter{CustomerID: h.c.id})
	require.NoError(t, err)
	require.Len(t, subs.Data, 1, "%v", session.Status)
	sub := subs.Data[0].ID
	w.nmi.SetDecline(visa.Last4, "202")
	w.advance(subs.Data[0].CurrentPeriodEndsAt.Sub(w.clock.Now()) + time.Second)
	w.runRenewals()
	for i := range 2 {
		if i == 1 {
			w.nmi.SetDecline(visa.Last4, "")
		}
		s := w.subscription(embedded, sub)
		require.NotNil(t, s.NextRetryAt)
		w.advance(s.NextRetryAt.Sub(w.clock.Now()) + time.Second)
		w.runRenewals()
	}
	require.Equal(t, "active", w.subscription(embedded, sub).Status)
	w.advance(time.Hour)

	newCard := map[string][]string{"kind": {"verify", "initial"}, "card_entry": {"new"}}
	require.Equal(t, 3.0, w.metricTotal(since, "attempts", newCard), "two verifications and the charge")
	require.Equal(t, 1.0, w.metricTotal(since, "failed_attempts", newCard))
	require.InDelta(t, 1.0/3, w.metricTotal(since, "attempt_failure_rate", newCard), 1e-9, "the new-card decline rate per attempt")
	require.Equal(t, 6.0, w.metricTotal(since, "attempts", nil), "and the renewal's three")
	byReason := map[string]float64{}
	for _, r := range w.metricRows(since, []string{"failed_attempts"}, []string{"reason"}, nil) {
		byReason[r["reason"].(string)] = decimal(t, r["failed_attempts"])
	}
	require.Equal(t, map[string]float64{"incorrect_cvc": 1, "insufficient_funds": 2}, byReason)

	require.Equal(t, 1.0, w.metricTotal(since, "checkouts", nil))
	require.Equal(t, 0.0, w.metricTotal(since, "failed_checkouts", nil))
	require.Equal(t, 0.0, w.metricTotal(since, "checkout_failure_rate", nil), "the buyer got through")
	require.Equal(t, 1.0, w.metricTotal(since, "checkout_recovery_rate", nil))
	require.Equal(t, 3.0, w.metricTotal(since, "attempts_per_checkout", nil))

	require.Equal(t, 1.0, w.metricTotal(since, "rebills_due", nil))
	require.Equal(t, 0.0, w.metricTotal(since, "rebills_open", nil))
	require.Equal(t, 1.0, w.metricTotal(since, "rebill_first_failure_rate", nil))
	require.Equal(t, 1.0, w.metricTotal(since, "dunning_recovery_rate", nil))
	require.Equal(t, 1.0, w.metricTotal(since, "rebill_collection_rate", nil))
	curve := w.metricRows(since, []string{"dunning_recovered"}, []string{"owner", "recovered_by", "recovery_attempt", "first_outcome"}, nil)
	require.Len(t, curve, 1)
	require.Equal(t, []any{"engine", "dunning_retry", "3", "declined", 1.0},
		[]any{curve[0]["owner"], curve[0]["recovered_by"], curve[0]["recovery_attempt"], curve[0]["first_outcome"], decimal(t, curve[0]["dunning_recovered"])})

	// Every dimension of every new family compiles and runs.
	families := map[string][]string{
		"attempts":    {"currency", "rail", "rail_account", "owner", "kind", "card_entry", "source", "observed_via", "category", "reason", "response_code", "issuer_code", "avs_result", "cvv_result", "card_brand", "card_bin", "token_type"},
		"checkouts":   {"currency", "rail", "rail_account", "owner", "card_entry"},
		"rebills_due": {"currency", "rail", "rail_account", "owner", "first_outcome", "first_failure_category", "first_failure_reason", "miss_reason", "recovered_by", "recovery_attempt", "days_to_recover"},
	}
	for measure, dims := range families {
		for _, dim := range dims {
			require.NotEmpty(t, w.metricRows(since, []string{measure}, []string{"time", dim}, nil), "%s by %s", measure, dim)
		}
	}
}

// A declined renewal collected on the second retry reads back through the
// attempt and cycle APIs (#1116).
func TestAttemptAndCycleReads(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	e := enroll(t, w, "nmi", embedded)
	e.setDecline(visa.Last4, "insufficient_funds", "202")
	e.toPeriodEnd()
	w.runRenewals()
	for i := range 2 {
		if i == 1 {
			e.setDecline(visa.Last4, "", "")
		}
		s := w.subscription(embedded, e.sub)
		w.advance(s.NextRetryAt.Sub(w.clock.Now()) + time.Second)
		w.runRenewals()
	}
	c := w.client[embedded]
	ctx := t.Context()

	cycles, err := c.ListRebillCycles(ctx, openrails.RebillCycleFilter{SubscriptionID: e.sub})
	require.NoError(t, err)
	require.Len(t, cycles.Data, 1)
	cycle := cycles.Data[0]
	require.Equal(t, []string{"engine", "declined", "collected", "dunning_retry"}, []string{cycle.Owner, cycle.FirstOutcome, cycle.Outcome, cycle.RecoveredBy})
	require.NotNil(t, cycle.CollectedAt)
	for outcome, n := range map[string]int{"collected": 1, "open": 0, "lost": 0, "open,lost": 0, "lost,collected": 1} {
		page, err := c.ListRebillCycles(ctx, openrails.RebillCycleFilter{SubscriptionID: e.sub, Outcome: strings.Split(outcome, ",")})
		require.NoError(t, err)
		require.Len(t, page.Data, n, outcome)
	}

	full, err := c.GetRebillCycle(ctx, cycle.ID)
	require.NoError(t, err)
	require.Len(t, full.Attempts, 3)
	require.Equal(t, []string{"rebill", "dunning_retry", "dunning_retry"}, []string{full.Attempts[0].Kind, full.Attempts[1].Kind, full.Attempts[2].Kind})
	require.Equal(t, openrails.DeclineInsufficientFunds, full.Attempts[0].Reason)

	attempts, err := c.ListPaymentAttempts(ctx, openrails.PaymentAttemptFilter{CycleID: cycle.ID})
	require.NoError(t, err)
	require.Equal(t, int64(3), attempts.Total)
	require.Equal(t, full.Attempts[2].ID, attempts.Data[0].ID, "newest first")
	one, err := c.GetPaymentAttempt(ctx, attempts.Data[0].ID)
	require.NoError(t, err)
	require.Equal(t, "approved", one.Category)
	require.Equal(t, cycle.ID, *one.CycleID)

	declined, err := c.ListPaymentAttempts(ctx, openrails.PaymentAttemptFilter{Category: []string{"issuer_soft"}, Kind: []string{"dunning_retry"}})
	require.NoError(t, err)
	require.Len(t, declined.Data, 1)
	failed, err := c.ListPaymentAttempts(ctx, openrails.PaymentAttemptFilter{CycleID: cycle.ID, Kind: []string{"rebill", "dunning_retry"}, Category: []string{"issuer_soft", "issuer_hard"}})
	require.NoError(t, err)
	require.Len(t, failed.Data, 2, "the cycle's two declines")
}
