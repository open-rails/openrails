//go:build integration

package critical_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/integrations/basistheory"
	"github.com/open-rails/openrails/internal/nmimock"
	"github.com/open-rails/openrails/internal/stripemock"
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
// reason, outcome and the reasons of its episodes in order (any, each case's
// own, aside).
func riskCases(t *testing.T, w *world, client *openrails.Client, id billing.SubscriptionID) []string {
	t.Helper()
	episodes := pgx.Identifier{w.schema, "mrr_risk_episodes"}.Sanitize()
	rows, err := w.pool.Query(t.Context(), `SELECT c.entry_reason || ' ' || COALESCE(c.outcome, 'open') || ' ['
		|| (SELECT string_agg(e.reason, ' ' ORDER BY e.entered_at, e.id) FROM `+episodes+` e WHERE e.merchant_id = c.merchant_id AND e.case_id = c.id AND e.reason <> 'any') || ']'
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
	status, raw := v.request(t, http.MethodPost, "/v1/me/payment-methods", bearer, billing.CreatePaymentMethodParams{PSPID: v.psp, Token: token, BillingDetails: billedTo("US", "10001")})
	require.Equal(t, http.StatusCreated, status, string(raw))
	var method billing.PaymentMethod
	require.NoError(t, json.Unmarshal(raw, &method))
	return method
}

// closedCase is the subscription's one closed risk case: when it closed, its
// outcome and whether its churn was voluntary.
func closedCase(t *testing.T, w *world, client *openrails.Client, id billing.SubscriptionID) (time.Time, string, *bool) {
	t.Helper()
	var at time.Time
	var outcome string
	var voluntary *bool
	require.NoError(t, w.pool.QueryRow(t.Context(), "SELECT closed_at, outcome, voluntary FROM "+pgx.Identifier{w.schema, "mrr_risk_cases"}.Sanitize()+
		" WHERE merchant_id = $1 AND subscription_id = $2 AND closed_at IS NOT NULL", client.MerchantID().UUID(), id.UUID()).Scan(&at, &outcome, &voluntary))
	return at, outcome, voluntary
}

// setDefaultCard makes method the bearer's default card in USD.
func setDefaultCard(t *testing.T, a *authorityWorld, bearer string, method billing.PaymentMethodID) {
	t.Helper()
	status, raw := a.request(t, http.MethodPut, "/v1/me/default-payment-methods/USD", bearer, billing.SetDefaultPaymentMethodParams{PaymentMethodID: method})
	require.Equal(t, http.StatusOK, status, string(raw))
}

// monthsAgo is the first of the month n months before now: a vault world's
// clock set before it keeps every payment in the database's past.
func monthsAgo(n int) time.Time {
	today := time.Now().UTC()
	return time.Date(today.Year(), today.Month()-time.Month(n), 1, 0, 0, 0, 0, time.UTC)
}

// expiringVaultCards is a vault world whose clock stands before boundary,
// with alice's card that expires there, the month before.
func expiringVaultCards(t *testing.T, boundary time.Time, before time.Duration) (*vaultCards, billing.PaymentMethod) {
	t.Helper()
	v := newVaultCards(t, false, false, func(w *world) { w.clock.Advance(boundary.Add(-before).Sub(w.clock.Now())) })
	expiry := boundary.Add(-24 * time.Hour)
	return v, v.saveExpiringCard(t, v.aliceToken, "4111111111111111", "1111", int(expiry.Month()), expiry.Year())
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

	t.Run("OR-J42-S13/a-card-expiring-before-renewal-is-at-risk-now-from-state-with-no-case-a-new-expiry-clears-it-at-once", func(t *testing.T) {
		boundary := monthsAgo(2)
		a, _, sub := purchasedMembershipWith(t, 7*24, func(w *world) { w.clock.Advance(boundary.Add(-3 * 24 * time.Hour).Sub(w.clock.Now())) })
		require.NotNil(t, sub.PaymentMethodID)
		require.True(t, sub.CurrentPeriodEndsAt.After(boundary))
		began := a.clock.Now()
		require.Nil(t, mrrRiskOf(t, a.alpha, sub.ID), "the card covers the renewal")
		expire := func(month, year int) {
			t.Helper()
			status, raw := a.request(t, http.MethodPatch, "/v1/me/payment-methods/"+sub.PaymentMethodID.String(), a.aliceToken,
				billing.UpdatePaymentMethodParams{ExpMonth: ptr(month), ExpYear: ptr(year)})
			require.Equal(t, http.StatusOK, status, string(raw))
		}

		// The card now expires before the renewal it pays for: read from
		// state at once, no job between.
		a.clock.Advance(time.Hour)
		expiry := boundary.Add(-24 * time.Hour)
		expire(int(expiry.Month()), expiry.Year())
		risk := mrrRiskOf(t, a.alpha, sub.ID)
		require.NotNil(t, risk)
		require.Equal(t, []billing.MRRRiskReason{billing.MRRRiskCardExpiring}, risk.Reasons)
		require.Equal(t, billing.MRRRiskCardExpiring, risk.EntryReason)
		listed, err := a.alpha.ListSubscriptions(t.Context(), billing.SubscriptionListParams{RiskReason: billing.MRRRiskCardExpiring})
		require.NoError(t, err)
		require.Len(t, listed.Items, 1)
		now := riskWindow(began, a.clock.Now().Add(time.Second))
		require.Equal(t, map[string]int64{"USD card_expiring": weeklyTenMRR}, atRiskMRR(t, a.alpha, now, "risk_reason"))
		require.Empty(t, riskCases(t, a.world, a.alpha, sub.ID), "card_expiring keeps no case")

		// A new expiry clears it at once; no past read holds it.
		a.clock.Advance(time.Hour)
		expire(12, 2035)
		require.Nil(t, mrrRiskOf(t, a.alpha, sub.ID))
		require.Empty(t, atRiskMRR(t, a.alpha, riskWindow(began, a.clock.Now().Add(time.Second))))
		require.Empty(t, atRiskMRR(t, a.alpha, riskWindow(began, a.clock.Now().Add(-30*time.Minute))), "a past read keeps the status reasons alone")
		require.Empty(t, riskCases(t, a.world, a.alpha, sub.ID))
	})

	t.Run("OR-J42-S14/unverified-enters-and-leaves", func(t *testing.T) {
		a, accounts, sub := stripeMembership(t)
		old := accounts.accounts["sk_test_old"].core
		remote := *sub.RailSubscriptionID
		// OpenRails heard nothing past the period's end: the row is unverified,
		// awaiting Stripe, and Stripe's read cannot tell.
		a.clock.Advance(sub.CurrentPeriodEndsAt.Sub(a.clock.Now()) + 49*time.Hour)
		fleet := startFleet(t, a.world)
		runRegistered(t, fleet, "openrails.billing_pass", 1)
		require.Eventually(t, func() bool {
			current, err := a.alpha.GetSubscription(t.Context(), sub.ID)
			return err == nil && current.Status == billing.SubscriptionUnverified && current.UnverifiedReason != nil && *current.UnverifiedReason == billing.UnverifiedUnconfirmed
		}, 20*time.Second, 20*time.Millisecond, "a read that cannot tell leaves it unconfirmed")
		risk := mrrRiskOf(t, a.alpha, sub.ID)
		require.NotNil(t, risk, "unconfirmed is at risk")
		require.Equal(t, []billing.MRRRiskReason{billing.MRRRiskUnverified}, risk.Reasons)
		// Stripe renews it; the hourly retry's read verifies it.
		old.RenewSubscription(remote, true)
		a.clock.Advance(time.Hour + time.Minute)
		runRegistered(t, fleet, "openrails.billing_pass", 1)
		require.Eventually(t, func() bool {
			current, err := a.alpha.GetSubscription(t.Context(), sub.ID)
			return err == nil && current.Status == billing.SubscriptionActive && current.CurrentPeriodStartsAt.Equal(*sub.CurrentPeriodEndsAt)
		}, 20*time.Second, 20*time.Millisecond, "the read's renewal verifies it")
		require.Nil(t, mrrRiskOf(t, a.alpha, sub.ID))
		require.Equal(t, []string{"unverified recovered [unverified]"}, riskCases(t, a.world, a.alpha, sub.ID))
	})

	t.Run("OR-J42-S15/usage-delinquency-puts-the-customers-mrr-at-risk-now-from-state-and-paying-clears-it", func(t *testing.T) {
		a, p, customer := usageBillingWorld(t, true)
		sub := nmiMembershipOn(t, a, p, catalog.Field[int]{})
		began := a.clock.Now()
		oneUsage(t, a.alpha, usageInput(customer, "overdue-work", 3))
		fleet := startFleet(t, a.world)
		postUsageMonth(t, a.world, fleet)
		runUsageDue(t, fleet)
		a.clock.Advance(24 * time.Hour)
		runRegistered(t, fleet, "openrails.billing_pass", 1)
		invoice := usageInvoiceOf(t, a, customer)
		require.True(t, invoice.Usage.Delinquent)
		// Read from state: the delinquency puts the subscription's MRR in the
		// currency at risk, with no case.
		risk := mrrRiskOf(t, a.alpha, sub.ID)
		require.NotNil(t, risk, "the customer's subscription MRR in the currency is at risk")
		require.Equal(t, []billing.MRRRiskReason{billing.MRRRiskDelinquent}, risk.Reasons)
		require.Equal(t, billing.MRRRiskDelinquent, risk.EntryReason)
		at := riskWindow(began, a.clock.Now().Add(time.Second))
		require.NotEmpty(t, atRiskMRR(t, a.alpha, at, "risk_reason"))
		require.Empty(t, riskCases(t, a.world, a.alpha, sub.ID), "delinquent keeps no case")

		_, err := a.alpha.RecordExternalPayment(t.Context(), billing.RecordExternalPaymentParams{InvoiceID: invoice.ID, Amount: 30_000_000, TransactionID: "bank-cleared"})
		require.NoError(t, err)
		runRegistered(t, fleet, "openrails.billing_pass", 1)
		require.False(t, usageInvoiceOf(t, a, customer).Usage.Delinquent)
		require.Nil(t, mrrRiskOf(t, a.alpha, sub.ID))
		require.Empty(t, riskCases(t, a.world, a.alpha, sub.ID))
	})

	t.Run("OR-J42-S17/a-cancel-at-period-end-while-at-risk-closes-the-case-churned-at-its-end-when-written-a-resume-reopens-it", func(t *testing.T) {
		// Alice's Stripe membership whose renewal Stripe failed: past due, at
		// risk, its new period's end ahead.
		pastDue := func(t *testing.T) (*trailWorld, billing.SubscriptionID, time.Time, time.Time) {
			t.Helper()
			tw := newTrailWorld(t, "stripe")
			tw.stripe.AddProduct("prod_basic", "basic")
			tw.stripe.AddPrice(stripemock.Price{ID: "price_basic", Product: "prod_basic", Amount: 900, Interval: "month"})
			tw.mapStripe(t, tw.stripeProduct(t, "basic", "member:basic"), "stripe-basic", "price_basic")
			began := tw.clock.Now()
			start := began.Add(-24 * time.Hour)
			customer := tw.stripe.AddCustomer(tw.alice.Email, "Alice", nil)
			remote := tw.stripe.AddSubscription(customer, "pm_alice", "price_basic", 900, start, start.AddDate(0, 1, 0))
			tw.stripe.TagSubscription(remote, "user_id", tw.alice.ID, start)
			tw.invoicePaid(t, remote)
			completedOnce(t, tw.fleet, kindConverge)
			held := tw.subscriptions(t, customerOf(tw.alice))
			require.Len(t, held, 1)
			tw.send(t, "invoice.payment_failed", tw.stripe.RenewSubscription(remote, false))
			completedOnce(t, tw.fleet, kindConverge)
			due, err := tw.alpha.GetSubscription(t.Context(), held[0].ID)
			require.NoError(t, err)
			require.Equal(t, billing.SubscriptionPastDue, due.Status)
			require.NotNil(t, due.MRRRisk)
			require.Equal(t, []billing.MRRRiskReason{billing.MRRRiskPastDue}, due.MRRRisk.Reasons)
			end := *due.CurrentPeriodEndsAt
			require.True(t, end.After(tw.clock.Now()))
			tw.clock.Advance(time.Hour)
			return tw, due.ID, end, began
		}

		// Canceled at its period end, it adds its MRR until then and stays at
		// risk; the case is written closed, churned, at the end.
		tw, sub, end, began := pastDue(t)
		customerCancel(t, tw.authorityWorld, sub)
		risk := mrrRiskOf(t, tw.alpha, sub)
		require.NotNil(t, risk, "MRR still added is still at risk")
		require.Equal(t, []billing.MRRRiskReason{billing.MRRRiskPastDue}, risk.Reasons)
		require.Equal(t, []string{"past_due churned [past_due]"}, riskCases(t, tw.world, tw.alpha, sub))
		closed, outcome, voluntary := closedCase(t, tw.world, tw.alpha, sub)
		require.Equal(t, "churned", outcome)
		require.True(t, closed.Equal(end), "churned at the end, %s", closed)
		require.NotNil(t, voluntary)
		require.True(t, *voluntary, "the customer's cancel is voluntary churn")
		require.Equal(t, map[string]int64{"USD": 9_000_000}, atRiskMRR(t, tw.alpha, riskWindow(began, tw.clock.Now().Add(time.Second))))
		require.Empty(t, recoveryRate(t, tw.alpha, riskWindow(began, tw.clock.Now().Add(time.Second))), "no case has closed yet")

		// Nothing runs at the end: the case reads churned there, once.
		tw.clock.Advance(end.Sub(tw.clock.Now()) + time.Hour)
		require.Nil(t, mrrRiskOf(t, tw.alpha, sub))
		require.Equal(t, []string{"past_due churned [past_due]"}, riskCases(t, tw.world, tw.alpha, sub))
		window := riskWindow(began, tw.clock.Now())
		require.Empty(t, atRiskMRR(t, tw.alpha, window))
		require.Equal(t, map[string]float64{"USD": 0}, recoveryRate(t, tw.alpha, window), "one case, churned")

		// A resume before the end reopens the same case; the resume makes the
		// subscription active, so the case is recovered.
		tw, sub, _, _ = pastDue(t)
		customerCancel(t, tw.authorityWorld, sub)
		require.Equal(t, []string{"past_due churned [past_due]"}, riskCases(t, tw.world, tw.alpha, sub))
		tw.clock.Advance(time.Hour)
		status, raw := tw.request(t, http.MethodPost, "/v1/me/subscriptions/"+sub.String()+"/resume", tw.aliceToken, map[string]any{})
		require.Equal(t, http.StatusOK, status, string(raw))
		require.Nil(t, mrrRiskOf(t, tw.alpha, sub), "active again")
		require.Equal(t, []string{"past_due recovered [past_due]"}, riskCases(t, tw.world, tw.alpha, sub), "the same case, reopened")
	})

	t.Run("OR-J42-S18/a-new-default-card-expiring-before-renewal-puts-a-subscription-following-it-at-risk-at-once-never-an-nmi-schedule", func(t *testing.T) {
		boundary := monthsAgo(3)
		v := newVaultCards(t, false, false, func(w *world) { w.clock.Advance(boundary.Add(-3 * 24 * time.Hour).Sub(w.clock.Now())) })
		good := v.saveCard(t, v.aliceToken, "4111111111111111", "visa", "1111")
		setDefaultCard(t, v.authorityWorld, v.aliceToken, good.ID)
		sub := v.subscribe(t, v.aliceToken, "following", 7*24, 10_000_000, good)
		require.Nil(t, sub.PaymentMethodID, "bought with the default, it follows the default")
		require.Nil(t, mrrRiskOf(t, v.alpha, sub.ID))
		startVaultFleet(t, v)

		// A new default that expires before the renewal: state says so at once.
		expiry := boundary.Add(-24 * time.Hour)
		expiring := v.saveExpiringCard(t, v.aliceToken, "4012888888881881", "1881", int(expiry.Month()), expiry.Year())
		setDefaultCard(t, v.authorityWorld, v.aliceToken, expiring.ID)
		risk := mrrRiskOf(t, v.alpha, sub.ID)
		require.NotNil(t, risk, "the subscription charges its customer's new default")
		require.Equal(t, []billing.MRRRiskReason{billing.MRRRiskCardExpiring}, risk.Reasons)

		// An NMI schedule charges the card NMI holds for it; one naming none
		// is never at risk on the default, though it renews after the default
		// expires. (A default held outside NMI cannot be set while an NMI
		// schedule follows it, so the schedule comes after the default.)
		schedule, _ := importNMISchedule(t, v.authorityWorld, v.journal, catalog.Field[int]{}, func(book *billing.DeclaredBilling) {
			book.Subscriptions[0].PaymentMethod = nil
		})
		require.Nil(t, schedule.PaymentMethodID)
		require.True(t, schedule.CurrentPeriodEndsAt.After(boundary))
		require.Nil(t, mrrRiskOf(t, v.alpha, schedule.ID), "no evidence ties the default to the card NMI charges")
	})

	t.Run("OR-J42-S19/an-upgrade-of-a-subscription-whose-card-expires-moves-its-at-risk-mrr-to-the-replacement-read-from-state", func(t *testing.T) {
		// The card expires at the boundary, before the renewal it pays for.
		boundary := monthsAgo(3)
		a, _, sub := purchasedMembershipWith(t, 24, func(w *world) { w.clock.Advance(boundary.Add(-12 * time.Hour).Sub(w.clock.Now())) })
		require.NotNil(t, sub.PaymentMethodID)
		require.True(t, sub.CurrentPeriodEndsAt.After(boundary))
		a.clock.Advance(time.Hour)
		expiry := boundary.Add(-24 * time.Hour)
		status, raw := a.request(t, http.MethodPatch, "/v1/me/payment-methods/"+sub.PaymentMethodID.String(), a.aliceToken,
			billing.UpdatePaymentMethodParams{ExpMonth: ptr(int(expiry.Month())), ExpYear: ptr(expiry.Year())})
		require.Equal(t, http.StatusOK, status, string(raw))
		risk := mrrRiskOf(t, a.alpha, sub.ID)
		require.NotNil(t, risk, "the card expires before the renewal it pays for")
		require.True(t, slices.Equal(risk.Reasons, []billing.MRRRiskReason{billing.MRRRiskCardExpiring}))

		price := upgradePrice(t, a, sub)
		status, raw = a.request(t, http.MethodPost, "/v1/me/subscriptions/"+sub.ID.String()+"/change", a.aliceToken, map[string]any{"price_id": price.ID},
			map[string]string{"Idempotency-Key": "upgrade-at-risk"})
		require.Equal(t, http.StatusOK, status, string(raw))
		var changed billing.SubscriptionChange
		require.NoError(t, json.Unmarshal(raw, &changed))
		require.Equal(t, "succeeded", changed.Status, string(raw))
		require.NotNil(t, changed.SubscriptionID)
		require.NotEqual(t, sub.ID, *changed.SubscriptionID)

		require.Nil(t, mrrRiskOf(t, a.alpha, sub.ID), "the replaced subscription adds no MRR")
		moved := mrrRiskOf(t, a.alpha, *changed.SubscriptionID)
		require.NotNil(t, moved, "the replacement charges the same card")
		require.Equal(t, []billing.MRRRiskReason{billing.MRRRiskCardExpiring}, moved.Reasons)
		require.Empty(t, riskCases(t, a.world, a.alpha, sub.ID))
		require.Empty(t, riskCases(t, a.world, a.alpha, *changed.SubscriptionID))
		window := riskWindow(boundary.Add(-24*time.Hour), a.clock.Now().Add(time.Second))
		require.Equal(t, map[string]int64{"USD": dailyTwentyMRR}, atRiskMRR(t, a.alpha, window))
		require.Empty(t, recoveryRate(t, a.alpha, window))
	})

	t.Run("OR-J42-S21/a-restore-carries-the-open-case-and-its-episodes-and-reads-the-same-at-risk-mrr", func(t *testing.T) {
		a, p, sub := purchasedMembership(t, 7*24)
		began := a.clock.Now()
		due := *sub.CurrentPeriodEndsAt
		p.core.SetDecline("4242", "insufficient_funds")
		a.clock.Advance(due.Sub(a.clock.Now()))
		fleet := startFleet(t, a.world)
		runRegistered(t, fleet, "openrails.billing_pass", 2)
		subscriptionState(t, a, sub.ID, "past_due")
		require.Equal(t, []string{"past_due open [past_due]"}, riskCases(t, a.world, a.alpha, sub.ID))

		archive := archiveBytes(t, a.alpha)
		target, client := archiveTarget(t, a.world)
		_, err := client.ImportBillingArchive(t.Context(), bytes.NewReader(archive))
		require.NoError(t, err)
		risk := mrrRiskOf(t, client, sub.ID)
		require.NotNil(t, risk, "the restore carries the open case")
		require.Equal(t, []billing.MRRRiskReason{billing.MRRRiskPastDue}, risk.Reasons)
		require.Equal(t, billing.MRRRiskPastDue, risk.EntryReason)
		require.Equal(t, []string{"past_due open [past_due]"}, riskCases(t, target, client, sub.ID))
		window := riskWindow(began, a.clock.Now().Add(time.Second))
		require.Equal(t, map[string]int64{"USD": weeklyTenMRR}, atRiskMRR(t, client, window))
		// A past read reads the restored episodes.
		past := riskWindow(began, a.clock.Now().Add(-time.Minute))
		require.Equal(t, atRiskMRR(t, a.alpha, past), atRiskMRR(t, client, past))
	})

	t.Run("OR-J42-S23/canceled-at-period-end-a-card-update-clears-card-expiring-at-once-with-no-case", func(t *testing.T) {
		boundary := monthsAgo(2)
		a, _, sub := purchasedMembershipWith(t, 7*24, func(w *world) { w.clock.Advance(boundary.Add(-3 * 24 * time.Hour).Sub(w.clock.Now())) })
		require.NotNil(t, sub.PaymentMethodID)
		require.True(t, sub.CurrentPeriodEndsAt.After(boundary))
		startFleet(t, a.world)
		expire := func(month, year int) {
			t.Helper()
			status, raw := a.request(t, http.MethodPatch, "/v1/me/payment-methods/"+sub.PaymentMethodID.String(), a.aliceToken,
				billing.UpdatePaymentMethodParams{ExpMonth: ptr(month), ExpYear: ptr(year)})
			require.Equal(t, http.StatusOK, status, string(raw))
		}
		risk := func() []billing.MRRRiskReason {
			if r := mrrRiskOf(t, a.alpha, sub.ID); r != nil {
				return r.Reasons
			}
			return nil
		}
		expiry := boundary.Add(-24 * time.Hour)
		expire(int(expiry.Month()), expiry.Year())
		require.Equal(t, []billing.MRRRiskReason{billing.MRRRiskCardExpiring}, risk(), "the card now expires before the renewal")

		// Canceled at its period end, it adds its MRR until then: its card is
		// still read, and a new expiry clears the reason at once.
		customerCancel(t, a, sub.ID)
		require.Equal(t, []billing.MRRRiskReason{billing.MRRRiskCardExpiring}, risk())
		expire(12, 2035)
		require.Nil(t, risk(), "card_expiring is read anew while canceled")
		require.Empty(t, riskCases(t, a.world, a.alpha, sub.ID), "card_expiring keeps no case")
	})
}

// usageInvoiceOf is the customer's one usage invoice.
func usageInvoiceOf(t *testing.T, a *authorityWorld, customer billing.CustomerID) billing.Invoice {
	t.Helper()
	rows, err := a.alpha.ListInvoices(t.Context(), billing.InvoiceListParams{CustomerID: customer})
	require.NoError(t, err)
	var usage []billing.Invoice
	for _, invoice := range rows.Items {
		if invoice.Usage != nil {
			usage = append(usage, invoice)
		}
	}
	require.Len(t, usage, 1)
	return usage[0]
}
