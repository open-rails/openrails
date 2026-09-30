//go:build greenfield && integration

package subscriptions_test

import (
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
)

// #1109: a card whose security code does not match is refused at
// verification, and the buyer is told which field to fix.
func TestHostedNewCardSecurityCodeMismatch(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	h := hostedPay{w: w, c: w.newCustomer(), tp: remote, price: w.membership("content:members", 9_990_000).ID}

	_, err := h.pay("pay-cvc", openrails.CheckoutPaymentOptions{PaymentToken: w.nmi.Tokenize(card{Brand: "visa", Last4: "0005", Decline: "200", CVV: "N"})})
	require.ErrorIs(t, err, openrails.ErrPaymentRefused)
	var status *openrails.StatusError
	require.True(t, errors.As(err, &status))
	require.Equal(t, openrails.CodeCardDeclined, status.Code)
	failure, ok := openrails.PaymentFailureFrom(err)
	require.True(t, ok)
	require.Equal(t, "incorrect_cvc", failure.Reason)
	require.Equal(t, "cvc", failure.Field)
	require.Empty(t, h.subscriptions())
	require.Empty(t, h.methods())
}

// #1109: a decline of NMI's own scheduled charge follows the same table as
// OpenRails' rebills. Do-not-honor is retried on the dunning schedule, an
// expired card waits for a new one, a stolen card ends the schedule.
func TestNMIScheduleDeclinePolicy(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, code, status string
		retry              bool
	}{
		{"do_not_honor", "201", "past_due", true},
		{"expired_card", "223", "awaiting_method", false},
		{"stolen_card", "252", "cancelled", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w := newWorld(t)
			w.armDestructive()
			l := importLegacy(t, w, "nmi", embedded, declareRecurringAnchor)
			w.converge()
			w.nmi.SetDecline(visa.Last4, tc.code)
			w.advance(l.periodEnd().Sub(w.clock.Now()) + time.Hour)
			first := w.clock.Now()
			require.Equal(t, http.StatusOK, w.deliver("nmi", l.providerRenewal(false)))
			w.settle()
			sub := w.subscription(embedded, l.sub)
			require.Equal(t, tc.status, sub.Status)
			if !tc.retry {
				require.Nil(t, sub.NextRetryAt)
				w.advance(3 * day)
				w.runRenewals()
				require.Zero(t, len(w.nmi.Attempts()), "no OpenRails charge against this card")
				if tc.status == "cancelled" {
					w.wake() // past the system delete's 24h cooling-off
					require.False(t, w.nmi.ScheduleLive(l.railSub), "the NMI schedule is ended")
				}
				return
			}
			require.NotNil(t, sub.NextRetryAt)
			require.WithinDuration(t, first.Add(2*day), *sub.NextRetryAt, time.Minute)
			w.advance(sub.NextRetryAt.Sub(w.clock.Now()) + time.Second)
			w.runRenewals()
			require.Len(t, w.nmi.Attempts(), 1, "OpenRails retries the declined period")
			sub = w.subscription(embedded, l.sub)
			require.Equal(t, "past_due", sub.Status)
			require.NotNil(t, sub.NextRetryAt)
			require.WithinDuration(t, first.Add(5*day), *sub.NextRetryAt, time.Minute)
			require.True(t, w.nmi.ScheduleLive(l.railSub))
		})
	}
}
