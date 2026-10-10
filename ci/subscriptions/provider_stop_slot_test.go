//go:build e2e && integration

package subscriptions_test

import (
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
)

const chargeAfterCancel = "life.charge_after_cancel"

// nmiDeleteFails answers NMI schedule deletes with 503 while failing is set.
func nmiDeleteFails(w *world, failing *atomic.Bool) {
	w.nmi.FailRequests(func(r *http.Request) bool {
		if !failing.Load() {
			return false
		}
		if r.Method == http.MethodDelete && strings.Contains(r.URL.Path, "subscriptions") {
			return true
		}
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "transact.php") {
			return false
		}
		_ = r.ParseForm()
		return r.Form.Get("recurring") == "delete_subscription"
	}, http.StatusServiceUnavailable, 1000)
}

// checkoutAgain buys the legacy membership's own price again through checkout.
func (l *legacy) checkoutAgain(method string) (*sessionPaid, error) {
	l.w.t.Helper()
	return l.c.checkout(embedded, order{price: l.price.ID, rail: "nmi", method: method, successURL: "https://e2e.test/return"})
}

// reenroll is a checkoutAgain the page answers.
func (l *legacy) reenroll(method string) *sessionPaid {
	l.w.t.Helper()
	paid, err := l.checkoutAgain(method)
	require.NoError(l.w.t, err)
	return paid
}

// requireSlotHeld is a purchase refused because the customer's slot is held:
// nothing charged, and the page says why.
func requireSlotHeld(t *testing.T, paid *sessionPaid) {
	t.Helper()
	require.Equal(t, "blocked", paid.Status, "%+v", paid.CheckoutSessionPayResult)
	require.NotNil(t, paid.FailureMessage)
}

// A canceled membership whose NMI schedule may still bill holds the product
// slot until the delete is verified: rebuying is refused, a charge after the
// cancel opens a finding and is refunded in full, and a verified delete frees
// the slot.
func TestPendingProviderStopHoldsSlot(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.armDestructive()
	l := importLegacy(t, w, "nmi", remote)
	w.converge()
	end := l.periodEnd()
	w.advanceHealthyTo(end.Add(-time.Hour))
	method := l.c.saveCard("nmi", visa)
	before := len(w.nmi.Ledger(""))
	var failing atomic.Bool
	failing.Store(true)
	nmiDeleteFails(w, &failing)

	status, body := l.meCancel(l.sub)
	require.Equal(t, http.StatusOK, status, "%v", body)
	w.settle()
	require.Equal(t, billing.SubscriptionCanceled, w.subscription(remote, l.sub).Status)
	require.True(t, w.nmi.ScheduleLive(l.railSub), "the provider stop has not succeeded")

	refused := l.reenroll(method)
	requireSlotHeld(t, refused)
	require.Contains(t, *refused.FailureMessage, "resume")
	require.Len(t, w.nmi.Ledger(""), before, "the refused enrollment charges nothing")

	w.advance(end.Add(time.Second).Sub(w.clock.Now()))
	require.Equal(t, http.StatusOK, w.deliver("nmi", l.providerRenewal(true)))
	w.settle()
	w.converge()
	ledger := w.nmi.Ledger("")
	require.Len(t, ledger, before+1, "only the still-live schedule collected")
	late := ledger[len(ledger)-1]
	w.until(func() bool {
		for _, e := range w.nmi.Ledger("") {
			if e.TransactionID == late.TransactionID {
				return e.RefundedCents == e.Cents
			}
		}
		return false
	}, "the charge after cancel is refunded in full")
	require.Len(t, w.openFindings(chargeAfterCancel), 1, "a standing finding names the charge")

	failing.Store(false)
	w.until(func() bool { return !w.nmi.ScheduleLive(l.railSub) }, "the delete succeeds once NMI answers")
	w.until(func() bool { return w.subscription(remote, l.sub).DeletionScheduledAt == nil }, "the verified stop releases the slot")
	require.Equal(t, "succeeded", l.reenroll(method).Status)
	require.Len(t, w.nmi.Ledger(""), before+2, "one charge for the new membership")
}

