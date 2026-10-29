//go:build integration

package critical_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/nmimock"
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
	t.Run("OR-J16-S05/a-failing-due-pass-holds-back-no-other-merchants-renewal", func(t *testing.T) {
		a, provider, stuck := purchasedMembershipWith(t, 24, func(w *world) {
			beta := w.cfg.Merchants["beta"]
			beta.PSPs = openrails.PSPs{"stripe": openrails.StripePSP{AccountID: "acct_e2e_beta", Environment: openrails.EnvironmentTest, SecretKey: "sk_test_critical", WebhookSigningSecret: "whsec_critical_beta", PublishableKey: "pk_test_critical"}.PSPConfig()}
			beta.CardVault = "stripe"
			w.cfg.Merchants["beta"] = beta
		})
		a.clock.Advance(time.Hour)
		a.bindCustomers(t, "beta")
		member := buyMembership(t, a, a.beta, provider, 24)
		stuckDue, memberDue := *stuck.CurrentPeriodEndsAt, *member.CurrentPeriodEndsAt
		// Alpha's member can be neither renewed nor recorded as refused, so
		// every pass that reaches it fails.
		storageFault(t, a.world, a.schema, "provider_intents", fmt.Sprintf("NEW.subscription_id = '%s'", stuck.ID.UUID()))
		storageFault(t, a.world, a.schema, "reconciliation_findings", fmt.Sprintf("NEW.subject_key = '%s'", stuck.ID.UUID()))
		a.clock.Advance(stuckDue.Sub(a.clock.Now()))
		// The fleet's own periodic schedule queues the passes: one on start,
		// then one a minute.
		fleet := hostFleet(t, a.world)
		finished, unsubscribe := fleet.Subscribe(river.EventKindJobCompleted, river.EventKindJobFailed)
		defer unsubscribe()
		require.NoError(t, a.engine.Start(t.Context(), openrails.WithRiverClient(fleet)))
		require.NoError(t, fleet.Start(t.Context()))
		global := func(job *rivertype.JobRow) bool {
			var args struct {
				MerchantID uuid.UUID `json:"merchant_id"`
			}
			return job.Kind == "openrails.dunning" && json.Unmarshal(job.EncodedArgs, &args) == nil && args.MerchantID == uuid.Nil
		}
		pass := func(within time.Duration) *river.Event {
			deadline := time.After(within)
			for {
				select {
				case event := <-finished:
					if event != nil && global(event.Job) {
						return event
					}
				case <-deadline:
					t.Fatal("no deployment-wide due pass finished")
				}
			}
		}
		// A pass waiting to retry is one not finished that has failed before.
		retrying := func() (waiting []int64) {
			jobs, err := fleet.JobList(t.Context(), river.NewJobListParams().Kinds("openrails.dunning"))
			require.NoError(t, err)
			for _, job := range jobs.Jobs {
				if global(job) && job.FinalizedAt == nil && len(job.Errors) > 0 {
					waiting = append(waiting, job.ID)
				}
			}
			return waiting
		}
		first := pass(30 * time.Second)
		require.Equal(t, river.EventKindJobFailed, first.Kind, "the pass reports the member it could not finish")
		job, err := fleet.JobGet(t.Context(), first.Job.ID)
		require.NoError(t, err)
		require.Equal(t, rivertype.JobStateDiscarded, job.State, "a failed pass is not retried: the next minute's pass is its retry")
		require.Empty(t, retrying())
		a.clock.Advance(memberDue.Sub(a.clock.Now()))
		second := pass(90 * time.Second)
		require.NotEqual(t, first.Job.ID, second.Job.ID, "the next minute's pass is queued, not absorbed")
		require.Eventually(t, func() bool {
			current, err := a.beta.GetSubscription(t.Context(), member.ID)
			return err == nil && current.CurrentPeriodStartsAt != nil && current.CurrentPeriodStartsAt.Equal(memberDue)
		}, 30*time.Second, 50*time.Millisecond, "the other merchant's member renews on the next minute's pass")
		renewals, err := a.beta.ListRenewals(t.Context(), billing.RenewalListParams{SubscriptionID: member.ID})
		require.NoError(t, err)
		require.Len(t, renewals.Items, 1)
		require.Equal(t, "collected", renewals.Items[0].Outcome)
		current, err := a.alpha.GetSubscription(t.Context(), stuck.ID)
		require.NoError(t, err)
		require.Equal(t, stuckDue, *current.CurrentPeriodEndsAt, "the stuck member stays as it was")
		require.Empty(t, retrying())
	})
	t.Run("OR-J16-S30/a-seen-nmi-decline-opens-dunning-in-one-transaction", func(t *testing.T) {
		a, p, sub := nmiMembership(t)
		due := *sub.CurrentPeriodEndsAt
		sale := p.core.AddScheduleSale(*sub.RailSubscriptionID, due)
		p.core.EditSale(sale.TransactionID, func(s *nmimock.Sale) { s.Declined = "202" })
		a.clock.Advance(due.Sub(a.clock.Now()) + time.Hour)
		fleet := hostFleet(t, a.world)
		fleet.PeriodicJobs().Clear()
		require.NoError(t, a.engine.Start(t.Context(), openrails.WithRiverClient(fleet)))
		require.NoError(t, fleet.Start(t.Context()))
		events, unsubscribe := fleet.Subscribe(river.EventKindJobCompleted, river.EventKindJobFailed)
		defer unsubscribe()
		finished := func(id int64) river.EventKind {
			deadline := time.After(20 * time.Second)
			for {
				select {
				case event := <-events:
					if event != nil && event.Job.ID == id {
						return event.Kind
					}
				case <-deadline:
					t.Fatal("the subscription's convergence did not finish")
				}
			}
		}
		// The decline's notice is read; the dunning writer fails once it is.
		restore := storageFault(t, a.world, a.schema, "notification_events", "NEW.event_type IN ('payment_method_failed', 'payment_method_update_required')")
		job, err := fleet.Insert(t.Context(), subscriptionConvergeJob{PSPID: uuid.UUID(sub.PSPID), MerchantID: a.alpha.MerchantID().UUID(), Rail: "nmi", SubscriptionReference: *sub.RailSubscriptionID}, &river.InsertOpts{Queue: openrails.QueueBilling})
		require.NoError(t, err)
		require.Equal(t, river.EventKindJobFailed, finished(job.Job.ID))
		current, err := a.alpha.GetSubscription(t.Context(), sub.ID)
		require.NoError(t, err)
		require.Equal(t, billing.SubscriptionActive, current.Status, "a failed dunning write leaves the row as it was")
		restore()
		_, err = fleet.JobRetry(t.Context(), job.Job.ID)
		require.NoError(t, err)
		require.Equal(t, river.EventKindJobCompleted, finished(job.Job.ID))
		dunned := subscriptionState(t, a, sub.ID, "past_due")
		require.NotNil(t, dunned.Dunning)
		require.NotNil(t, dunned.Dunning.NextRetryAt, "a dunned decline schedules its retry")
	})
	t.Run("OR-J16-S31/held-awaiting-method-expiry-waits-for-its-recheck", func(t *testing.T) {
		a, p, sub := purchasedMembership(t, 7*24)
		due := *sub.CurrentPeriodEndsAt
		p.core.SetDecline("4242", "lost_card")
		a.clock.Advance(due.Sub(a.clock.Now()))
		fleet := hostFleet(t, a.world)
		fleet.PeriodicJobs().Clear()
		require.NoError(t, a.engine.Start(t.Context(), openrails.WithRiverClient(fleet)))
		require.NoError(t, fleet.Start(t.Context()))
		runRegistered(t, fleet, "openrails.dunning", 1)
		waiting := subscriptionState(t, a, sub.ID, "awaiting_method")
		a.clock.Advance(waiting.Dunning.FinalRetryAt.Sub(a.clock.Now()))
		held := func() billing.Finding {
			findings, err := a.alpha.ListFindings(t.Context(), billing.FindingListParams{Type: "life.terminal_outcome.held"})
			require.NoError(t, err)
			require.Len(t, findings.Items, 1)
			return findings.Items[0]
		}
		// The kill switch is off: the expiry is held, and not retried every pass.
		runRegistered(t, fleet, "openrails.dunning", 1)
		seen := held().LastSeenAt
		runRegistered(t, fleet, "openrails.dunning", 1)
		require.Equal(t, seen, held().LastSeenAt, "a held expiry leaves the due pass until its recheck")
		subscriptionState(t, a, sub.ID, "awaiting_method")
		enableLifecycleMaintenance(t, a)
		a.clock.Advance(time.Hour)
		runRegistered(t, fleet, "openrails.dunning", 1)
		subscriptionState(t, a, sub.ID, "canceled")
	})
	t.Run("OR-J16-S32/due-pass-queues-an-nmi-retry-without-waiting-on-the-gateway", func(t *testing.T) {
		a, p, sub := nmiMembership(t)
		due := *sub.CurrentPeriodEndsAt
		p.core.EditSchedule(*sub.RailSubscriptionID, func(s *nmimock.Schedule) { s.NextBilling = due.Add(7 * 24 * time.Hour) })
		a.clock.Advance(due.Sub(a.clock.Now()) + 25*time.Hour)
		fleet := hostFleet(t, a.world)
		fleet.PeriodicJobs().Clear()
		require.NoError(t, a.engine.Start(t.Context(), openrails.WithRiverClient(fleet)))
		require.NoError(t, fleet.Start(t.Context()))
		runRegistered(t, fleet, "openrails.rebill_watch", 1)
		hold := p.core.Hold(func(r *http.Request) bool { return strings.HasSuffix(r.URL.Path, "/transact.php") }, nmimock.HoldRequest)
		defer hold.Release()
		runRegistered(t, fleet, "openrails.dunning", 1)
		select {
		case <-hold.Arrived():
		case <-time.After(20 * time.Second):
			t.Fatal("the queued retry never charged")
		}
		hold.Release()
		nmiRenewalReadback(t, a, p, sub)
	})
}

// subscriptionConvergeJob is the convergence a subscription's provider
// notice queues.
type subscriptionConvergeJob struct {
	PSPID                 uuid.UUID `json:"psp_id"`
	MerchantID            uuid.UUID `json:"merchant_id"`
	Rail                  string    `json:"rail"`
	SubscriptionReference string    `json:"subscription_reference"`
}

func (subscriptionConvergeJob) Kind() string { return "openrails.subscription_converge" }
