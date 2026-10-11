// Package orderevent records an order's transitions for the host, in the
// transaction that makes them, and formats its document numbers.
package orderevent

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/open-rails/openrails/internal/db/gen"
)

// Enqueue records one order transition as a host event; order is the row as
// the transition leaves it.
func Enqueue(ctx context.Context, q *gen.Queries, order *gen.BillingOrder, kind string, at time.Time, extra map[string]any) error {
	data := map[string]any{"customer_id": order.CustomerID.String(), "status": order.Status, "payment_status": order.PaymentStatus}
	for k, v := range extra {
		data[k] = v
	}
	raw, err := json.Marshal(data)
	if err != nil {
		return err
	}
	dedupe := kind + ":" + order.ID.String()
	if order.AttemptID != nil && (kind == "order.requires_action" || kind == "order.payment_failed") {
		dedupe += ":" + order.AttemptID.String()
	}
	_, err = q.EnqueueOrderHostEvent(ctx, gen.EnqueueOrderHostEventParams{MerchantID: order.MerchantID, EventType: kind, OrderID: order.ID,
		Amount: order.Total, Currency: order.Currency, OccurredAt: at, Data: raw, DedupeKey: dedupe})
	return err
}

// Number formats a document number from the merchant's sequence.
func Number(n int64) string { return fmt.Sprintf("%06d", n) }