// The undo window keeps the NMI schedule for a resume: buying the same
// product meanwhile is refused and the customer resumes instead.
func TestCanceledInUndoWindowResumesInsteadOfBuyingAgain(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.armDestructive()
	l := importLegacy(t, w, "nmi", remote)
	w.converge()
	method := l.c.saveCard("nmi", visa)
	before := len(w.nmi.Ledger(""))
	status, body := l.meCancel(l.sub)
	require.Equal(t, http.StatusOK, status, "%v", body)
	w.settle()
	require.NotNil(t, w.subscription(remote, l.sub).DeletionScheduledAt)

	requireSlotHeld(t, l.reenroll(method))
	require.Len(t, w.nmi.Ledger(""), before)

	status, body = l.c.call(http.MethodPost, "/subscriptions/"+l.sub.String()+"/resume", "", nil)
	require.Equal(t, http.StatusOK, status, "%v", body)
	sub := w.subscription(remote, l.sub)
	require.Equal(t, billing.SubscriptionActive, sub.Status)
	require.Nil(t, sub.DeletionScheduledAt, "a resumed membership holds no pending stop")
	require.True(t, w.nmi.ScheduleLive(l.railSub))
	require.Len(t, w.nmi.Ledger(""), before)
}

// The kill switch parks the delete of an account deletion's cancel: checkout
// stays refused for that product until the operator arms the merchant and the
// delete runs, and then until the paid period ends.
func TestDisarmedProviderStopHoldsSlot(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	l := importLegacy(t, w, "nmi", remote)
	w.converge()
	w.refreshProviders()
	method := l.c.saveCard("nmi", visa)
	before := len(w.nmi.Ledger(""))
	_, err := w.client[remote].CancelSubscription(t.Context(), l.sub, billing.CancelSubscriptionParams{Reason: "account deleted", AccountDeletion: true})
	require.NoError(t, err)
	w.settle()
	require.Equal(t, billing.SubscriptionCanceled, w.subscription(remote, l.sub).Status)

	requireSlotHeld(t, l.reenroll(method))
	require.Len(t, w.nmi.Ledger(""), before)

	w.armDestructive()
	w.until(func() bool { return !w.nmi.ScheduleLive(l.railSub) }, "the held delete runs once armed")
	w.until(func() bool { return w.subscription(remote, l.sub).DeletionScheduledAt == nil }, "the verified stop releases the slot")
	_, err = l.checkoutAgain(method)
	requireCode(t, err, http.StatusConflict, billing.CodeSubscriptionPaidThrough)
	w.advanceHealthyTo(l.periodEnd().Add(time.Minute))
	require.Equal(t, "succeeded", l.reenroll(method).Status)
	require.Len(t, w.nmi.Ledger(""), before+1, "one charge, after the paid period")
}

// A customer's cancel never races an accepted engine renewal whose outcome is
// unknown: it is refused while the payment is unresolved and accepted once
// the renewal settles.
func TestEngineCancelWaitsForUnresolvedRenewal(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	e := enroll(t, w, "stripe", embedded)
	end := e.periodEnd()
	e.refreshBeforePeriodEnd()
	w.loseSubmissions("stripe", 1)
	w.readUnavailable("stripe", true)
	e.toPeriodEnd()
	w.runRenewals()
	w.until(func() bool { return len(w.openFindings("life.submission.unresolved")) == 1 }, "the renewal outcome is unknown")

	status, body := e.c.call(http.MethodPost, "/subscriptions/"+e.sub.String()+"/cancel", "", map[string]any{"reason": "too expensive"})
	require.Equal(t, http.StatusConflict, status, "%v", body)
	require.Equal(t, "payment_in_progress", errorCode(body))
	require.Equal(t, billing.SubscriptionActive, w.subscription(embedded, e.sub).Status)

	w.readUnavailable("stripe", false)
	w.refreshProviders()
	w.until(func() bool { return w.subscription(embedded, e.sub).CurrentPeriodEndsAt.After(end) }, "the renewal resolves")
	status, body = e.c.call(http.MethodPost, "/subscriptions/"+e.sub.String()+"/cancel", "", map[string]any{"reason": "too expensive"})
	require.Equal(t, http.StatusOK, status, "%v", body)
	require.Equal(t, billing.SubscriptionCanceled, w.subscription(embedded, e.sub).Status)
	require.Len(t, e.providerLedger(), 2, "one renewal charge, kept for the paid period")
	require.Empty(t, w.openFindings(chargeAfterCancel))
}
