//go:build greenfield && integration

package subscriptions_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/config"
)

// A dunning recovery of an NMI-scheduled subscription refused as a duplicate
// stays unknown: the matching charge on the card may be NMI's own schedule
// paying the period, so no new order is sent on later due passes.
func TestNMIRecoveryDuplicateRefusalIsNotResent(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	l := importLegacy(t, w, "nmi", embedded, func(book *openrails.DeclaredBilling) {
		book.Subscriptions[0].CollectionPolicy = "provider_dunning"
	})
	w.converge()
	w.cfg = func(c *config.Config) { c.ProviderWriteMode = config.ProviderWriteModeReadOnly }
	w.restart()
	end := l.periodEnd()
	w.advance(end.Sub(w.clock.Now()) + time.Hour)
	require.Equal(t, http.StatusOK, w.deliver("nmi", l.providerRenewal(false)))
	sub := w.subscription(embedded, l.sub)
	require.Equal(t, "past_due", sub.Status)
	require.NotNil(t, sub.NextRetryAt)
	w.cfg = nil
	w.restart()

	w.nmi.RefuseDuplicates(1)
	w.advance(sub.NextRetryAt.Sub(w.clock.Now()) + time.Second)
	w.runRenewals()
	require.Equal(t, 1, len(w.nmi.Attempts()), "the recovery reached NMI once and was refused")
	for range 6 {
		w.advance(time.Hour)
		w.wake()
		w.runRenewals()
	}
	require.Equal(t, 1, len(w.nmi.Attempts()), "no new order while the duplicate is unresolved")
	require.Len(t, w.nmi.ledger(""), 1, "only the imported period was ever charged")
	require.True(t, w.subscription(embedded, l.sub).CurrentPeriodEndsAt.Equal(end))
	require.True(t, l.c.entitled(l.ent), "access holds while the charge is verified")
}

// saleOrders is the order id of every sale request, in arrival order.
func (f *nmiFake) saleOrders() []string {
	attempts := f.Attempts()
	out := make([]string, 0, len(attempts))
	for _, form := range attempts {
		out = append(out, form.Get("orderid"))
	}
	return out
}
