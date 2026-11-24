//go:build integration

package critical_test

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/nmimock"
	"github.com/open-rails/openrails/internal/stripemock"
	"github.com/riverqueue/river"
	"github.com/stretchr/testify/require"
)

// Each refusal is stored with what caused it: the customer, the issuer's
// discretion, suspected fraud, the merchant or gateway, or processing.
func TestDeclineCauses(t *testing.T) {
	t.Run("OR-J45-S03/a-stripe-renewal-radar-blocked-is-the-merchants-refusal-not-the-issuers-fraud", func(t *testing.T) {
		for _, tc := range []struct {
			name   string
			block  func(*providerLedger)
			code   string
			reason billing.DeclineReason
			action string
		}{
			// Radar stops the charge before the network; its error still reads
			// as a fraudulent decline.
			{"radar-block-waits-for-a-new-card", func(p *providerLedger) { p.core.SetRadarBlock("4242", "highest_risk_level") }, "highest_risk_level", billing.DeclineBlockedByPSP, "fix_payment_method"},
			{"issuer-fraud-is-the-issuers", func(p *providerLedger) { p.core.SetDecline("4242", "fraudulent") }, "fraudulent", billing.DeclineFraudulent, "non_recoverable"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				a, p, sub := purchasedMembership(t, 7*24)
				tc.block(p)
				a.clock.Advance(sub.CurrentPeriodEndsAt.Sub(a.clock.Now()))
				fleet := startFleet(t, a.world)
				runRegistered(t, fleet, "openrails.billing_pass", 1)
				rebill := awaitAttempt(t, a, sub.ID, "rebill")
				require.Equal(t, tc.reason, rebill.Reason)
				require.Equal(t, tc.action, rebill.Action)
				require.Equal(t, tc.code, rebill.ResponseCode)
				if tc.reason == billing.DeclineBlockedByPSP {
					waiting := subscriptionState(t, a, sub.ID, "awaiting_method")
					require.NotNil(t, waiting.Dunning)
					require.True(t, waiting.Dunning.WaitingForNewCard, "a block never cancels the subscription")
				}
			})
		}
	})
	t.Run("OR-J45-S04/newly-mapped-codes-decline-with-their-cause", func(t *testing.T) {
		for _, tc := range []struct {
			code   string
			reason billing.DeclineReason
			action string
		}{
			{"do_not_try_again", billing.DeclineGeneric, "fix_payment_method"},
			{"new_account_information_available", billing.DeclineUpdateCardholderData, "fix_payment_method"},
			{"service_not_allowed", billing.DeclineTransactionNotAllowed, "fix_payment_method"},
			{"mobile_device_authentication_required", billing.DeclineAuthenticationRequired, "fix_payment_method"},
			{"invalid_amount", billing.DeclineInvalidRequest, "retry"},
			{"testmode_decline", billing.DeclineInvalidRequest, "retry"},
		} {
			t.Run("stripe-"+tc.code, func(t *testing.T) {
				a, p, sub := purchasedMembership(t, 7*24)
				p.core.SetDecline("4242", tc.code)
				a.clock.Advance(sub.CurrentPeriodEndsAt.Sub(a.clock.Now()))
				fleet := startFleet(t, a.world)
				runRegistered(t, fleet, "openrails.billing_pass", 1)
				rebill := awaitAttempt(t, a, sub.ID, "rebill")
				require.Equal(t, tc.reason, rebill.Reason)
				require.Equal(t, tc.action, rebill.Action)
				state := "past_due"
				if tc.action == "fix_payment_method" {
					state = "awaiting_method"
				}
				subscriptionState(t, a, sub.ID, state)
			})
		}
		t.Run("nmi-461-the-merchants-account-takes-no-such-brand", func(t *testing.T) {
			a, p, sub := nmiMembership(t)
			due := *sub.CurrentPeriodEndsAt
			sale := p.core.AddScheduleSale(*sub.RailSubscriptionID, due)
			p.core.EditSale(sale.TransactionID, func(s *nmimock.Sale) { s.Declined = "461" })
			a.clock.Advance(due.Sub(a.clock.Now()) + time.Hour)
			fleet := hostFleet(t, a.world)
			fleet.PeriodicJobs().Clear()
			require.NoError(t, a.engine.Start(t.Context(), openrails.WithRiverClient(fleet)))
			require.NoError(t, fleet.Start(t.Context()))
			convergeNMISubscription(t, a, fleet, sub)
			rebill := awaitAttempt(t, a, sub.ID, "rebill")
			require.Equal(t, billing.DeclineBrandNotAccepted, rebill.Reason)
			require.Equal(t, "fix_payment_method", rebill.Action)
			subscriptionState(t, a, sub.ID, "awaiting_method")
		})
	})
	t.Run("OR-J45-S05/mastercard-retry-after-advice-delays-dunning-and-past-the-window-waits-for-a-new-card", func(t *testing.T) {
		t.Run("mac-26-retries-two-days-after-the-decline", func(t *testing.T) {
			a, p, sub := purchasedMembership(t, 7*24)
			p.core.SetDecline("4242", "insufficient_funds")
			p.core.SetAdvice("4242", stripemock.Advice{NetworkAdviceCode: "26", NetworkDeclineCode: "51"})
			a.clock.Advance(sub.CurrentPeriodEndsAt.Sub(a.clock.Now()))
			fleet := startFleet(t, a.world)
			runRegistered(t, fleet, "openrails.billing_pass", 1)
			rebill := awaitAttempt(t, a, sub.ID, "rebill")
			require.Equal(t, billing.DeclineInsufficientFunds, rebill.Reason)
			dunned := subscriptionState(t, a, sub.ID, "past_due")
			require.NotNil(t, dunned.Dunning)
			require.NotNil(t, dunned.Dunning.NextRetryAt)
			// The weekly schedule's first retry is a day out; the advice says two.
			require.WithinDuration(t, rebill.AttemptedAt.Add(48*time.Hour), *dunned.Dunning.NextRetryAt, time.Minute)
		})
		t.Run("mac-27-past-the-window-waits-for-a-new-card", func(t *testing.T) {
			a, p, sub := purchasedMembership(t, 7*24)
			p.core.SetDecline("4242", "insufficient_funds")
			p.core.SetAdvice("4242", stripemock.Advice{NetworkAdviceCode: "27", NetworkDeclineCode: "51"})
			a.clock.Advance(sub.CurrentPeriodEndsAt.Sub(a.clock.Now()))
			fleet := startFleet(t, a.world)
			runRegistered(t, fleet, "openrails.billing_pass", 1)
			rebill := awaitAttempt(t, a, sub.ID, "rebill")
			require.Equal(t, "retry", rebill.Action, "the decline itself is retryable")
			waiting := subscriptionState(t, a, sub.ID, "awaiting_method")
			require.NotNil(t, waiting.Dunning)
			require.True(t, waiting.Dunning.WaitingForNewCard, "four days is past the weekly case: it waits, it doesn't cancel")
			require.Nil(t, waiting.Dunning.NextRetryAt)
		})
	})
	t.Run("OR-J45-S06/new-card-data-and-cards-that-wont-recur-wait-for-a-new-card", func(t *testing.T) {
		for _, tc := range []struct {
			name   string
			advice stripemock.Advice
			reason billing.DeclineReason
		}{
			{"mastercard-01-new-account-information", stripemock.Advice{NetworkAdviceCode: "01"}, billing.DeclineUpdateCardholderData},
			{"stripe-confirm-card-data", stripemock.Advice{Code: "confirm_card_data"}, billing.DeclineUpdateCardholderData},
			{"mastercard-40-non-reloadable-prepaid", stripemock.Advice{NetworkAdviceCode: "40"}, billing.DeclineGeneric},
			{"mastercard-41-single-use-virtual-card", stripemock.Advice{NetworkAdviceCode: "41"}, billing.DeclineGeneric},
		} {
			t.Run(tc.name, func(t *testing.T) {
				a, p, sub := purchasedMembership(t, 7*24)
				p.core.SetDecline("4242", "generic_decline")
				p.core.SetAdvice("4242", tc.advice)
				a.clock.Advance(sub.CurrentPeriodEndsAt.Sub(a.clock.Now()))
				fleet := startFleet(t, a.world)
				runRegistered(t, fleet, "openrails.billing_pass", 1)
				rebill := awaitAttempt(t, a, sub.ID, "rebill")
				require.Equal(t, tc.reason, rebill.Reason)
				require.Equal(t, "fix_payment_method", rebill.Action)
				waiting := subscriptionState(t, a, sub.ID, "awaiting_method")
				require.NotNil(t, waiting.Dunning)
				require.True(t, waiting.Dunning.WaitingForNewCard)
			})
		}
	})
	t.Run("OR-J45-S02/an-nmi-catch-all-decline-is-classified-by-the-issuers-code", func(t *testing.T) {
		for _, tc := range []struct {
			name, issuer string
			reason       billing.DeclineReason
			action       string
			waits        bool
		}{
			// Visa category 1: the card is never charged again.
			{"pick-up-card-waits-for-a-new-card", "04", billing.DeclinePickupCard, "fix_payment_method", true},
			// A stop order behind NMI's catch-all waits; it never cancels.
			{"stop-order-waits-and-never-cancels", "R1", billing.DeclineStopRecurring, "fix_payment_method", true},
			{"insufficient-funds-is-retried", "51", billing.DeclineInsufficientFunds, "retry", false},
			{"suspected-fraud-is-retried", "59", billing.DeclineSuspectedFraud, "retry", false},
		} {
			t.Run(tc.name, func(t *testing.T) {
				a, p, sub := nmiMembership(t)
				due := *sub.CurrentPeriodEndsAt
				sale := p.core.AddScheduleSale(*sub.RailSubscriptionID, due)
				p.core.EditSale(sale.TransactionID, func(s *nmimock.Sale) { s.Declined = "202" })
				a.clock.Advance(due.Sub(a.clock.Now()) + time.Hour)
				fleet := hostFleet(t, a.world)
				fleet.PeriodicJobs().Clear()
				require.NoError(t, a.engine.Start(t.Context(), openrails.WithRiverClient(fleet)))
				require.NoError(t, fleet.Start(t.Context()))
				convergeNMISubscription(t, a, fleet, sub)
				dunned := subscriptionState(t, a, sub.ID, "past_due")
				require.NotNil(t, dunned.Dunning)
				require.NotNil(t, dunned.Dunning.NextRetryAt)

				// OpenRails' retry: Direct Post answers NMI's catch-all 200, and
				// only the Query API names the issuer's code.
				p.core.SetDecline("4242", "200")
				p.core.SetIssuerCode("4242", tc.issuer)
				a.clock.Advance(dunned.Dunning.NextRetryAt.Sub(a.clock.Now()) + time.Minute)
				runRegistered(t, fleet, "openrails.billing_pass", 1)
				retry := awaitAttempt(t, a, sub.ID, "dunning_retry")
				require.Equal(t, tc.reason, retry.Reason)
				require.Equal(t, tc.action, retry.Action)
				require.Equal(t, tc.issuer, retry.IssuerCode)
				require.Equal(t, "200", retry.ResponseCode, "NMI's own code is kept")
				if !tc.waits {
					retried := subscriptionState(t, a, sub.ID, "past_due")
					require.NotNil(t, retried.Dunning)
					require.NotNil(t, retried.Dunning.NextRetryAt, "the schedule goes on")
					return
				}
				waiting := subscriptionState(t, a, sub.ID, "awaiting_method")
				require.NotNil(t, waiting.Dunning)
				require.True(t, waiting.Dunning.WaitingForNewCard)
				require.Nil(t, waiting.Dunning.NextRetryAt, "no retry is scheduled on the card")
			})
		}
	})
}

