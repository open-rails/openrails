//go:build integration

package critical_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/integrations/basistheory"
	"github.com/open-rails/openrails/internal/nmimock"
	"github.com/stretchr/testify/require"
)

// Monthly MRR of 10.00 a week: 52 weeks over 12 months.
const weeklyTenMRR = 43_333_333

// mrrRiskOf is the subscription's risk as the merchant routes serve it.
func mrrRiskOf(t *testing.T, client *openrails.Client, id billing.SubscriptionID) *billing.MRRRisk {
	t.Helper()
	sub, err := client.GetSubscription(t.Context(), id)
	require.NoError(t, err)
	return sub.MRRRisk
}

// riskCases are the subscription's risk cases, oldest first: each entry
// reason, outcome and the reasons of its episodes in order.
func riskCases(t *testing.T, w *world, client *openrails.Client, id billing.SubscriptionID) []string {
	t.Helper()
	episodes := pgx.Identifier{w.schema, "mrr_risk_episodes"}.Sanitize()
	rows, err := w.pool.Query(t.Context(), `SELECT c.entry_reason || ' ' || COALESCE(c.outcome, 'open') || ' ['
		|| (SELECT string_agg(e.reason, ' ' ORDER BY e.entered_at, e.id) FROM `+episodes+` e WHERE e.merchant_id = c.merchant_id AND e.case_id = c.id) || ']'
		FROM `+pgx.Identifier{w.schema, "mrr_risk_cases"}.Sanitize()+` c WHERE c.merchant_id = $1 AND c.subscription_id = $2 ORDER BY c.opened_at, c.id`,
		client.MerchantID().UUID(), id.UUID())
	require.NoError(t, err)
	out, err := pgx.CollectRows(rows, pgx.RowTo[string])
	require.NoError(t, err)
	return out
}

// riskWindow is a metrics range ending at to.
func riskWindow(from, to time.Time) *billing.MetricsRange {
	return &billing.MetricsRange{From: from.UTC().Format(time.RFC3339), To: to.UTC().Format(time.RFC3339)}
}

// atRiskMRR is mrr_at_risk at the end of window per currency, or per
// currency and reason when by is risk_reason.
func atRiskMRR(t *testing.T, client *openrails.Client, window *billing.MetricsRange, by ...string) map[string]int64 {
	t.Helper()
	out := map[string]int64{}
	for _, row := range metricTable(t, client, billing.QueryMetricsParams{Measures: []string{"mrr_at_risk"}, By: by, Range: window}) {
		key := fmt.Sprint(row["currency"])
		if len(by) > 0 {
			key += " " + fmt.Sprint(row["risk_reason"])
		}
		if n := whole(t, row["mrr_at_risk"]); n != 0 {
			out[key] = n
		}
	}
	return out
}

// recoveryRate is at_risk_recovery_rate over window per currency.
func recoveryRate(t *testing.T, client *openrails.Client, window *billing.MetricsRange) map[string]float64 {
	t.Helper()
	out := map[string]float64{}
	for _, row := range metricTable(t, client, billing.QueryMetricsParams{Measures: []string{"at_risk_recovery_rate"}, Range: window}) {
		var rate float64
		switch v := row["at_risk_recovery_rate"].(type) {
		case nil:
			continue
		case float64:
			rate = v
		case json.Number:
			f, err := v.Float64()
			require.NoError(t, err)
			rate = f
		default:
			f, err := strconv.ParseFloat(fmt.Sprint(v), 64)
			require.NoError(t, err)
			rate = f
		}
		out[fmt.Sprint(row["currency"])] = rate
	}
	return out
}

// saveExpiringCard is v.saveCard with the card's expiry.
func (v *vaultCards) saveExpiringCard(t *testing.T, bearer, number, last4 string, month, year int) billing.PaymentMethod {
	t.Helper()
	v.journal.core.AddNumber(number, nmimock.Card{Brand: "visa", Last4: last4})
	token := v.vendor.collectCard(merchantBTTenant, number, basistheory.CardDetails{Brand: "visa", Last4: last4, ExpirationMonth: month, ExpirationYear: year})
	status, raw := v.request(t, http.MethodPost, "/v1/me/payment-methods", bearer, billing.CreatePaymentMethodParams{PSPID: v.psp, Token: token})
	require.Equal(t, http.StatusCreated, status, string(raw))
	var method billing.PaymentMethod
	require.NoError(t, json.Unmarshal(raw, &method))
	return method
}

