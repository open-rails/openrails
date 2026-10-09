//go:build e2e && integration

package subscriptions_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
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
		_, err := e.c.enroll(e.rail, e.price, e.ent, e.method)
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
		_, err = l.reenroll(method)
		requireCode(t, err, http.StatusConflict, billing.CodeSubscriptionPaidThrough)
		require.Len(t, w.nmi.Ledger(""), before, "nothing is charged for the paid runway")
		w.advanceHealthyTo(end.Add(time.Minute))
		attempt, err := l.reenroll(method)
		require.NoError(t, err)
		require.Equal(t, billing.CheckoutAttemptSucceeded, attempt.Status)
		require.Len(t, w.nmi.Ledger(""), before+1)
	})
}

// enroll buys a membership through checkout and answers its refusal.
func (c *customer) enroll(rail, priceID, entitlement, method string) (*billing.CheckoutAttempt, error) {
	return createCheckoutAttempt(c.w.t.Context(), c.w.client[embedded], billing.CreateCheckoutAttemptParams{
		OfferKind: billing.OfferRecurring, Customer: c.identity(), Entitlement: entitlement, PriceID: pid(priceID),
		IdempotencyKey: "enroll-" + uuid.NewString(), PaymentOptions: billing.CheckoutPaymentOptions{PSP: rail, PaymentMethodID: pmid(method)},
		SuccessURL: "https://e2e.test/return", CancelURL: "https://e2e.test/return?canceled=1",
	})
}
