// Package invoiceevent records an invoice's transitions for the host, in the
// transaction that makes them, and formats its document numbers.
package invoiceevent

import (
	"context"
	"encoding/json"
	"time"

	"github.com/open-rails/openrails/internal/db/gen"
)

// Enqueue records one invoice transition as a host event; invoice is the row as
// the transition leaves it.
func Enqueue(ctx context.Context, q *gen.Queries, invoice *gen.BillingInvoice, kind string, at time.Time, extra map[string]any) error {
	data := map[string]any{"customer_id": invoice.CustomerID.String(), "status": invoice.Status, "payment_status": invoice.PaymentStatus}
	for k, v := range extra {
		data[k] = v
	}
	raw, err := json.Marshal(data)
	if err != nil {
		return err
	}
	dedupe := kind + ":" + invoice.ID.String()
	if invoice.AttemptID != nil && (kind == "invoice.requires_action" || kind == "invoice.payment_failed") {
		dedupe += ":" + invoice.AttemptID.String()
	}
	_, err = q.EnqueueInvoiceHostEvent(ctx, gen.EnqueueInvoiceHostEventParams{MerchantID: invoice.MerchantID, EventType: kind, InvoiceID: invoice.ID,
		Amount: invoice.Total, Currency: invoice.Currency, OccurredAt: at, Data: raw, DedupeKey: dedupe})
	return err
}
