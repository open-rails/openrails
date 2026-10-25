//go:build integration

package critical_test

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
	"github.com/stretchr/testify/require"
)

func TestCancellationRecoveryContracts(t *testing.T) {
	t.Run("OR-J17-S03/account-removal-retains-held-provider-delete-across-process-death", func(t *testing.T) {
		a, p, sub := nmiMembership(t)
		queue := hostFleet(t, a.world)
		first := startWorkerProcess(t, a.world, nil)
		canceled, err := a.alpha.CancelSubscription(t.Context(), sub.ID, billing.CancelSubscriptionParams{Reason: "Deleted account must never resume billing", AccountDeletion: true})
		require.NoError(t, err)
		require.Equal(t, billing.SubscriptionCanceled, canceled.Status)
		require.False(t, canceled.Resumable)
		// A merchant's terminal cancel is due immediately; the queue keeps the
		// provider-delete obligation while destructive actions are disarmed.
		require.NotNil(t, canceled.DeletionScheduledAt)
		require.Equal(t, a.clock.Now(), *canceled.DeletionScheduledAt)
		require.True(t, p.core.ScheduleLive(*sub.RailSubscriptionID))
		_, err = a.alpha.ResumeSubscription(t.Context(), sub.ID)
		require.Error(t, err, "account deletion cannot resume during its previously paid period")
		jobs, err := queue.JobList(t.Context(), river.NewJobListParams().Kinds("openrails.provider_operation"))
		require.NoError(t, err)
		require.NotEmpty(t, jobs.Jobs)
		first.kill(t)
		a.clock.Advance(8 * 24 * time.Hour)
		second := startWorkerProcess(t, a.world, nil)
		for _, old := range jobs.Jobs {
			job, err := queue.Insert(t.Context(), replayedOperation{Payload: old.EncodedArgs}, &river.InsertOpts{Queue: openrails.QueueBilling})
			require.NoError(t, err)
			require.Eventually(t, func() bool {
				row, err := queue.JobGet(t.Context(), job.Job.ID)
				return err == nil && (row.State == rivertype.JobStateScheduled || row.State == rivertype.JobStateCompleted)
			}, 15*time.Second, 20*time.Millisecond)
		}
		second.kill(t)
		retained, err := a.alpha.GetSubscription(t.Context(), sub.ID)
		require.NoError(t, err)
		require.Equal(t, billing.SubscriptionCanceled, retained.Status)
		require.Equal(t, canceled.DeletionScheduledAt, retained.DeletionScheduledAt)
		for _, original := range jobs.Jobs {
			row, err := queue.JobGet(t.Context(), original.ID)
			require.NoError(t, err)
			require.JSONEq(t, string(original.EncodedArgs), string(row.EncodedArgs), "restart must retain the original remote obligation")
		}
		require.True(t, p.core.ScheduleLive(*sub.RailSubscriptionID), "the public account-deletion contract keeps its remote obligation held until the operator arms destructive actions")
		require.Zero(t, p.core.ScheduleDeletes(*sub.RailSubscriptionID))
		_, err = a.alpha.ResumeSubscription(t.Context(), sub.ID)
		require.Error(t, err)
		p.money(t, 1, 1000)
		findings, err := a.alpha.ListFindings(t.Context(), billing.FindingListParams{Type: "life.provider_cancel.held"})
		require.NoError(t, err)
		require.Len(t, findings.Items, 1)
		require.Equal(t, sub.ID.String(), findings.Items[0].Evidence["subscription_id"])
		require.Equal(t, *sub.RailSubscriptionID, findings.Items[0].Evidence["rail_subscription_id"])
		require.Equal(t, true, findings.Items[0].Evidence["account_deletion"])
		// This documented operator SQL changes only the owned fixture's
		// deployment switch. The retained financial operation is never edited;
		// the actual registered workers must finish its original provider delete.
		enableLifecycleMaintenance(t, a)
		a.clock.Advance(20 * time.Minute)
		third := startWorkerProcess(t, a.world, nil)
		_, err = queue.Insert(t.Context(), registeredJob{KindName: "openrails.dunning"}, &river.InsertOpts{Queue: openrails.QueueBilling})
		require.NoError(t, err)
		for _, original := range jobs.Jobs {
			for range 2 {
				_, err := queue.Insert(t.Context(), replayedOperation{Payload: original.EncodedArgs}, &river.InsertOpts{Queue: openrails.QueueBilling})
				require.NoError(t, err)
			}
		}
		require.Eventually(t, func() bool { return !p.core.ScheduleLive(*sub.RailSubscriptionID) }, 20*time.Second, 20*time.Millisecond)
		require.Equal(t, 1, p.core.ScheduleDeletes(*sub.RailSubscriptionID), "replayed retained cancellation must delete the provider schedule once")
		third.kill(t)
		retained, err = a.alpha.GetSubscription(t.Context(), sub.ID)
		require.NoError(t, err)
		require.Equal(t, billing.SubscriptionCanceled, retained.Status)
		require.False(t, retained.Resumable)
		p.money(t, 1, 1000)
	})
	t.Run("OR-J17-S04/revoked-agreement-and-late-callback-cannot-restart-renewal", func(t *testing.T) {
		a, p, sub := purchasedMembership(t)
		status, raw := a.request(t, http.MethodGet, "/v1/me/agreements", a.aliceToken, nil)
		require.Equal(t, http.StatusOK, status, string(raw))
		var agreements billing.ListPage[billing.Agreement]
		require.NoError(t, json.Unmarshal(raw, &agreements))
		var recurring *billing.Agreement
		for i := range agreements.Items {
			if agreements.Items[i].Subscription != nil && agreements.Items[i].Subscription.ID == sub.ID {
				recurring = &agreements.Items[i]
			}
		}
		require.NotNil(t, recurring)
		status, raw = a.request(t, http.MethodPost, "/v1/me/agreements/"+recurring.ID.String()+"/revoke", a.aliceToken, nil)
		require.Equal(t, http.StatusOK, status, string(raw))
		var revoked billing.AgreementRevocation
		require.NoError(t, json.Unmarshal(raw, &revoked))
		require.Equal(t, "revoked", string(revoked.Agreement.Status))
		require.NotNil(t, revoked.Subscription)
		require.Equal(t, billing.SubscriptionCanceled, revoked.Subscription.Status)
		object := p.core.PaymentIntent(p.effects(t)[0].Operation)
		body := stripeNotice(t, "evt_late_after_revoke", "payment_intent.succeeded", object)
		for range 2 {
			status, raw := callback(t, a, "acct_e2e", body, stripeSignature(body, "whsec_critical_fixture"))
			require.Equal(t, http.StatusOK, status, raw)
		}
		a.clock.Advance(sub.CurrentPeriodEndsAt.Sub(a.clock.Now()) + time.Hour)
		fleet := startFleet(t, a.world)
		runRegistered(t, fleet, "openrails.dunning", 2)
		current, err := a.alpha.GetSubscription(t.Context(), sub.ID)
		require.NoError(t, err)
		require.Equal(t, billing.SubscriptionCanceled, current.Status)
		require.Equal(t, sub.CurrentPeriodEndsAt, current.CurrentPeriodEndsAt)
		access, err := a.alpha.CheckEntitlements(t.Context(), billing.CheckEntitlementsParams{CustomerID: sub.CustomerID, Entitlements: []string{"membership:paid"}})
		require.NoError(t, err)
		require.False(t, access.Entitlements["membership:paid"])
		require.Len(t, p.effects(t), 1)
		payments, err := a.alpha.ListPayments(t.Context(), billing.PaymentListParams{CustomerID: sub.CustomerID})
		require.NoError(t, err)
		require.Len(t, payments.Items, 1)
	})
	t.Run("OR-J17-S05/cancel-after-paid-boundary-has-no-resume-or-new-charge", func(t *testing.T) {
		a, p, sub := purchasedMembership(t)
		a.clock.Advance(sub.CurrentPeriodEndsAt.Sub(a.clock.Now()) + time.Hour)
		status, raw := a.request(t, http.MethodPost, "/v1/me/subscriptions/"+sub.ID.String()+"/cancel", a.aliceToken, map[string]any{"reason": "Cancel after purchased access ended"})
		require.Equal(t, http.StatusOK, status, string(raw))
		var canceled billing.Subscription
		require.NoError(t, json.Unmarshal(raw, &canceled))
		require.False(t, canceled.Resumable)
		require.Equal(t, billing.SubscriptionCanceled, canceled.Status)
		status, raw = a.request(t, http.MethodPost, "/v1/me/subscriptions/"+sub.ID.String()+"/resume", a.aliceToken, nil)
		require.Equal(t, http.StatusBadRequest, status, string(raw))
		fleet := startFleet(t, a.world)
		runRegistered(t, fleet, "openrails.dunning", 2)
		current, err := a.alpha.GetSubscription(t.Context(), sub.ID)
		require.NoError(t, err)
		require.Equal(t, billing.SubscriptionCanceled, current.Status)
		require.Equal(t, sub.CurrentPeriodEndsAt, current.CurrentPeriodEndsAt)
		access, err := a.alpha.CheckEntitlements(t.Context(), billing.CheckEntitlementsParams{CustomerID: sub.CustomerID, Entitlements: []string{"membership:paid"}})
		require.NoError(t, err)
		require.False(t, access.Entitlements["membership:paid"])
		require.Len(t, p.effects(t), 1)
	})
}
