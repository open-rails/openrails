//go:build greenfield && integration

package subscriptions_test

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// Legacy schedules changed at NMI behind OpenRails' back. With the
// destructive switch off the pull is gated and changes nothing. Armed, the
// pull reports amount, plan and pause drift for review without touching the
// schedule, reports the removed vault, and mirrors the schedule NMI deleted.
// OpenRails never writes to NMI.
func TestLegacyNMIDrift(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	monthly := w.bookTier("monthly", 999, 30)
	other := "lb_other_" + uuid.NewString()[:8]
	w.nmi.legacyPlan(other, "14.99", 30, 0)
	m := w.mirrorBook(remote, monthly, 6)
	amount, plan, paused, vaultGone, deleted, clean := m[0], m[1], m[2], m[3], m[4], m[5]
	writes := len(w.nmiWrites())

	w.nmi.editSchedule(amount.railSub, func(s *nmiSchedule) { s.Amount = "14.99" })
	w.nmi.editSchedule(plan.railSub, func(s *nmiSchedule) { s.Plan = other })
	w.nmi.editSchedule(paused.railSub, func(s *nmiSchedule) { s.Paused = true })
	w.nmi.removeVault(vaultGone.railCust)
	w.nmi.providerCancel(deleted.railSub)
	w.advance(time.Hour)

	w.pull()
	require.Empty(t, w.openFindings("pull.subscription.drift"), "a disarmed merchant's pull is gated")
	require.Equal(t, "active", w.subscription(remote, deleted.sub).Status, "nothing changes without the switch")

	w.armDestructive()
	w.pull()
	require.ElementsMatch(t, []string{amount.sub.UUID().String(), plan.sub.UUID().String(), paused.sub.UUID().String()}, w.openFindings("pull.subscription.drift"))
	require.Contains(t, w.openFindings("pull.payment_method.mismatch"), vaultGone.railCust, "the removed vault card is reported")
	require.Equal(t, "cancelled", w.subscription(remote, deleted.sub).Status, "a schedule NMI deleted is mirrored")
	for _, l := range []*legacy{amount, plan, paused, vaultGone, clean} {
		require.Equal(t, "active", w.subscription(remote, l.sub).Status, "drift is reported, never acted on")
		require.True(t, l.c.entitled(l.ent))
	}
	require.Equal(t, "14.99", w.nmi.scheduleState(amount.railSub).Amount, "the schedule is left as NMI holds it")
	require.Len(t, w.nmiWrites(), writes, "no NMI write, destructive or otherwise")
	require.Zero(t, w.nmi.saleAttempts())
}
