package webhooks

import (
	"context"
	"fmt"
	"slices"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/merchant"
)

// paymentSubscriptionIDs includes both legacy direct references and subscriptions
// produced by order lines. A single subscription order has both references.
func paymentSubscriptionIDs(ctx context.Context, d *db.DB, paymentID uuid.UUID) ([]uuid.UUID, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	q := d.Gen(ctx)
	payment, err := q.GetPaymentByID(ctx, gen.GetPaymentByIDParams{MerchantID: mid.UUID(), ID: paymentID})
	if err != nil {
		return nil, fmt.Errorf("load disputed payment: %w", err)
	}
	var ids []uuid.UUID
	if payment.SubscriptionID != nil {
		ids = append(ids, *payment.SubscriptionID)
	}
	if payment.OrderID == nil {
		return ids, nil
	}
	lines, err := q.ListOrderLines(ctx, gen.ListOrderLinesParams{MerchantID: mid.UUID(), OrderIds: []uuid.UUID{*payment.OrderID}})
	if err != nil {
		return nil, fmt.Errorf("load disputed order lines: %w", err)
	}
	for _, line := range lines {
		if line.SubscriptionID != nil && !slices.Contains(ids, *line.SubscriptionID) {
			ids = append(ids, *line.SubscriptionID)
		}
	}
	return ids, nil
}
