//go:build greenfield && integration

package subscriptions_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

var mirrorCadences = []struct {
	name  string
	days  int
	cents int64
}{{"daily", 1, 199}, {"monthly", 30, 999}, {"yearly", 365, 9999}}

// mirrorBook imports n active legacy members of tier whose paid period ends
// half a cycle from now, converges, and returns them.
func (w *world) mirrorBook(tp topology, tier bookTier, n int) []*legacy {
	w.t.Helper()
	b := w.newLegacyBook()
	paid := w.clock.Now().Add(time.Duration(tier.days) * 12 * time.Hour)
	for range n {
		b.add(&bookRow{source: "m-" + uuid.NewString()[:8], tier: tier, paid: paid, declared: true})
	}
	result, err := w.client[tp].ImportBilling(w.t.Context(), b.book)
	require.NoError(w.t, err)
	require.Len(w.t, result.Imported, n, "%+v", result)
	w.settle()
	w.converge()
	out := make([]*legacy, 0, n)
	for _, r := range b.rows {
		sub := r.sub(w, tp)
		require.Equal(w.t, "active", sub.Status)
		l := &legacy{w: w, rail: "nmi", tp: tp, c: r.c, price: tier.price, railSub: r.schedule, sub: sub.ID, ent: tier.ent, railCust: r.vault}
		require.True(w.t, l.c.entitled(l.ent))
		out = append(out, l)
	}
	return out
}

// nmiNext is the schedule's next billing date as NMI reports it (a date).
func (l *legacy) nmiNext() time.Time {
	return l.w.nmi.scheduleState(l.railSub).NextBilling.UTC().Truncate(24 * time.Hour)
}

// requireMirrored asserts the local mirror of an NMI-billed membership:
// exactly one local capture per approved NMI sale, the period ending on
// NMI's next billing date, continuous access, and no OpenRails charge.
func (l *legacy) requireMirrored(what string) {
	t := l.w.t
	t.Helper()
	require.Equal(t, l.w.nmiCharges(l.railCust), l.w.localCharges(l.tp, l.c.id), "%s: one local payment per NMI sale", what)
	sub := l.w.subscription(l.tp, l.sub)
	require.Equal(t, "active", sub.Status, what)
	require.True(t, l.nmiNext().Equal(*sub.CurrentPeriodEndsAt), "%s: period ends on NMI's next billing date (%s vs %s)", what, l.nmiNext(), sub.CurrentPeriodEndsAt)
	require.True(t, l.c.entitled(l.ent), "%s: access is continuous", what)
	require.Zero(t, l.w.nmi.saleAttempts(), "%s: OpenRails never charges", what)
}

// NMI's renewals reach the mirror through signed webhooks, delivered once,
// twice, late, or reporting a decline or NMI's own cancellation.
func TestLegacyNMIMirrorWebhooks(t *testing.T) {
	t.Parallel()
	for i, cadence := range mirrorCadences {
		t.Run(cadence.name, func(t *testing.T) {
			t.Parallel()
			tp := []topology{embedded, remote}[i%2]
			w := newWorld(t)
			w.armDestructive()
			tier := w.bookTier(cadence.name, cadence.cents, cadence.days)
			m := w.mirrorBook(tp, tier, 5)
			once, twice, late, declined, cancelled := m[0], m[1], m[2], m[3], m[4]
			end := *w.subscription(tp, once.sub).CurrentPeriodEndsAt
			w.advance(end.Sub(w.clock.Now()) + time.Hour)

			notices := map[*legacy]obj{}
			for _, l := range []*legacy{once, twice, late} {
				notices[l] = l.providerRenewal(true)
			}
			require.Equal(t, http.StatusOK, w.deliver("nmi", notices[once]))
			once.requireMirrored("once")
			require.Equal(t, http.StatusOK, w.deliver("nmi", notices[twice]))
			require.Equal(t, http.StatusOK, w.deliver("nmi", notices[twice]))
			twice.requireMirrored("duplicated")

			require.Equal(t, http.StatusOK, w.deliver("nmi", declined.providerRenewal(false)))
			sub := w.subscription(tp, declined.sub)
			require.Equal(t, "past_due", sub.Status, "NMI's failed renewal is mirrored")
			require.True(t, sub.CurrentPeriodEndsAt.Equal(end), "an unpaid period is never granted")
			require.True(t, declined.c.entitled(declined.ent), "NMI's own dunning keeps standing access")
			require.Empty(t, w.localCharges(tp, declined.c.id)[1:], "a decline is not a charge")

			require.Equal(t, http.StatusOK, w.deliver("nmi", cancelled.providerCancelNotice()))
			require.Equal(t, "cancelled", w.subscription(tp, cancelled.sub).Status, "NMI's cancellation is mirrored")

			// Delivered a quarter cycle late, still once.
			w.advance(time.Duration(cadence.days) * 6 * time.Hour)
			require.Equal(t, http.StatusOK, w.deliver("nmi", notices[late]))
			late.requireMirrored("late")
			require.Zero(t, w.nmi.saleAttempts())
			require.Zero(t, w.nmi.deletesOf(cancelled.railSub), "a schedule NMI ended is never deleted again")
		})
	}
}

