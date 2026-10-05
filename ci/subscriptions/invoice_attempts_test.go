//go:build e2e && integration

package subscriptions_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/riverqueue/river"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
)

// invoicePass is the hourly invoice pass: arrears past their threshold are
// invoiced.
type invoicePass struct{}

func (invoicePass) Kind() string { return "openrails.invoice" }

// A payer in arrears pays the invoice with a card the issuer declines, then
// with one it approves: each answer is an invoice attempt, and the decline
// never becomes a payment (#1111).
func TestInvoiceCollectionAttempts(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	ctx, client, c := t.Context(), w.client[embedded], w.newCustomer()
	const owed = 50_000_000 // the default invoice threshold
	customer := billing.CustomerID(uuid.MustParse(c.id))
	_, err := client.SetCreditLimit(ctx, customer, billing.SetCreditLimitParams{Currency: "USD", Amount: owed})
	require.NoError(t, err)
	request, expires := uuid.NewString(), w.clock.Now().Add(time.Hour)
	admitted, err := client.Admit(ctx, []billing.AdmitParams{{CustomerID: customer, Invoker: c.id, InvokerType: billing.InvokerTypePayer, Currency: "USD", EstimatedAmount: owed, RequestID: request, ExpiresAt: &expires}})
	require.NoError(t, err)
	require.True(t, admitted[0].Allowed(), "%+v", admitted)
	_, err = client.CaptureAdmission(ctx, request, billing.CaptureAdmissionParams{Amount: owed})
	require.NoError(t, err)

	w.advance(time.Minute)
	res, err := w.jobs.Insert(ctx, invoicePass{}, &river.InsertOpts{Queue: openrails.QueueBilling})
	require.NoError(t, err)
	w.waitJob(res.Job.ID)
	invoices, err := client.ListInvoices(ctx, billing.InvoiceListParams{CustomerID: c.cid()})
	require.NoError(t, err)
	require.Len(t, invoices.Items, 1)
	invoice := invoices.Items[0].ID.String()

	declining := c.saveCard("nmi", card{Brand: "visa", Last4: "0002", Decline: "202"})
	status, out := c.call(http.MethodPost, "/invoices/"+invoice+"/pay-now", "pay-"+uuid.NewString(), map[string]any{"payment_method_id": declining})
	require.Equal(t, http.StatusPaymentRequired, status, "%v", out)
	approving := c.saveCard("nmi", visa)
	c.must(http.MethodPost, "/invoices/"+invoice+"/pay-now", "pay-"+uuid.NewString(), map[string]any{"payment_method_id": approving})

	var charges []attempt
	for _, a := range w.attempts(c.id) {
		if a.Kind == "invoice" {
			charges = append(charges, a)
		}
	}
	require.Len(t, charges, 2)
	require.Equal(t, []string{"issuer_soft", "insufficient_funds", "saved"}, []string{charges[0].Category, str(charges[0].Reason), charges[0].CardEntry})
	require.Equal(t, "approved", charges[1].Category)
}
