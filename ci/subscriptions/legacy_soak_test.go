//go:build e2e && integration

package subscriptions_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/nmimock"
)

// With provider deletes disarmed, a revoke_access refund of an NMI-billed
// payment is refused (NMI would keep charging a member without access); armed,
// it ends the membership, deletes the schedule once, and nothing restores the
// access later.
func TestLegacyNMIRefundRevokeEndsMembership(t *testing.T) {
	t.Parallel()
	for _, tp := range []topology{embedded, remote} {
		t.Run(string(tp), func(t *testing.T) {
			t.Parallel()
			w := newWorld(t)
			l := importLegacy(t, w, "nmi", tp)
			w.refreshProviders()
			w.settleCollectionScans()
			paid := completed(w.payments(tp, l.c.id))
			require.Len(t, paid, 1)
			legacy := paid[0]
			params := billing.RefundPaymentParams{Full: true, Reason: "requested_by_customer", RevokeAccess: true, IdempotencyKey: "refund-" + legacy.ID.String()}

			_, err := w.client[tp].RefundPayment(t.Context(), legacy.ID, params)
			requireCode(t, err, http.StatusConflict, billing.CodeProviderCancelHeld)
			w.settle()
			require.Empty(t, w.nmi.CallsTo(http.MethodPost, "/payments/"+legacy.TransactionID+"/refund", nil), "nothing refunded")
			require.Equal(t, billing.SubscriptionActive, w.subscription(tp, l.sub).Status)
			require.True(t, l.c.entitled(l.ent))
			require.Contains(t, w.openFindings(providerCancelHeld), l.sub.UUID().String())

			w.armDestructive()
			_, err = w.client[tp].RefundPayment(t.Context(), legacy.ID, params)
			require.NoError(t, err)
			w.settle()
			require.Len(t, w.nmi.CallsTo(http.MethodPost, "/payments/"+legacy.TransactionID+"/refund", nil), 1)
			require.Equal(t, billing.SubscriptionCanceled, w.subscription(tp, l.sub).Status)
			w.until(func() bool { return w.nmi.ScheduleDeletes(l.railSub) > 0 }, "the NMI schedule delete")
			require.False(t, l.c.entitled(l.ent))

			require.Equal(t, http.StatusOK, w.deliver("nmi", l.staleNotice()))
			require.Equal(t, http.StatusOK, w.deliver("nmi", w.refundNotice("nmi")))
			w.converge()
			w.pull()
			w.advance(time.Hour)
			w.wake()
			w.converge()
			require.False(t, l.c.entitled(l.ent), "revoked access stays revoked")
			require.Equal(t, billing.SubscriptionCanceled, w.subscription(tp, l.sub).Status)
			require.Equal(t, 1, w.nmi.ScheduleDeletes(l.railSub))
			require.Zero(t, len(w.nmi.Attempts()))
			require.Empty(t, w.nmi.Unexpected())
		})
	}
}

// An imported book grants access by itself: active, mid-dunning in grace and
// canceled-with-runway members are entitled right after ImportBilling
// returns, with no operator Converge.
func TestLegacyNMIImportDerivesAccess(t *testing.T) {
	t.Parallel()
	for _, tp := range []topology{embedded, remote} {
		t.Run(string(tp), func(t *testing.T) {
			t.Parallel()
			w := newWorld(t)
			now := w.clock.Now()
			monthly := w.bookTier("monthly", 999, 30)
			b := w.newLegacyBook()
			active := b.add(&bookRow{source: "active", tier: monthly, paid: now.Add(10 * day), declared: true})
			last := now.Add(-12 * time.Hour)
			pastDue := b.add(&bookRow{source: "past_due", tier: monthly, paid: now.Add(-day), declared: true,
				dunning: &billing.DunningEvidence{Retries: 1, LastRetryAt: &last, ScheduleLive: true}})
			w.nmi.EditSchedule(pastDue.schedule, func(s *nmimock.Schedule) { s.NextBilling = pastDue.paid.AddDate(0, 0, 30) })
			runway := b.add(&bookRow{source: "runway", tier: monthly, paid: now.Add(20 * day), declared: true,
				cancel: billing.CancelEvidence{Kind: "user_canceled", At: now.Add(-5 * day)}})
			w.nmi.DeleteSchedule(runway.schedule)

			result, err := w.client[tp].ImportBilling(t.Context(), b.book)
			require.NoError(t, err)
			require.Len(t, result.Imported, 3, "%+v", result)
			for _, row := range []*bookRow{active, pastDue, runway} {
				require.True(t, row.c.entitled(monthly.ent), "%s is entitled right after import", row.source)
			}
			require.Equal(t, billing.SubscriptionPastDue, pastDue.sub(w, tp).Status)
			require.Equal(t, billing.SubscriptionCanceled, runway.sub(w, tp).Status)

			// A replay changes nothing and stays entitled.
			replay, err := w.client[tp].ImportBilling(t.Context(), b.book)
			require.NoError(t, err)
			require.Empty(t, replay.Imported)
			require.True(t, active.c.entitled(monthly.ent))
			require.Zero(t, len(w.nmi.Attempts()))
		})
	}
}

// A schedule NMI deleted without a notification is mirrored as soon as the
// host asks for a provider refresh through its Client, embedded or remote.
func TestLegacyNMIRefreshPSPs(t *testing.T) {
	t.Parallel()
	for _, tp := range []topology{embedded, remote} {
		t.Run(string(tp), func(t *testing.T) {
			t.Parallel()
			w := newWorld(t)
			w.armDestructive()
			// A second member keeps the NMI roster non-empty: an empty roster never
			// proves absence.
			l := w.mirrorBook(tp, w.bookTier("monthly", 999, 30), 2)[0]
			w.nmi.DeleteSchedule(l.railSub)
			w.advance(time.Hour)

			res, err := w.client[tp].RefreshPSPs(t.Context())
			require.NoError(t, err)
			require.Contains(t, []string{"queued", "already_running"}, res.Status)
			require.Positive(t, res.JobID)
			w.waitJob(res.JobID)
			require.Eventually(t, func() bool { return w.subscription(embedded, l.sub).Status == "canceled" }, 20*time.Second, 250*time.Millisecond,
				"the NMI-side delete is mirrored by the requested refresh")
			require.Zero(t, w.nmi.ScheduleDeletes(l.railSub), "a schedule NMI ended is never deleted again")
			require.Zero(t, len(w.nmi.Attempts()))
		})
	}
}
