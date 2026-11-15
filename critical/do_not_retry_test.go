//go:build integration

package critical_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/nmimock"
	"github.com/open-rails/openrails/internal/stripemock"
	"github.com/riverqueue/river"
	"github.com/stretchr/testify/require"
)

// Visa forbids a reattempt after a category 1 code and Mastercard after MAC
// 03/21: a card the issuer said not to charge again is not charged again,
// by dunning or by its customer, until the card changes.
func TestDoNotRetry(t *testing.T) {
	t.Run("OR-J16-S40/a-lost-card-renewal-refuses-the-same-card-and-admits-another", func(t *testing.T) {
		a, p, sub := purchasedMembership(t, 7*24)
		require.NotNil(t, sub.PaymentMethodID)
		p.core.SetDecline("4242", "lost_card")
		a.clock.Advance(sub.CurrentPeriodEndsAt.Sub(a.clock.Now()))
		fleet := startFleet(t, a.world)
		runRegistered(t, fleet, "openrails.billing_pass", 1)
		subscriptionState(t, a, sub.ID, "awaiting_method")
		order := myRenewalOrder(t, a, sub.ID)
		creates := paymentIntentCreates(p)
		status, raw := payMyRenewal(t, a, order, *sub.PaymentMethodID, "lost-card-again")
		require.Equal(t, http.StatusConflict, status, string(raw))
		require.Contains(t, string(raw), `"code":"order_not_payable"`)
		require.Equal(t, creates, paymentIntentCreates(p), "the refused card never reaches the provider")
		subscriptionState(t, a, sub.ID, "awaiting_method")
		replacement := saveMethod(t, a, p, a.aliceToken, "5556")
		status, raw = payMyRenewal(t, a, order, replacement.ID, "replacement-card")
		require.Equal(t, http.StatusOK, status, string(raw))
		var paid billing.Order
		require.NoError(t, json.Unmarshal(raw, &paid))
		require.Equal(t, billing.OrderComplete, paid.Status)
		subscriptionState(t, a, sub.ID, "active")
		require.Len(t, p.effects(t), 2, "enrollment and the renewal on the new card")
	})
	t.Run("OR-J16-S41/an-nmi-decline-with-a-never-retry-issuer-code-waits-for-a-new-card", func(t *testing.T) {
		a, p, sub := nmiMembership(t)
		due := *sub.CurrentPeriodEndsAt
		sale := p.core.AddScheduleSale(*sub.RailSubscriptionID, due)
		// NMI's catch-all decline; the Query API names the issuer's pick-up
		// card, a Visa category 1 code.
		p.core.EditSale(sale.TransactionID, func(s *nmimock.Sale) { s.Declined, s.IssuerCode = "200", "04" })
		a.clock.Advance(due.Sub(a.clock.Now()) + time.Hour)
		fleet := hostFleet(t, a.world)
		fleet.PeriodicJobs().Clear()
		require.NoError(t, a.engine.Start(t.Context(), openrails.WithRiverClient(fleet)))
		require.NoError(t, fleet.Start(t.Context()))
		events, unsubscribe := fleet.Subscribe(river.EventKindJobCompleted, river.EventKindJobFailed)
		defer unsubscribe()
		job, err := fleet.Insert(t.Context(), subscriptionConvergeJob{PSPID: uuid.UUID(sub.PSPID), MerchantID: a.alpha.MerchantID().UUID(), Rail: "nmi", SubscriptionReference: *sub.RailSubscriptionID}, &river.InsertOpts{Queue: openrails.QueueBilling})
		require.NoError(t, err)
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
				t.Fatal("the subscription's convergence did not finish")
			}
		}
		waiting := subscriptionState(t, a, sub.ID, "awaiting_method")
		require.NotNil(t, waiting.Dunning)
		require.True(t, waiting.Dunning.WaitingForNewCard)
		require.Nil(t, waiting.Dunning.NextRetryAt, "no retry is scheduled on the card")
		sales := len(p.core.CallsTo(http.MethodPost, "transact.php", nil))
		status, raw := a.request(t, http.MethodPost, "/v1/me/subscriptions/"+sub.ID.String()+"/retry-now", a.aliceToken, map[string]any{}, map[string]string{"Idempotency-Key": "same-card"})
		require.Equal(t, http.StatusConflict, status, string(raw))
		require.Contains(t, string(raw), `"code":"payment_not_retryable"`)
		mine := mySubscription(t, a, sub.ID)
		require.NotNil(t, mine.Recovery)
		require.False(t, mine.Recovery.Retryable)
		require.Equal(t, "payment_method_update_required", mine.Recovery.BlockedReason)
		a.clock.Advance(2 * 24 * time.Hour)
		runRegistered(t, fleet, "openrails.billing_pass", 1)
		subscriptionState(t, a, sub.ID, "awaiting_method")
		require.Len(t, p.core.CallsTo(http.MethodPost, "transact.php", nil), sales, "neither dunning nor the customer charges the card")
	})
	t.Run("OR-J16-S42/network-do-not-retry-advice-stops-dunning-on-the-card", func(t *testing.T) {
		for _, tc := range []struct {
			name   string
			advice stripemock.Advice
		}{
			{"stripe-do-not-try-again", stripemock.Advice{Code: "do_not_try_again"}},
			{"mastercard-advice-03", stripemock.Advice{NetworkAdviceCode: "03"}},
			{"visa-category-1-closed-account", stripemock.Advice{NetworkDeclineCode: "46"}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				a, p, sub := purchasedMembership(t, 7*24)
				due := *sub.CurrentPeriodEndsAt
				p.core.SetDecline("4242", "generic_decline")
				p.core.SetAdvice("4242", tc.advice)
				a.clock.Advance(due.Sub(a.clock.Now()))
				fleet := startFleet(t, a.world)
				runRegistered(t, fleet, "openrails.billing_pass", 1)
				waiting := subscriptionState(t, a, sub.ID, "awaiting_method")
				require.NotNil(t, waiting.Dunning)
				require.True(t, waiting.Dunning.WaitingForNewCard)
				require.Nil(t, waiting.Dunning.NextRetryAt)
				declines, err := a.alpha.ListPaymentAttempts(t.Context(), billing.PaymentAttemptListParams{SubscriptionID: sub.ID, Kind: []string{"rebill"}})
				require.NoError(t, err)
				require.Len(t, declines.Items, 1)
				require.Equal(t, billing.DeclineGeneric, declines.Items[0].Reason, "the issuer's reason is kept")
				require.Equal(t, "fix_payment_method", declines.Items[0].Action)
				require.Equal(t, tc.advice.NetworkDeclineCode, declines.Items[0].IssuerCode)
				creates := paymentIntentCreates(p)
				// Past both weekly retry offsets, inside the wait for a card.
				a.clock.Advance(2 * 24 * time.Hour)
				runRegistered(t, fleet, "openrails.billing_pass", 1)
				subscriptionState(t, a, sub.ID, "awaiting_method")
				require.Equal(t, creates, paymentIntentCreates(p), "dunning does not retry the card")
				require.Len(t, p.effects(t), 1)
			})
		}
	})
	t.Run("OR-J16-S43/a-decline-the-cardholder-can-clear-is-retried-on-the-card-up-to-the-cap", func(t *testing.T) {
		a, p, sub := purchasedMembership(t, 7*24)
		require.NotNil(t, sub.PaymentMethodID)
		p.core.SetDecline("4242", "incorrect_zip")
		a.clock.Advance(sub.CurrentPeriodEndsAt.Sub(a.clock.Now()))
		fleet := startFleet(t, a.world)
		runRegistered(t, fleet, "openrails.billing_pass", 1)
		subscriptionState(t, a, sub.ID, "awaiting_method")
		order := myRenewalOrder(t, a, sub.ID)
		const limit = 3
		for i := range limit {
			status, raw := payMyRenewal(t, a, order, *sub.PaymentMethodID, fmt.Sprintf("corrected-address-%d", i))
			require.Equal(t, http.StatusPaymentRequired, status, string(raw))
			require.Contains(t, string(raw), `"code":"card_declined"`)
		}
		retries, err := a.alpha.ListPaymentAttempts(t.Context(), billing.PaymentAttemptListParams{SubscriptionID: sub.ID, Kind: []string{"customer_retry"}})
		require.NoError(t, err)
		require.Len(t, retries.Items, limit)
		creates := paymentIntentCreates(p)
		status, raw := payMyRenewal(t, a, order, *sub.PaymentMethodID, "past-the-cap")
		require.Equal(t, http.StatusConflict, status, string(raw))
		require.Contains(t, string(raw), `"code":"order_not_payable"`)
		require.Equal(t, creates, paymentIntentCreates(p), "past the cap the card never reaches the provider")
		subscriptionState(t, a, sub.ID, "awaiting_method")
		require.Len(t, p.effects(t), 1)
	})
}