func TestMRRRisk(t *testing.T) {
	t.Run("OR-J42-S11/a-decline-puts-mrr-at-risk-as-past-due-payment-recovers-it-exhaustion-churns-it", func(t *testing.T) {
		a, p, sub := purchasedMembership(t, 7*24)
		enableLifecycleMaintenance(t, a)
		began := a.clock.Now()
		due := *sub.CurrentPeriodEndsAt
		p.core.SetDecline("4242", "insufficient_funds")
		a.clock.Advance(due.Sub(a.clock.Now()))
		fleet := startFleet(t, a.world)
		runRegistered(t, fleet, "openrails.billing_pass", 2)
		subscriptionState(t, a, sub.ID, "past_due")
		risk := mrrRiskOf(t, a.alpha, sub.ID)
		require.NotNil(t, risk, "a declined renewal puts the subscription's MRR at risk")
		require.Equal(t, []billing.MRRRiskReason{billing.MRRRiskPastDue}, risk.Reasons)
		require.Equal(t, billing.MRRRiskPastDue, risk.EntryReason)
		listed, err := a.alpha.ListSubscriptions(t.Context(), billing.SubscriptionListParams{AtRisk: true})
		require.NoError(t, err)
		require.Len(t, listed.Items, 1)
		require.Equal(t, sub.ID, listed.Items[0].ID)
		require.NotNil(t, listed.Items[0].MRRRisk)
		none, err := a.alpha.ListSubscriptions(t.Context(), billing.SubscriptionListParams{RiskReason: billing.MRRRiskNeedsPaymentMethod})
		require.NoError(t, err)
		require.Empty(t, none.Items)
		now := riskWindow(began, a.clock.Now().Add(time.Second))
		require.Equal(t, map[string]int64{"USD": weeklyTenMRR}, atRiskMRR(t, a.alpha, now))
		require.Equal(t, map[string]int64{"USD past_due": weeklyTenMRR}, atRiskMRR(t, a.alpha, now, "risk_reason"))
		require.Empty(t, atRiskMRR(t, a.alpha, riskWindow(began, due.Add(-time.Hour))), "nothing was at risk before the renewal")

		// The retry pays: the case is recovered.
		p.core.SetDecline("4242", "")
		a.clock.Advance(24 * time.Hour)
		runRegistered(t, fleet, "openrails.billing_pass", 2)
		renewalSettled(t, a, p, sub, due)
		require.Nil(t, mrrRiskOf(t, a.alpha, sub.ID))
		require.Equal(t, []string{"past_due recovered [past_due]"}, riskCases(t, a.world, a.alpha, sub.ID))

		// The next renewal declines through its last retry: the case churns.
		current, err := a.alpha.GetSubscription(t.Context(), sub.ID)
		require.NoError(t, err)
		p.core.SetDecline("4242", "insufficient_funds")
		a.clock.Advance(current.CurrentPeriodEndsAt.Sub(a.clock.Now()))
		runRegistered(t, fleet, "openrails.billing_pass", 2)
		subscriptionState(t, a, sub.ID, "past_due")
		for range 3 {
			a.clock.Advance(24 * time.Hour)
			runRegistered(t, fleet, "openrails.billing_pass", 2)
		}
		subscriptionState(t, a, sub.ID, "canceled")
		require.Equal(t, []string{"past_due recovered [past_due]", "past_due churned [past_due]"}, riskCases(t, a.world, a.alpha, sub.ID))
		var voluntary bool
		require.NoError(t, a.pool.QueryRow(t.Context(), "SELECT voluntary FROM "+pgx.Identifier{a.schema, "mrr_risk_cases"}.Sanitize()+
			" WHERE merchant_id = $1 AND subscription_id = $2 AND outcome = 'churned'", a.alpha.MerchantID().UUID(), sub.ID.UUID()).Scan(&voluntary))
		require.False(t, voluntary, "exhausted dunning is involuntary churn")
		window := riskWindow(began, a.clock.Now().Add(time.Second))
		require.Empty(t, atRiskMRR(t, a.alpha, window))
		require.Equal(t, map[string]float64{"USD": 0.5}, recoveryRate(t, a.alpha, window), "one case recovered, one churned, of the same MRR")
	})

	t.Run("OR-J42-S12/a-non-retryable-decline-needs-a-payment-method-a-card-update-moves-it-to-past-due-then-paid", func(t *testing.T) {
		a, p, sub := purchasedMembership(t, 7*24)
		due := *sub.CurrentPeriodEndsAt
		p.core.SetDecline("4242", "lost_card")
		a.clock.Advance(due.Sub(a.clock.Now()))
		fleet := startFleet(t, a.world)
		runRegistered(t, fleet, "openrails.billing_pass", 2)
		subscriptionState(t, a, sub.ID, "awaiting_method")
		risk := mrrRiskOf(t, a.alpha, sub.ID)
		require.NotNil(t, risk)
		require.Equal(t, []billing.MRRRiskReason{billing.MRRRiskNeedsPaymentMethod}, risk.Reasons)
		waiting, err := a.alpha.ListSubscriptions(t.Context(), billing.SubscriptionListParams{RiskReason: billing.MRRRiskNeedsPaymentMethod})
		require.NoError(t, err)
		require.Len(t, waiting.Items, 1)

		// A new card resumes dunning: past due, in the same case.
		method := saveMethod(t, a, p, a.aliceToken, "5556")
		status, raw := a.request(t, http.MethodPut, "/v1/me/subscriptions/"+sub.ID.String()+"/payment-method", a.aliceToken, billing.SetSubscriptionPaymentMethodParams{PaymentMethodID: &method.ID})
		require.Equal(t, http.StatusOK, status, string(raw))
		subscriptionState(t, a, sub.ID, "past_due")
		risk = mrrRiskOf(t, a.alpha, sub.ID)
		require.NotNil(t, risk)
		require.Equal(t, []billing.MRRRiskReason{billing.MRRRiskPastDue}, risk.Reasons)
		require.Equal(t, billing.MRRRiskNeedsPaymentMethod, risk.EntryReason, "the case keeps the reason it entered with")

		a.clock.Advance(24 * time.Hour)
		runRegistered(t, fleet, "openrails.billing_pass", 2)
		subscriptionState(t, a, sub.ID, "active")
		require.Nil(t, mrrRiskOf(t, a.alpha, sub.ID))
		require.Equal(t, []string{"needs_payment_method recovered [needs_payment_method past_due]"}, riskCases(t, a.world, a.alpha, sub.ID))
	})

	t.Run("OR-J42-S13/a-card-expiring-before-renewal-enters-on-the-period-change-and-leaves-on-an-account-updater-refresh", func(t *testing.T) {
		// A month boundary falls between the first and second period ends:
		// the card expires at it. Everything happens months ago, so every
		// payment is in the database's past.
		today := time.Now().UTC()
		boundary := time.Date(today.Year(), today.Month()-3, 1, 0, 0, 0, 0, time.UTC)
		v := newVaultCards(t, false, true, func(w *world) { w.clock.Advance(boundary.Add(-10 * 24 * time.Hour).Sub(w.clock.Now())) })
		expiry := boundary.Add(-24 * time.Hour)
		card := v.saveExpiringCard(t, v.aliceToken, "4111111111111111", "1111", int(expiry.Month()), expiry.Year())
		sub := v.subscribe(t, v.aliceToken, "expiring", 7*24, 10_000_000, card)
		require.True(t, sub.CurrentPeriodEndsAt.Before(boundary))
		require.Nil(t, mrrRiskOf(t, v.alpha, sub.ID), "the card covers the first renewal")
		fleet := startVaultFleet(t, v)
		checkAhead(t, v, fleet, 1)

		// The account updater has a new expiry once the renewal is paid.
		v.vendor.reissue(v.token(t, "1111"), btReissue{code: basistheory.AUUpdatedExpDate,
			card: basistheory.CardDetails{Brand: "visa", Last4: "1111", ExpirationMonth: 11, ExpirationYear: 2031}})
		v.clock.Advance(sub.CurrentPeriodEndsAt.Sub(v.clock.Now()))
		runRegistered(t, fleet, "openrails.billing_pass", 2)
		require.Eventually(t, func() bool {
			current, err := v.alpha.GetSubscription(t.Context(), sub.ID)
			return err == nil && current.CurrentPeriodEndsAt != nil && current.CurrentPeriodEndsAt.After(boundary)
		}, 20*time.Second, 20*time.Millisecond, "the renewal moves the period past the card's expiry")
		checkAhead(t, v, fleet, 2)
		require.Eventually(t, func() bool {
			year := v.card(t, card).Card.ExpYear
			return year != nil && *year == 2031
		}, 20*time.Second, 20*time.Millisecond, "the updater refreshes the card's expiry")
		runRegistered(t, fleet, "openrails.billing_pass", 1)
		require.Eventually(t, func() bool { return mrrRiskOf(t, v.alpha, sub.ID) == nil }, 20*time.Second, 20*time.Millisecond)
		require.Equal(t, []string{"card_expiring recovered [card_expiring]"}, riskCases(t, v.world, v.alpha, sub.ID))
	})

	t.Run("OR-J42-S14/unverified-enters-and-leaves", func(t *testing.T) {
		a, accounts, sub := stripeMembership(t)
		old := accounts.accounts["sk_test_old"].core
		remote := *sub.RailSubscriptionID
		// Stripe renewed it; OpenRails heard nothing, so the period's end
		// leaves the row unverified until a read confirms it.
		old.RenewSubscription(remote, true)
		a.clock.Advance(sub.CurrentPeriodEndsAt.Sub(a.clock.Now()) + 49*time.Hour)
		fleet := startFleet(t, a.world)
		runRegistered(t, fleet, "openrails.billing_pass", 1)
		require.Eventually(t, func() bool {
			current, err := a.alpha.GetSubscription(t.Context(), sub.ID)
			return err == nil && current.Status == billing.SubscriptionActive && current.CurrentPeriodStartsAt.Equal(*sub.CurrentPeriodEndsAt)
		}, 20*time.Second, 20*time.Millisecond, "the read's renewal verifies it")
		require.Nil(t, mrrRiskOf(t, a.alpha, sub.ID))
		require.Equal(t, []string{"unverified recovered [unverified]"}, riskCases(t, a.world, a.alpha, sub.ID))
	})

	t.Run("OR-J42-S15/usage-delinquency-puts-the-customers-mrr-at-risk-and-clears-it", func(t *testing.T) {
		a, p, customer := usageBillingWorld(t, true, true)
		sub := nmiMembershipOn(t, a, p, catalog.Field[int]{})
		oneUsage(t, a.alpha, usageInput(customer, "overdue-work", 3))
		fleet := startFleet(t, a.world)
		postUsageDay(t, a.world, fleet)
		runUsageDue(t, fleet)
		a.clock.Advance(24 * time.Hour)
		runRegistered(t, fleet, "openrails.billing_pass", 1)
		order := usageOrderOf(t, a, customer)
		require.True(t, order.Usage.Delinquent)
		// The delinquency queued the customer; the next pass re-evaluates.
		runRegistered(t, fleet, "openrails.billing_pass", 1)
		require.Eventually(t, func() bool {
			risk := mrrRiskOf(t, a.alpha, sub.ID)
			return risk != nil && len(risk.Reasons) == 1 && risk.Reasons[0] == billing.MRRRiskDelinquent
		}, 20*time.Second, 20*time.Millisecond, "the customer's subscription MRR in the currency is at risk")
		var overdue int64
		require.NoError(t, a.pool.QueryRow(t.Context(), "SELECT overdue_amount FROM "+pgx.Identifier{a.schema, "mrr_risk_episodes"}.Sanitize()+
			" WHERE merchant_id = $1 AND subscription_id = $2 AND reason = 'delinquent'", a.alpha.MerchantID().UUID(), sub.ID.UUID()).Scan(&overdue))
		require.Positive(t, overdue, "the overdue amount sits beside the at-risk MRR")

		_, err := a.alpha.RecordExternalPayment(t.Context(), billing.RecordExternalPaymentParams{OrderID: order.ID, Amount: 30_000_000, TransactionID: "bank-cleared"})
		require.NoError(t, err)
		runRegistered(t, fleet, "openrails.billing_pass", 1)
		require.False(t, usageOrderOf(t, a, customer).Usage.Delinquent)
		runRegistered(t, fleet, "openrails.billing_pass", 1)
		require.Eventually(t, func() bool { return mrrRiskOf(t, a.alpha, sub.ID) == nil }, 20*time.Second, 20*time.Millisecond)
		require.Equal(t, []string{"delinquent recovered [delinquent]"}, riskCases(t, a.world, a.alpha, sub.ID))
	})
}

// usageOrderOf is the customer's one usage order.
func usageOrderOf(t *testing.T, a *authorityWorld, customer billing.CustomerID) billing.Order {
	t.Helper()
	rows, err := a.alpha.ListOrders(t.Context(), billing.OrderListParams{CustomerID: customer})
	require.NoError(t, err)
	var usage []billing.Order
	for _, order := range rows.Items {
		if order.Usage != nil {
			usage = append(usage, order)
		}
	}
	require.Len(t, usage, 1)
	return usage[0]
}
