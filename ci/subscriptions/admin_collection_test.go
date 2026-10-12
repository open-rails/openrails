//go:build e2e && integration

package subscriptions_test

import (
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/billing"
	"github.com/stretchr/testify/require"
)

func TestAdminCannotTriggerOrderCollection(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	customer := w.newCustomer()
	customer.saveCard("nmi", visa)
	order := observedInvoice(t, w, customer, 50_000_000)
	before := len(w.nmi.Attempts())
	for _, token := range []string{w.auth.token(t, "staff"), w.auth.hostToken(t)} {
		status, body := w.merchantCall(token, http.MethodPost, "/v1/admin/orders/"+order.String()+"/retry-collection")
		require.Equal(t, http.StatusNotFound, status, body)
	}
	require.Len(t, w.nmi.Attempts(), before, "staff and application credentials cannot start a card charge")
	require.Equal(t, billing.OrderOpen, w.invoice(order).Status)

	// Recording a remittance and forgiving its unpaid remainder neither charge
	// a card nor erase the payment already received.
	client := w.client[remote]
	payment, err := client.RecordExternalPayment(t.Context(), billing.RecordExternalPaymentParams{OrderID: order, Amount: 10_000_000, TransactionID: uuid.NewString()})
	require.NoError(t, err)
	waived, err := client.WaiveOrderBalance(t.Context(), order)
	require.NoError(t, err)
	require.Equal(t, billing.OrderVoid, waived.Status)
	retained, err := client.GetPayment(t.Context(), payment.ID)
	require.NoError(t, err)
	require.Equal(t, payment.ID, retained.ID)
	require.EqualValues(t, 10_000_000, retained.Amount)
	balance, err := client.GetBalance(t.Context(), customer.cid(), "USD")
	require.NoError(t, err)
	require.Zero(t, balance.OwedAmount)
	require.Len(t, w.nmi.Attempts(), before, "remittance and waiver perform no provider charge")
}
