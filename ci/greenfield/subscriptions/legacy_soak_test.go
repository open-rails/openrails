//go:build greenfield && integration

package subscriptions_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
)

// Soak: refunding an NMI-billed legacy payment with revoke_access while
// provider deletes are disarmed is refused before anything moves (NMI would
// keep charging a member without access). Armed, the refund ends the
// membership, deletes the schedule once, and no later converge, pull or
// stale NMI notice restores the access.
func TestLegacyNMIRefundRevokeEndsMembership(t *testing.T) {
	t.Parallel()
	for _, tp := range []topology{embedded, remote} {
		t.Run(string(tp), func(t *testing.T) {
			t.Parallel()
			w := newWorld(t)
			l := importLegacy(t, w, "nmi", tp)
			paid := completed(w.payments(tp, l.c.id))
			require.Len(t, paid, 1)
			legacy := paid[0]
			params := openrails.RefundPaymentParams{Full: true, Reason: "requested_by_customer", RevokeAccess: true, IdempotencyKey: "refund-" + legacy.ID.String()}

			_, err := w.client[tp].RefundPayment(t.Context(), legacy.ID, params)
			requireCode(t, err, http.StatusConflict, openrails.CodeProviderCancelHeld)
			w.settle()
			require.Empty(t, w.nmi.callsTo(http.MethodPost, "/payments/"+legacy.TransactionID+"/refund", nil), "nothing refunded")
			require.Equal(t, "active", w.subscription(tp, l.sub).Status)
			require.True(t, l.c.entitled(l.ent))
			require.Contains(t, w.openFindings(providerCancelHeld), l.sub.UUID().String())

			w.armDestructive()
			_, err = w.client[tp].RefundPayment(t.Context(), legacy.ID, params)
			require.NoError(t, err)
			w.settle()
			require.Len(t, w.nmi.callsTo(http.MethodPost, "/payments/"+legacy.TransactionID+"/refund", nil), 1)
			require.Equal(t, "cancelled", w.subscription(tp, l.sub).Status)
			w.until(func() bool { return w.nmi.deletesOf(l.railSub) > 0 }, "the NMI schedule delete")
			require.False(t, l.c.entitled(l.ent))

			require.Equal(t, http.StatusOK, w.deliver("nmi", l.staleNotice()))
			require.Equal(t, http.StatusOK, w.deliver("nmi", w.refundNotice("nmi")))
			w.converge()
			w.pull()
			w.advance(time.Hour)
			w.wake()
			w.converge()
			require.False(t, l.c.entitled(l.ent), "revoked access stays revoked")
			require.Equal(t, "cancelled", w.subscription(tp, l.sub).Status)
			require.Equal(t, 1, w.nmi.deletesOf(l.railSub))
			require.Zero(t, w.nmi.saleAttempts())
			require.Empty(t, w.nmi.unexpected())
		})
	}
}

// Soak: an imported book grants access by itself — active, mid-dunning in
// grace and cancelled-with-runway members are entitled right after
// ImportBilling returns, with no operator Converge.
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
				dunning: &openrails.DunningEvidence{Retries: 1, LastRetryAt: &last, ScheduleLive: true}})
			w.nmi.editSchedule(pastDue.schedule, func(s *nmiSchedule) { s.NextBilling = pastDue.paid.AddDate(0, 0, 30) })
			runway := b.add(&bookRow{source: "runway", tier: monthly, paid: now.Add(20 * day), declared: true,
				cancel: openrails.CancelEvidence{Kind: "user_cancelled", At: now.Add(-5 * day)}})
			w.nmi.providerCancel(runway.schedule)

			result, err := w.client[tp].ImportBilling(t.Context(), b.book)
			require.NoError(t, err)
			require.Len(t, result.Imported, 3, "%+v", result)
			for _, row := range []*bookRow{active, pastDue, runway} {
				require.True(t, row.c.entitled(monthly.ent), "%s is entitled right after import", row.source)
			}
			require.Equal(t, "past_due", pastDue.sub(w, tp).Status)
			require.Equal(t, "cancelled", runway.sub(w, tp).Status)

			// A replay changes nothing and stays entitled.
			replay, err := w.client[tp].ImportBilling(t.Context(), b.book)
			require.NoError(t, err)
			require.Empty(t, replay.Imported)
			require.True(t, active.c.entitled(monthly.ent))
			require.Zero(t, w.nmi.saleAttempts())
		})
	}
}

// Soak: a schedule NMI deleted without a notification is mirrored as soon as
// the host asks for a provider refresh through its Client, embedded or remote.
func TestLegacyNMIRefreshProviders(t *testing.T) {
	t.Parallel()
	for _, tp := range []topology{embedded, remote} {
		t.Run(string(tp), func(t *testing.T) {
			t.Parallel()
			w := newWorld(t)
			w.armDestructive()
			l := w.mirrorBook(tp, w.bookTier("monthly", 999, 30), 1)[0]
			w.nmi.providerCancel(l.railSub)
			w.advance(time.Hour)

			res, err := w.client[tp].RefreshProviders(t.Context())
			require.NoError(t, err)
			require.Contains(t, []string{"queued", "already_running"}, res.Status)
			require.Positive(t, res.JobID)
			w.waitJob(res.JobID)
			require.Eventually(t, func() bool { return w.subscription(tp, l.sub).Status == "cancelled" }, 20*time.Second, 50*time.Millisecond,
				"the NMI-side delete is mirrored by the requested refresh")
			require.Zero(t, w.nmi.deletesOf(l.railSub), "a schedule NMI ended is never deleted again")
			require.Zero(t, w.nmi.saleAttempts())
		})
	}
}
