//go:build e2e && integration

package subscriptions_test

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/nmimock"
)

// Provider drift remains visible with the destructive switch off. Arming does
// not bypass unresolved account evidence: neither provider writes nor local
// cancellations resume until ordinary observation confirms the drift is gone.
func TestLegacyNMIDrift(t *testing.T) {
	t.Parallel()
	w := newWorld(t)

	monthly := w.bookTier("monthly", 999, 30)
	other := "lb_other_" + uuid.NewString()[:8]
	w.nmi.AddPlan(nmimock.Plan{ID: other, Name: "Legacy " + other, Amount: "14.99", Days: 30})
	m := w.mirrorBook(remote, monthly, 6)
	amount, plan, paused, vaultGone, deleted, clean := m[0], m[1], m[2], m[3], m[4], m[5]
	w.refreshProviders()
	w.settleCollectionScans()
	writes := len(w.nmiWrites())
	pullHeld := func() {
		t.Helper()
		job, err := w.jobs.Insert(t.Context(), refreshMerchant{MerchantID: w.client[embedded].MerchantID().UUID()}, &river.InsertOpts{Queue: openrails.QueueBilling})
		require.NoError(t, err)
		require.Eventually(t, func() bool {
			row, err := w.jobs.JobGet(t.Context(), job.Job.ID)
			return err == nil && row.State == rivertype.JobStateRetryable
		}, 20*time.Second, 20*time.Millisecond, "observed drift keeps financial recovery incomplete")
	}

	w.nmi.EditSchedule(amount.railSub, func(s *nmimock.Schedule) { s.Amount = "14.99" })
	w.nmi.EditSchedule(plan.railSub, func(s *nmimock.Schedule) { s.Plan, s.Amount = other, "14.99" })
	w.nmi.EditSchedule(paused.railSub, func(s *nmimock.Schedule) { s.Paused = true })
	w.nmi.RemoveVault(vaultGone.railCust)
	w.nmi.DeleteSchedule(deleted.railSub)
	w.advance(time.Hour)

	pullHeld()
	drifted := []string{amount.sub.UUID().String(), plan.sub.UUID().String(), paused.sub.UUID().String()}
	require.ElementsMatch(t, drifted, w.openFindings("pull.subscription.drift"), "the switch blocks effects, not provider observation")
	require.Equal(t, billing.SubscriptionActive, w.subscription(remote, deleted.sub).Status, "nothing changes without the switch")

	w.armDestructive()
	pullHeld()
	require.ElementsMatch(t, drifted, w.openFindings("pull.subscription.drift"))
	require.Contains(t, w.openFindings("pull.payment_method.mismatch"), vaultGone.railCust, "the removed vault card is reported")
	require.Equal(t, billing.SubscriptionActive, w.subscription(remote, deleted.sub).Status, "arming cannot bypass unresolved provider drift")
	require.Equal(t, "14.99", w.nmi.Schedule(amount.railSub).Amount, "the schedule is left as NMI holds it")
	require.Equal(t, other, w.nmi.Schedule(plan.railSub).Plan)
	require.True(t, w.nmi.Schedule(paused.railSub).Paused)

	// The operator corrects these schedules at the provider; OpenRails only
	// reads that correction. A subsequent armed pass can mirror the deletion.
	w.nmi.EditSchedule(amount.railSub, func(s *nmimock.Schedule) { s.Amount = monthly.amount })
	w.nmi.EditSchedule(plan.railSub, func(s *nmimock.Schedule) { s.Plan, s.Amount = monthly.plan, monthly.amount })
	w.nmi.EditSchedule(paused.railSub, func(s *nmimock.Schedule) { s.Paused = false })
	w.pull()
	w.pull()
	require.Empty(t, w.openFindings("pull.subscription.drift"))
	require.Equal(t, billing.SubscriptionCanceled, w.subscription(remote, deleted.sub).Status, "a schedule NMI deleted is mirrored once recovery completes")
	require.True(t, deleted.c.entitled(deleted.ent), "canceling an NMI schedule preserves its already-paid access")
	for _, l := range []*legacy{amount, plan, paused, vaultGone, clean} {
		require.Equal(t, billing.SubscriptionActive, w.subscription(remote, l.sub).Status, "drift is reported, never acted on")
		require.True(t, l.c.entitled(l.ent))
	}
	require.Len(t, w.nmiWrites(), writes, "no NMI write, destructive or otherwise")
	require.Zero(t, len(w.nmi.Attempts()))
}
