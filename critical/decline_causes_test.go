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
	"github.com/riverqueue/river"
	"github.com/stretchr/testify/require"
)

// Each refusal is stored with what caused it: the customer, the issuer's
// discretion, suspected fraud, the merchant or gateway, or processing.
func TestDeclineCauses(t *testing.T) {
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
					require.NotNil(t, retried.Dunning.NextRetryAt, "the schedule goes on")
					return
				}
				waiting := subscriptionState(t, a, sub.ID, "awaiting_method")
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
