//go:build integration

package critical_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/integrations/basistheory"
	"github.com/open-rails/openrails/internal/nmimock"
	"github.com/riverqueue/river"
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

// riskQueued counts the customer's queued MRR risk changes.
func riskQueued(t *testing.T, w *world, client *openrails.Client, customer billing.CustomerID) int {
	t.Helper()
	var n int
	require.NoError(t, w.pool.QueryRow(t.Context(), "SELECT count(*) FROM "+pgx.Identifier{w.schema, "mrr_risk_due"}.Sanitize()+
		" WHERE merchant_id = $1 AND customer_id = $2", client.MerchantID().UUID(), customer.UUID()).Scan(&n))
	return n
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

	t.Run("OR-J42-S13/a-card-expiring-before-renewal-enters-on-the-period-change-and-leaves-on-an-account-updater-refresh", func(t *testing.T) {
		// A month boundary falls between the first and second period ends:
		// the card expires at it. Everything happens months ago, so every
		// payment is in the database's past. Ten-day periods put the second
		// renewal's 14-day lookahead after the first check, so the updater
		// is asked again once the renewal is paid.
		boundary := monthsAgo(3)
		v := newVaultCards(t, false, true, func(w *world) { w.clock.Advance(boundary.Add(-12 * 24 * time.Hour).Sub(w.clock.Now())) })
		expiry := boundary.Add(-24 * time.Hour)
		card := v.saveExpiringCard(t, v.aliceToken, "4111111111111111", "1111", int(expiry.Month()), expiry.Year())
		sub := v.subscribe(t, v.aliceToken, "expiring", 10*24, 10_000_000, card)
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
		a, p, customer := usageBillingWorld(t, true)
		sub := nmiMembershipOn(t, a, p, catalog.Field[int]{})
		oneUsage(t, a.alpha, usageInput(customer, "overdue-work", 3))
		fleet := startFleet(t, a.world)
		postUsageMonth(t, a.world, fleet)
		runUsageDue(t, fleet)
		a.clock.Advance(24 * time.Hour)
		runRegistered(t, fleet, "openrails.billing_pass", 1)
		invoice := usageInvoiceOf(t, a, customer)
		require.True(t, invoice.Usage.Delinquent)
		// The delinquency queued the customer; its commit wakes the risk drain.
		require.Eventually(t, func() bool {
			risk := mrrRiskOf(t, a.alpha, sub.ID)
			return risk != nil && len(risk.Reasons) == 1 && risk.Reasons[0] == billing.MRRRiskDelinquent
		}, 20*time.Second, 20*time.Millisecond, "the customer's subscription MRR in the currency is at risk")
		var overdue int64
		require.NoError(t, a.pool.QueryRow(t.Context(), "SELECT overdue_amount FROM "+pgx.Identifier{a.schema, "mrr_risk_episodes"}.Sanitize()+
			" WHERE merchant_id = $1 AND subscription_id = $2 AND reason = 'delinquent'", a.alpha.MerchantID().UUID(), sub.ID.UUID()).Scan(&overdue))
		require.Positive(t, overdue, "the overdue amount sits beside the at-risk MRR")

		_, err := a.alpha.RecordExternalPayment(t.Context(), billing.RecordExternalPaymentParams{InvoiceID: invoice.ID, Amount: 30_000_000, TransactionID: "bank-cleared"})
		require.NoError(t, err)
		runRegistered(t, fleet, "openrails.billing_pass", 1)
		require.False(t, usageInvoiceOf(t, a, customer).Usage.Delinquent)
		require.Eventually(t, func() bool { return mrrRiskOf(t, a.alpha, sub.ID) == nil }, 20*time.Second, 20*time.Millisecond)
		require.Equal(t, []string{"delinquent recovered [delinquent]"}, riskCases(t, a.world, a.alpha, sub.ID))
	})

	t.Run("OR-J42-S16/the-risk-drain-and-a-decline-for-one-customer-both-commit-and-the-decline-queued-after-the-claim-is-re-evaluated", func(t *testing.T) {
		a, p, sub := nmiMembership(t)
		merchant := a.alpha.MerchantID().UUID()
		_, err := a.pool.Exec(t.Context(), "INSERT INTO "+pgx.Identifier{a.schema, "mrr_risk_due"}.Sanitize()+" (merchant_id, customer_id, changed_at) VALUES ($1, $2, $3)",
			merchant, sub.CustomerID.UUID(), a.clock.Now())
		require.NoError(t, err)

		// The drain claims the customer's changes and holds its subscriptions.
		drain, err := a.pool.Begin(t.Context())
		require.NoError(t, err)
		defer func() { _ = drain.Rollback(context.Background()) }()
		_, err = drain.Exec(t.Context(), "SELECT set_config('openrails.merchant_id', $1, true)", merchant.String())
		require.NoError(t, err)
		var claimed, holder int
		require.NoError(t, drain.QueryRow(t.Context(), "SELECT "+pgx.Identifier{a.schema, "effect_due_mrr_risk"}.Sanitize()+"($1, 100, NULL), pg_backend_pid()", merchant).Scan(&claimed, &holder))
		require.Positive(t, claimed)
		var left int
		require.NoError(t, drain.QueryRow(t.Context(), "SELECT count(*) FROM "+pgx.Identifier{a.schema, "mrr_risk_due"}.Sanitize()+
			" WHERE merchant_id = $1 AND customer_id = $2", merchant, sub.CustomerID.UUID()).Scan(&left))
		require.Zero(t, left, "the claim took the customer's changes")

		// NMI declines the renewal for a card it must not charge again; the
		// decline's writer queues the customer and waits for the drain's locks.
		due := *sub.CurrentPeriodEndsAt
		sale := p.core.AddScheduleSale(*sub.RailSubscriptionID, due)
		p.core.EditSale(sale.TransactionID, func(s *nmimock.Sale) { s.Declined, s.IssuerCode = "200", "04" })
		a.clock.Advance(due.Sub(a.clock.Now()) + time.Hour)
		fleet := hostFleet(t, a.world)
		fleet.PeriodicJobs().Clear()
		require.NoError(t, fleet.Start(t.Context()))
		events, unsubscribe := fleet.Subscribe(river.EventKindJobCompleted, river.EventKindJobFailed)
		defer unsubscribe()
		job, err := fleet.Insert(t.Context(), subscriptionConvergeJob{PSPID: uuid.UUID(sub.PSPID), MerchantID: merchant, Rail: "nmi", SubscriptionReference: *sub.RailSubscriptionID},
			&river.InsertOpts{Queue: openrails.QueueBilling})
		require.NoError(t, err)
		require.Eventually(t, func() bool {
			var n int
			require.NoError(t, a.pool.QueryRow(t.Context(), "SELECT count(*) FROM pg_stat_activity WHERE $1::int = ANY(pg_blocking_pids(pid))", holder).Scan(&n))
			return n > 0
		}, 20*time.Second, 20*time.Millisecond, "the decline's writer waits on the drain")
		require.NoError(t, drain.Commit(t.Context()))
		deadline := time.After(20 * time.Second)
	converged:
		for {
			select {
			case event := <-events:
				if event != nil && event.Job.ID == job.Job.ID {
					require.Equal(t, river.EventKindJobCompleted, event.Kind, "%+v", event.Job.Errors)
					break converged
				}
			case <-deadline:
				t.Fatal("the decline's convergence did not finish")
			}
		}
		subscriptionState(t, a, sub.ID, "awaiting_method")

		// The decline committed after the claim: it is still queued, and the
		// next drain takes it.
		require.Positive(t, riskQueued(t, a.world, a.alpha, sub.CustomerID))
		runRegistered(t, fleet, "openrails.mrr_risk", 1)
		require.Zero(t, riskQueued(t, a.world, a.alpha, sub.CustomerID))
		risk := mrrRiskOf(t, a.alpha, sub.ID)
		require.NotNil(t, risk)
		require.Equal(t, []billing.MRRRiskReason{billing.MRRRiskNeedsPaymentMethod}, risk.Reasons)
		require.Equal(t, []string{"needs_payment_method open [needs_payment_method]"}, riskCases(t, a.world, a.alpha, sub.ID))
	})

	t.Run("OR-J42-S17/a-cancel-at-period-end-keeps-the-case-open-a-resume-continues-it-the-end-churns-it-once", func(t *testing.T) {
		boundary := monthsAgo(3)
		v, card := expiringVaultCards(t, boundary, 3*24*time.Hour)
		began := v.clock.Now()
		sub := v.subscribe(t, v.aliceToken, "canceled-at-risk", 7*24, 10_000_000, card)
		end := *sub.CurrentPeriodEndsAt
		require.True(t, end.After(boundary))
		risk := mrrRiskOf(t, v.alpha, sub.ID)
		require.NotNil(t, risk, "the card expires before the renewal it pays for")
		require.Equal(t, []billing.MRRRiskReason{billing.MRRRiskCardExpiring}, risk.Reasons)

		// Canceled at its period end, it adds its MRR until then: the case
		// stays open with its reasons.
		customerCancel(t, v.authorityWorld, sub.ID)
		risk = mrrRiskOf(t, v.alpha, sub.ID)
		require.NotNil(t, risk, "MRR still added is still at risk")
		require.Equal(t, []billing.MRRRiskReason{billing.MRRRiskCardExpiring}, risk.Reasons)
		require.Equal(t, []string{"card_expiring open [card_expiring]"}, riskCases(t, v.world, v.alpha, sub.ID))
		require.Equal(t, map[string]int64{"USD": weeklyTenMRR}, atRiskMRR(t, v.alpha, riskWindow(began, v.clock.Now().Add(time.Second))))

		// A resume continues the same case.
		_, err := v.alpha.ResumeSubscription(t.Context(), sub.ID)
		require.NoError(t, err)
		require.Equal(t, []string{"card_expiring open [card_expiring]"}, riskCases(t, v.world, v.alpha, sub.ID))

		// Canceled again: the end churns it, once, at the end.
		customerCancel(t, v.authorityWorld, sub.ID)
		v.clock.Advance(end.Sub(v.clock.Now()))
		fleet := startFleet(t, v.world)
		runRegistered(t, fleet, "openrails.billing_pass", 2)
		require.Eventually(t, func() bool { return mrrRiskOf(t, v.alpha, sub.ID) == nil }, 20*time.Second, 20*time.Millisecond)
		require.Equal(t, []string{"card_expiring churned [card_expiring]"}, riskCases(t, v.world, v.alpha, sub.ID))
		closed, outcome, voluntary := closedCase(t, v.world, v.alpha, sub.ID)
		require.Equal(t, "churned", outcome)
		require.True(t, closed.Equal(end), "churned at the end, %s", closed)
		require.NotNil(t, voluntary)
		require.True(t, *voluntary, "the customer's cancel is voluntary churn")
		require.Empty(t, atRiskMRR(t, v.alpha, riskWindow(began, v.clock.Now().Add(time.Second))))
	})

	t.Run("OR-J42-S18/a-new-default-card-expiring-before-renewal-puts-a-subscription-following-it-at-risk-never-an-nmi-schedule", func(t *testing.T) {
		boundary := monthsAgo(3)
		v := newVaultCards(t, false, false, func(w *world) { w.clock.Advance(boundary.Add(-3 * 24 * time.Hour).Sub(w.clock.Now())) })
		good := v.saveCard(t, v.aliceToken, "4111111111111111", "visa", "1111")
		setDefaultCard(t, v.authorityWorld, v.aliceToken, good.ID)
		sub := v.subscribe(t, v.aliceToken, "following", 7*24, 10_000_000, good)
		require.Nil(t, sub.PaymentMethodID, "bought with the default, it follows the default")
		require.Nil(t, mrrRiskOf(t, v.alpha, sub.ID))
		startVaultFleet(t, v)

		// A new default that expires before the renewal: the change queues the
		// customer and its commit wakes the drain.
		expiry := boundary.Add(-24 * time.Hour)
		expiring := v.saveExpiringCard(t, v.aliceToken, "4012888888881881", "1881", int(expiry.Month()), expiry.Year())
		setDefaultCard(t, v.authorityWorld, v.aliceToken, expiring.ID)
		require.Eventually(t, func() bool {
			risk := mrrRiskOf(t, v.alpha, sub.ID)
			return risk != nil && len(risk.Reasons) == 1 && risk.Reasons[0] == billing.MRRRiskCardExpiring
		}, 20*time.Second, 20*time.Millisecond, "the subscription charges its customer's new default")
		require.Zero(t, riskQueued(t, v.world, v.alpha, sub.CustomerID))

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

	t.Run("OR-J42-S19/an-upgrade-while-at-risk-supersedes-the-case-and-the-replacement-opens-its-own", func(t *testing.T) {
		// The card expires at the boundary, before the renewal it pays for.
		boundary := monthsAgo(3)
		a, _, sub := purchasedMembershipWith(t, 24, func(w *world) { w.clock.Advance(boundary.Add(-12 * time.Hour).Sub(w.clock.Now())) })
		require.NotNil(t, sub.PaymentMethodID)
		require.True(t, sub.CurrentPeriodEndsAt.After(boundary))
		startFleet(t, a.world)
		a.clock.Advance(time.Hour)
		expiry := boundary.Add(-24 * time.Hour)
		status, raw := a.request(t, http.MethodPatch, "/v1/me/payment-methods/"+sub.PaymentMethodID.String(), a.aliceToken,
			billing.UpdatePaymentMethodParams{ExpMonth: ptr(int(expiry.Month())), ExpYear: ptr(expiry.Year())})
		require.Equal(t, http.StatusOK, status, string(raw))
		require.Eventually(t, func() bool {
			risk := mrrRiskOf(t, a.alpha, sub.ID)
			return risk != nil && slices.Equal(risk.Reasons, []billing.MRRRiskReason{billing.MRRRiskCardExpiring})
		}, 20*time.Second, 20*time.Millisecond, "the card expires before the renewal it pays for")

		price := upgradePrice(t, a, sub)
		status, raw = a.request(t, http.MethodPost, "/v1/me/subscriptions/"+sub.ID.String()+"/change", a.aliceToken, map[string]any{"price_id": price.ID},
			map[string]string{"Idempotency-Key": "upgrade-at-risk"})
		require.Equal(t, http.StatusOK, status, string(raw))
		var changed billing.SubscriptionChange
		require.NoError(t, json.Unmarshal(raw, &changed))
		require.Equal(t, "succeeded", changed.Status, string(raw))
		require.NotNil(t, changed.SubscriptionID)
		require.NotEqual(t, sub.ID, *changed.SubscriptionID)

		require.Equal(t, []string{"card_expiring superseded [card_expiring]"}, riskCases(t, a.world, a.alpha, sub.ID), "an upgrade is neither recovery nor churn")
		require.Equal(t, []string{"card_expiring open [card_expiring]"}, riskCases(t, a.world, a.alpha, *changed.SubscriptionID))
		window := riskWindow(boundary.Add(-24*time.Hour), a.clock.Now().Add(time.Second))
		require.Equal(t, map[string]int64{"USD": dailyTwentyMRR}, atRiskMRR(t, a.alpha, window))
		require.Empty(t, recoveryRate(t, a.alpha, window))
	})

	t.Run("OR-J42-S21/a-restore-carries-the-open-case-and-re-evaluates-every-customer-with-mrr", func(t *testing.T) {
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

		// The source's queued changes stay there: the restore queues every
		// customer with MRR, and the target's drain re-evaluates them.
		require.Equal(t, 1, riskQueued(t, target, client, sub.CustomerID))
		startFleet(t, target)
		require.Eventually(t, func() bool { return riskQueued(t, target, client, sub.CustomerID) == 0 }, 20*time.Second, 20*time.Millisecond)
		require.Equal(t, []string{"past_due open [past_due]"}, riskCases(t, target, client, sub.ID))
	})

	t.Run("OR-J42-S22/a-customer-whose-evaluation-fails-alone-is-set-aside-with-a-finding-and-the-rest-drain", func(t *testing.T) {
		a := ccbillWebhookWorld(t)
		price := ccbillMemberPrice(t, a.alpha)
		book := ccbillBook(price, a.clock.Now().UTC().Truncate(time.Second), 0, 2)
		res, err := a.alpha.ImportBilling(t.Context(), book)
		require.NoError(t, err)
		require.Len(t, res.Imported, 2, "%v", res.Reasons)
		failing, healthy := book.Customers[0].Customer, book.Customers[1].Customer
		due := pgx.Identifier{a.schema, "mrr_risk_due"}.Sanitize()
		queue := func(customer billing.CustomerID) {
			t.Helper()
			_, err := a.pool.Exec(t.Context(), "INSERT INTO "+due+" (merchant_id, customer_id, changed_at) VALUES ($1, $2, $3)",
				a.alpha.MerchantID().UUID(), customer.UUID(), a.clock.Now())
			require.NoError(t, err)
		}
		finding := func() string {
			t.Helper()
			var status string
			err := a.pool.QueryRow(t.Context(), "SELECT status FROM "+pgx.Identifier{a.schema, "reconciliation_findings"}.Sanitize()+
				" WHERE merchant_id = $1 AND finding_type = 'mrr.risk.evaluation_failed' AND subject_key = $2", a.alpha.MerchantID().UUID(), failing.String()).Scan(&status)
			if errors.Is(err, pgx.ErrNoRows) {
				return ""
			}
			require.NoError(t, err)
			return status
		}
		// One customer's changes cannot be taken: its evaluation fails, alone.
		poison := pgx.Identifier{a.schema, "poison_mrr_risk"}.Sanitize()
		_, err = a.pool.Exec(t.Context(), "CREATE FUNCTION "+poison+"() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected evaluation failure'; END $$")
		require.NoError(t, err)
		_, err = a.pool.Exec(t.Context(), "CREATE TRIGGER poison_mrr_risk BEFORE DELETE ON "+due+" FOR EACH ROW WHEN (OLD.customer_id = '"+failing.String()+"') EXECUTE FUNCTION "+poison+"()")
		require.NoError(t, err)
		queue(failing)
		queue(healthy)
		fleet := hostFleet(t, a.world)
		fleet.PeriodicJobs().Clear()
		require.NoError(t, fleet.Start(t.Context()))

		runRegistered(t, fleet, "openrails.mrr_risk", 1)
		require.Zero(t, riskQueued(t, a.world, a.alpha, healthy), "the rest of the merchant drains")
		require.Equal(t, 1, riskQueued(t, a.world, a.alpha, failing), "the failing customer stays queued")
		require.Equal(t, "requires_review", finding())
		var held bool
		require.NoError(t, a.pool.QueryRow(t.Context(), "SELECT bool_and(held_until > now()) FROM "+due+" WHERE customer_id = $1", failing.UUID()).Scan(&held))
		require.True(t, held, "set aside")

		// Set aside, it is skipped: the next drain completes without it.
		queue(healthy)
		runRegistered(t, fleet, "openrails.mrr_risk", 1)
		require.Zero(t, riskQueued(t, a.world, a.alpha, healthy))
		require.Equal(t, 1, riskQueued(t, a.world, a.alpha, failing))

		// Once it evaluates, it takes the changes set aside and the finding closes.
		_, err = a.pool.Exec(t.Context(), "DROP TRIGGER poison_mrr_risk ON "+due)
		require.NoError(t, err)
		queue(failing)
		runRegistered(t, fleet, "openrails.mrr_risk", 1)
		require.Zero(t, riskQueued(t, a.world, a.alpha, failing))
		require.Equal(t, "fixed", finding())
	})

	t.Run("OR-J42-S23/canceled-at-period-end-a-card-update-clears-card-expiring-and-recovers-the-case", func(t *testing.T) {
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
		require.Eventually(t, func() bool { return slices.Equal(risk(), []billing.MRRRiskReason{billing.MRRRiskCardExpiring}) },
			20*time.Second, 20*time.Millisecond, "the card now expires before the renewal")

		// Canceled at its period end, the case stays open; its card is still
		// read, and a new expiry clears the reason.
		customerCancel(t, a, sub.ID)
		require.Equal(t, []billing.MRRRiskReason{billing.MRRRiskCardExpiring}, risk())
		expire(12, 2035)
		require.Eventually(t, func() bool { return risk() == nil }, 20*time.Second, 20*time.Millisecond, "card_expiring is taken anew while canceled")
		require.Equal(t, []string{"card_expiring recovered [card_expiring]"}, riskCases(t, a.world, a.alpha, sub.ID))
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
