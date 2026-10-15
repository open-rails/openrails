//go:build e2e && integration

package subscriptions_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/open-rails/openrails/billing"
	"github.com/stretchr/testify/require"
)

func TestNMICardRemovalIsInlineWithUnresolvedFindings(t *testing.T) {
	w := newWorld(t)
	c := w.newCustomer()
	id := c.saveCard("nmi", visa)
	method, err := billing.ParsePaymentMethodID(id)
	require.NoError(t, err)
	purchase := order{price: w.permanent("removal:manual").ID, rail: "nmi", method: id}
	sale, err := c.sell(remote, purchase)
	require.NoError(t, err)
	w.holdRecovery("nmi") // The unrelated finding remains a report, not a gate.
	status, out := c.call(http.MethodDelete, "/payment-methods/"+id, "", nil)
	require.Equal(t, http.StatusNoContent, status, "%v", out)
	require.Empty(t, c.must(http.MethodGet, "/payment-methods", "", nil)["data"])
	var state string
	var attempts int
	require.NoError(t, w.pool.QueryRow(t.Context(), w.q(`SELECT status,attempts FROM billing.provider_intents WHERE intent_type='nmi_vault_delete' AND payload->>'payment_method_id'=$1`), method.UUID().String()).Scan(&state, &attempts))
	require.Equal(t, "succeeded", state)
	require.Equal(t, 1, attempts)
	assertNoCardDeleteJobs(t, w, id)
	_, err = sale.buy(c, purchase)
	require.Equal(t, "payment_method_stale", codeOf(err))
	require.Empty(t, completed(w.payments(remote, c.id)))
	w.advance(5 * time.Minute)
	w.wake()
	require.Len(t, w.nmi.CallsTo(http.MethodDelete, "/customers/", nil), 1)
}