func mySubscription(t *testing.T, a *authorityWorld, id billing.SubscriptionID) billing.Subscription {
	t.Helper()
	status, raw := a.request(t, http.MethodGet, "/v1/me/subscriptions/"+id.String(), a.aliceToken, nil)
	require.Equal(t, http.StatusOK, status, string(raw))
	var mine billing.Subscription
	require.NoError(t, json.Unmarshal(raw, &mine))
	return mine
}

// myRenewalOrder is the unpaid renewal the customer's subscription names.
func myRenewalOrder(t *testing.T, a *authorityWorld, id billing.SubscriptionID) billing.Order {
	t.Helper()
	mine := mySubscription(t, a, id)
	require.NotNil(t, mine.Recovery)
	require.NotNil(t, mine.Recovery.OrderID)
	status, raw := a.request(t, http.MethodGet, "/v1/me/orders/"+mine.Recovery.OrderID.String(), a.aliceToken, nil)
	require.Equal(t, http.StatusOK, status, string(raw))
	var order billing.Order
	require.NoError(t, json.Unmarshal(raw, &order))
	return order
}

func payMyRenewal(t *testing.T, a *authorityWorld, order billing.Order, method billing.PaymentMethodID, key string) (int, []byte) {
	t.Helper()
	return a.request(t, http.MethodPost, "/v1/me/orders/"+order.ID.String()+"/pay", a.aliceToken, billing.PayOrderParams{ExpectedTotal: order.Total, Payment: billing.OrderPaymentParams{PaymentMethodID: &method}}, map[string]string{"Idempotency-Key": key})
}

// paymentIntentCreates counts the PaymentIntents Stripe was asked to create.
func paymentIntentCreates(p *providerLedger) int {
	n := 0
	for _, call := range p.core.Submitted("/v1/payment_intents") {
		if call.Method == http.MethodPost {
			n++
		}
	}
	return n
}