// convergeNMISubscription runs the convergence a provider notice queues for
// an NMI subscription and waits for it to complete.
func convergeNMISubscription(t *testing.T, a *authorityWorld, fleet *river.Client[pgx.Tx], sub *billing.Subscription) {
	t.Helper()
	events, unsubscribe := fleet.Subscribe(river.EventKindJobCompleted, river.EventKindJobFailed)
	defer unsubscribe()
	job, err := fleet.Insert(t.Context(), subscriptionConvergeJob{PSPID: uuid.UUID(sub.PSPID), MerchantID: a.alpha.MerchantID().UUID(), Rail: "nmi", SubscriptionReference: *sub.RailSubscriptionID}, &river.InsertOpts{Queue: openrails.QueueBilling})
	require.NoError(t, err)
	deadline := time.After(20 * time.Second)
	for {
		select {
		case event := <-events:
			if event != nil && event.Job.ID == job.Job.ID {
				require.Equal(t, river.EventKindJobCompleted, event.Kind, "%+v", event.Job.Errors)
				return
			}
		case <-deadline:
			t.Fatal("the subscription's convergence did not finish")
		}
	}
}

// awaitAttempt waits for the subscription's one attempt of kind.
func awaitAttempt(t *testing.T, a *authorityWorld, id billing.SubscriptionID, kind string) billing.PaymentAttempt {
	t.Helper()
	var found []billing.PaymentAttempt
	require.Eventually(t, func() bool {
		page, err := a.alpha.ListPaymentAttempts(t.Context(), billing.PaymentAttemptListParams{SubscriptionID: id, Kind: []string{kind}})
		require.NoError(t, err)
		found = page.Items
		return len(found) > 0
	}, 20*time.Second, 20*time.Millisecond)
	require.Len(t, found, 1)
	return found[0]
}
