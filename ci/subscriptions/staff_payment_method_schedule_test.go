//go:build e2e && integration

package subscriptions_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
)

// Selecting a card as staff must not serve as an immediate dunning command,
// even when the caller submits the card already selected on the membership.
func TestStaffCardSelectionPreservesEngineRetrySchedule(t *testing.T) {
	forEach(t, func(t *testing.T, rail string, tp topology) {
		w := newWorld(t)
		e := enroll(t, w, rail, tp)
		e.setDecline(visa.Last4, "insufficient_funds", "202")
		e.toFreshPeriodEnd()
		w.runRenewals()
		before := w.subscription(tp, e.sub)
		require.Equal(t, billing.SubscriptionPastDue, before.Status)
		require.NotNil(t, nextRetry(before))
		retryAt := *nextRetry(before)
		require.True(t, retryAt.After(w.clock.Now()))
		other := e.c.saveCard(rail, mastercard)
		attempts, charges := e.providerAttempts(), len(e.providerLedger())
		for _, method := range []string{e.method, other} {
			id := pmid(method)
			updated, err := w.client[tp].SetSubscriptionPaymentMethod(t.Context(), e.sub, billing.SetSubscriptionPaymentMethodParams{PaymentMethodID: &id})
			require.NoError(t, err)
			require.Equal(t, billing.SubscriptionPastDue, updated.Status)
			require.Equal(t, before.Dunning, updated.Dunning, "staff changes only the instrument, retaining retry evidence and deadline")
			w.runRenewals()
			require.Equal(t, attempts, e.providerAttempts(), "no collection before the original retry deadline")
			require.Len(t, e.providerLedger(), charges)
		}
		w.advanceHealthyTo(retryAt.Add(time.Second))
		w.runRenewals()
		require.Equal(t, billing.SubscriptionActive, w.subscription(tp, e.sub).Status)
		require.Equal(t, attempts+1, e.providerAttempts(), "the existing policy still collects once due")
		require.Len(t, e.providerLedger(), charges+1)
		require.Equal(t, mastercard.Last4, e.lastChargedCard())
	})
}

// A staff assignment also cannot turn a stopped awaiting-method case into an
// immediate retry. The customer's own selection keeps its explicit recovery.
func TestStaffCardSelectionDoesNotResumeAwaitingMethod(t *testing.T) {
	w := newWorld(t)
	e := enroll(t, w, "nmi", remote)
	e.setDecline(visa.Last4, "expired_card", "223")
	e.toFreshPeriodEnd()
	w.runRenewals()
	before := w.subscription(remote, e.sub)
	require.Equal(t, billing.SubscriptionAwaitingMethod, before.Status)
	require.Nil(t, nextRetry(before))
	other := e.c.saveCard("nmi", mastercard)
	id := pmid(other)
	attempts := e.providerAttempts()
	updated, err := w.client[remote].SetSubscriptionPaymentMethod(t.Context(), e.sub, billing.SetSubscriptionPaymentMethodParams{PaymentMethodID: &id})
	require.NoError(t, err)
	require.Equal(t, before.Status, updated.Status)
	require.Equal(t, before.Dunning, updated.Dunning)
	w.runRenewals()
	require.Equal(t, attempts, e.providerAttempts())
	// It is a customer decision, even though staff already selected the card.
	e.c.must(http.MethodPut, "/subscriptions/"+e.sub.String()+"/payment-method", "", map[string]any{"payment_method_id": other})
	w.runRenewals()
	require.Equal(t, attempts+1, e.providerAttempts())
	require.Equal(t, billing.SubscriptionActive, w.subscription(remote, e.sub).Status)
}

// Gateway-owned NMI swaps finalize through a durable intent; its stored
// staff origin must preserve the same retry boundary as engine selection.
func TestStaffNMIScheduleCardSwapPreservesRetryDeadline(t *testing.T) {
	w := newWorld(t)
	legacy := importLegacy(t, w, "nmi", remote, declareRecurringAnchor)
	w.converge()
	w.nmi.SetDecline(visa.Last4, "202")
	due := legacy.periodEnd()
	w.nmi.RenewSchedule(legacy.railSub, false)
	w.advance(due.Sub(w.clock.Now()) + 30*time.Hour)
	w.watchRebills()
	w.refreshProviders()
	before := w.subscription(remote, legacy.sub)
	require.Equal(t, billing.SubscriptionPastDue, before.Status)
	require.NotNil(t, nextRetry(before))
	retryAt := *nextRetry(before)
	require.True(t, retryAt.After(w.clock.Now()))
	other := legacy.c.saveCard("nmi", mastercard)
	id := pmid(other)
	attempts := len(w.nmi.Attempts())
	updated, err := w.client[remote].SetSubscriptionPaymentMethod(t.Context(), legacy.sub, billing.SetSubscriptionPaymentMethodParams{PaymentMethodID: &id})
	require.NoError(t, err)
	require.Equal(t, other, updated.PaymentMethodID.String())
	require.Equal(t, before.Dunning, updated.Dunning)
	w.runRenewals()
	require.Len(t, w.nmi.Attempts(), attempts, "a staff schedule swap does not request an early rebill")
	w.advanceHealthyTo(retryAt.Add(time.Second))
	w.runRenewals()
	require.Len(t, w.nmi.Attempts(), attempts+1, "the normal scheduled retry still runs")
	require.Equal(t, mastercard.Last4, w.nmi.LastSale().Card.Last4)
}
