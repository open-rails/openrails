//go:build integration

package critical_test

import (
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/billing"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
	"github.com/stretchr/testify/require"
)

// docs/operations.md defines this operator SQL switch as deployment setup.
// Arm only the scenario's disposable schema; lifecycle outcomes still run
// through the actual admission gate and registered workers.
func enableLifecycleMaintenance(t *testing.T, a *authorityWorld) {
	t.Helper()
	result, err := a.pool.Exec(t.Context(), "UPDATE "+pgx.Identifier{a.schema, "destructive_action_switch"}.Sanitize()+" SET enabled=true, updated_by='critical fixture operator', reason='owned disposable lifecycle test'")
	require.NoError(t, err)
	require.EqualValues(t, 1, result.RowsAffected())
}

func subscriptionState(t *testing.T, a *authorityWorld, id billing.SubscriptionID, status string) *billing.Subscription {
	t.Helper()
	var result *billing.Subscription
	require.Eventually(t, func() bool {
		var err error
		result, err = a.alpha.GetSubscription(t.Context(), id)
		return err == nil && string(result.Status) == status
	}, 20*time.Second, 20*time.Millisecond, "subscription must reach %s", status)
	return result
}

func TestDunning(t *testing.T) {
	t.Run("OR-J16-S01/retryable-decline-recovers-on-bounded-weekly-schedule", func(t *testing.T) {
		a, p, sub := purchasedMembership(t, 7*24)
		due := *sub.CurrentPeriodEndsAt
		p.core.SetDecline("4242", "insufficient_funds")
		a.clock.Advance(due.Sub(a.clock.Now()))
		fleet := startFleet(t, a.world)
		runRegistered(t, fleet, "openrails.dunning", 2)
		declined := subscriptionState(t, a, sub.ID, "past_due")
		require.NotNil(t, declined.Dunning)
		require.Equal(t, 1, declined.Dunning.Attempts)
		require.Equal(t, 2, *declined.Dunning.RetriesLeft)
		require.Equal(t, due.Add(24*time.Hour), *declined.Dunning.NextRetryAt)
		require.Equal(t, due.Add(48*time.Hour), *declined.Dunning.FinalRetryAt)
		require.Equal(t, billing.DeclineInsufficientFunds, *declined.Dunning.LastFailureReason)
		require.Len(t, p.effects(t), 1, "a declined renewal is not a debit")
		attempts, err := a.alpha.ListPaymentAttempts(t.Context(), billing.PaymentAttemptListParams{SubscriptionID: sub.ID})
		require.NoError(t, err)
		attemptCount := len(attempts.Items)
		a.clock.Advance(24*time.Hour - time.Microsecond)
		runRegistered(t, fleet, "openrails.dunning", 2)
		before, err := a.alpha.ListPaymentAttempts(t.Context(), billing.PaymentAttemptListParams{SubscriptionID: sub.ID})
		require.NoError(t, err)
		require.Len(t, before.Items, attemptCount, "retry may not run before its accepted schedule")
		p.core.SetDecline("4242", "")
		a.clock.Advance(time.Microsecond)
		runRegistered(t, fleet, "openrails.dunning", 2)
		renewalSettled(t, a, p, sub, due)
		recovered, err := a.alpha.GetSubscription(t.Context(), sub.ID)
		require.NoError(t, err)
		require.Nil(t, recovered.Dunning)
		cycles, err := a.alpha.ListRenewals(t.Context(), billing.RenewalListParams{SubscriptionID: sub.ID})
		require.NoError(t, err)
		require.Equal(t, "declined", cycles.Items[0].FirstOutcome)
		require.Equal(t, "dunning_retry", cycles.Items[0].RecoveredBy)
	})
	t.Run("OR-J16-S02/withdrawn-mandate-stops-future-billing", func(t *testing.T) {
		a, p, sub := purchasedMembership(t, 7*24)
		enableLifecycleMaintenance(t, a)
		due := *sub.CurrentPeriodEndsAt
		p.core.SetDecline("4242", "stolen_card")
		a.clock.Advance(due.Sub(a.clock.Now()))
		fleet := startFleet(t, a.world)
		runRegistered(t, fleet, "openrails.dunning", 2)
		ended := subscriptionState(t, a, sub.ID, "canceled")
		require.False(t, ended.Resumable)
		require.NotNil(t, ended.CanceledAt)
		access, err := a.alpha.CheckEntitlements(t.Context(), billing.CheckEntitlementsParams{CustomerID: sub.CustomerID, Entitlements: []string{"membership:paid"}})
		require.NoError(t, err)
		require.False(t, access.Entitlements["membership:paid"])
		p.core.SetDecline("4242", "")
		a.clock.Advance(8 * 24 * time.Hour)
		runRegistered(t, fleet, "openrails.dunning", 2)
		require.Len(t, p.effects(t), 1, "a later healthy card cannot revive a canceled mandate")
		current, err := a.alpha.GetSubscription(t.Context(), sub.ID)
		require.NoError(t, err)
		require.Equal(t, ended.Status, current.Status)
	})
	t.Run("OR-J16-S03/awaiting-method-expires-at-declared-deadline", func(t *testing.T) {
		a, p, sub := purchasedMembership(t, 7*24)
		enableLifecycleMaintenance(t, a)
		due := *sub.CurrentPeriodEndsAt
		p.core.SetDecline("4242", "lost_card")
		a.clock.Advance(due.Sub(a.clock.Now()))
		fleet := startFleet(t, a.world)
		runRegistered(t, fleet, "openrails.dunning", 1)
		waiting := subscriptionState(t, a, sub.ID, "awaiting_method")
		require.NotNil(t, waiting.Dunning)
		require.True(t, waiting.Dunning.WaitingForNewCard)
		require.Nil(t, waiting.Dunning.NextRetryAt)
		require.NotNil(t, waiting.Dunning.FinalRetryAt)
		deadline := *waiting.Dunning.FinalRetryAt
		require.Equal(t, due.Add(3*24*time.Hour), deadline, "weekly two-day retry window plus one-day allowance")
		a.clock.Advance(deadline.Sub(a.clock.Now()) - time.Microsecond)
		runRegistered(t, fleet, "openrails.dunning", 1)
		subscriptionState(t, a, sub.ID, "awaiting_method")
		a.clock.Advance(time.Microsecond)
		runRegistered(t, fleet, "openrails.dunning", 1)
		subscriptionState(t, a, sub.ID, "canceled")
		access, err := a.alpha.CheckEntitlements(t.Context(), billing.CheckEntitlementsParams{CustomerID: sub.CustomerID, Entitlements: []string{"membership:paid"}})
		require.NoError(t, err)
		require.False(t, access.Entitlements["membership:paid"])
		require.Len(t, p.effects(t), 1)
	})
	t.Run("OR-J16-S04/unknown-renewal-is-not-a-decline-or-replacement-obligation", func(t *testing.T) {
		a, p, sub := purchasedMembership(t, 7*24)
		due := *sub.CurrentPeriodEndsAt
		gate := &wireGate{arrived: make(chan struct{}), release: make(chan struct{})}
		defer gate.unblock()
		p.mu.Lock()
		p.responseGate = gate
		p.mu.Unlock()
		p.interruptPaymentResponses()
		a.clock.Advance(due.Sub(a.clock.Now()))
		fleet := startFleet(t, a.world)
		runRegistered(t, fleet, "openrails.dunning", 2)
		require.Eventually(t, func() bool { return len(p.effects(t)) == 2 }, 15*time.Second, 20*time.Millisecond)
		// The adapter first checks authoritative history before submitting a
		// renewal. Cut evidence only AFTER acceptance, so the fixture actually
		// creates ambiguous money rather than refusing the original charge.
		p.unavailableEvidence(true)
		paused, unsubscribe := fleet.Subscribe(river.EventKindJobSnoozed, river.EventKindJobFailed)
		defer unsubscribe()
		awaitPause := func() {
			deadline := time.After(15 * time.Second)
			for {
				select {
				case event := <-paused:
					if event.Job.Kind == "openrails.provider_operation" {
						require.Equal(t, river.EventKindJobSnoozed, event.Kind)
						return
					}
				case <-deadline:
					t.Fatal("unknown collection was not durably retained for verification")
				}
			}
		}
		gate.unblock()
		awaitPause()
		current, err := a.alpha.GetSubscription(t.Context(), sub.ID)
		require.NoError(t, err)
		require.Equal(t, *sub.CurrentPeriodEndsAt, *current.CurrentPeriodEndsAt)
		require.Nil(t, current.Dunning, "no authoritative decline exists")
		payments, err := a.alpha.ListPayments(t.Context(), billing.PaymentListParams{CustomerID: sub.CustomerID})
		require.NoError(t, err)
		require.Len(t, payments.Items, 1, "accepted remote debit is not yet local payment proof")
		a.clock.Advance(24 * time.Hour)
		runRegistered(t, fleet, "openrails.dunning", 2)
		awaitPause()
		require.Len(t, p.effects(t), 2, "uncertainty cannot authorize a second collection")
		p.restorePaymentResponses()
		p.unavailableEvidence(false)
		a.clock.Advance(time.Hour)
		jobs, err := fleet.JobList(t.Context(), river.NewJobListParams().Kinds("openrails.provider_operation"))
		require.NoError(t, err)
		for _, job := range jobs.Jobs {
			if job.State != rivertype.JobStateCompleted && job.State != rivertype.JobStateRunning {
				_, err = fleet.JobRetry(t.Context(), job.ID)
				require.NoError(t, err)
			}
		}
		renewalSettled(t, a, p, sub, due)
	})
}