// Two NMI renewals notified newest first, then a stale schedule notice: both
// charges land once and the period follows NMI's latest date.
func TestLegacyNMIMirrorOutOfOrder(t *testing.T) {
	t.Parallel()
	for i, cadence := range mirrorCadences {
		t.Run(cadence.name, func(t *testing.T) {
			t.Parallel()
			tp := []topology{remote, embedded}[i%2]
			w := newWorld(t)
			tier := w.bookTier(cadence.name, cadence.cents, cadence.days)
			l := w.mirrorBook(tp, tier, 1)[0]
			end := *w.subscription(tp, l.sub).CurrentPeriodEndsAt
			w.advance(end.Sub(w.clock.Now()) + time.Duration(cadence.days)*24*time.Hour + time.Hour)
			first := l.providerRenewal(true)
			second := l.providerRenewal(true)
			require.Equal(t, http.StatusOK, w.deliver("nmi", second))
			require.Equal(t, http.StatusOK, w.deliver("nmi", first))
			require.Equal(t, http.StatusOK, w.deliver("nmi", l.staleNotice()))
			require.Len(t, w.localCharges(tp, l.c.id), 3, "the imported charge and both renewals")
			l.requireMirrored("out of order")
		})
	}
}

// A renewal whose webhook never arrives is recovered by the provider pull:
// the lapsed row parks for verification, and the pull's per-schedule probe
// records every NMI charge since and renews it. A renewal notified AND pulled
// lands once. A schedule NMI deleted without notice is mirrored as cancelled.
func TestLegacyNMIMirrorPull(t *testing.T) {
	t.Parallel()
	for i, cadence := range mirrorCadences {
		t.Run(cadence.name, func(t *testing.T) {
			t.Parallel()
			tp := []topology{remote, embedded}[i%2]
			w := newWorld(t)
			w.armDestructive()
			tier := w.bookTier(cadence.name, cadence.cents, cadence.days)
			m := w.mirrorBook(tp, tier, 3)
			missed, both, gone := m[0], m[1], m[2]
			end := *w.subscription(tp, missed.sub).CurrentPeriodEndsAt
			w.advance(end.Sub(w.clock.Now()) + 49*time.Hour)
			// NMI bills every due period meanwhile (three for a daily plan).
			var notice obj
			for _, l := range []*legacy{missed, both} {
				for !l.w.nmi.scheduleState(l.railSub).NextBilling.After(w.clock.Now()) {
					notice = l.providerRenewal(true)
				}
			}
			require.Equal(t, http.StatusOK, w.deliver("nmi", notice))
			both.requireMirrored("notified")
			w.nmi.providerCancel(gone.railSub)

			w.converge()
			w.pull()
			missed.requireMirrored("missing webhook recovered by pull")
			both.requireMirrored("notified and pulled")
			require.Equal(t, "cancelled", w.subscription(tp, gone.sub).Status, "a schedule NMI deleted is mirrored by the pull")
			require.Zero(t, w.nmi.deletesOf(gone.railSub))
			require.Zero(t, w.nmi.saleAttempts())
		})
	}
}
