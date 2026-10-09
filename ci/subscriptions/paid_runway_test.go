//go:build e2e && integration

package subscriptions_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
)

// Buying a product again while a canceled subscription to it is still paid
// charges nothing (tracker 1137): a resumable one is resumed instead, any
// other is bought again once its paid period ends.
func TestBuyingAgainDuringPaidRunway(t *testing.T) {
	t.Parallel()
	t.Run("engine_resumes", func(t *testing.T) {
		t.Parallel()
		w := newWorld(t)
		e := enroll(t, w, "nmi", embedded)
		status, body := e.c.call(http.MethodPost, "/subscriptions/"+e.sub.String()+"/cancel", "", map[string]any{"reason": "too expensive"})
		require.Equal(t, http.StatusOK, status, "%v", body)
		before := len(e.providerLedger())
		_, err := e.c.checkout(embedded, order{price: pid(e.price), rail: e.rail, method: e.method, successURL: "https://e2e.test/return"})
		requireCode(t, err, http.StatusConflict, billing.CodeSubscriptionResumable)
		require.Len(t, e.providerLedger(), before, "nothing is charged for the paid runway")
		status, body = e.c.call(http.MethodPost, "/subscriptions/"+e.sub.String()+"/resume", "", nil)
		require.Equal(t, http.StatusOK, status, "%v", body)
		require.Equal(t, billing.SubscriptionActive, w.subscription(embedded, e.sub).Status)
		require.Len(t, e.providerLedger(), before)
	})
	t.Run("legacy_waits_for_period_end", func(t *testing.T) {
		t.Parallel()
		w := newWorld(t)
		w.armDestructive()
		l := importLegacy(t, w, "nmi", remote)
		w.converge()
		w.refreshProviders()
		end := l.periodEnd()
		_, err := w.client[remote].CancelSubscription(t.Context(), l.sub, billing.CancelSubscriptionParams{Reason: "member left"})
		require.NoError(t, err)
		w.until(func() bool { return w.subscription(remote, l.sub).DeletionScheduledAt == nil }, "the NMI schedule is deleted")
		method := l.c.saveCard("nmi", visa)
		before := len(w.nmi.Ledger(""))
		_, err = l.checkoutAgain(method)
		requireCode(t, err, http.StatusConflict, billing.CodeSubscriptionPaidThrough)
		require.Len(t, w.nmi.Ledger(""), before, "nothing is charged for the paid runway")
		w.advanceHealthyTo(end.Add(time.Minute))
		require.Equal(t, "succeeded", l.reenroll(method).Status)
		require.Len(t, w.nmi.Ledger(""), before+1)
	})
}
