package payments

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/merchant"
)

// queueRefundReceipt records refund's refund_completed fact in the
// transaction that settles it. Its id is the refund's, so it is written once
// however often the settlement is replayed. A chargeback or dispute reversal
// has none.
func (s *PaymentService) queueRefundReceipt(ctx context.Context, refund *models.Payment) error {
	if refund.ReversalKind == nil || *refund.ReversalKind != ReversalRefund || refund.RefundedPaymentID == nil ||
		refund.Amount >= 0 || !PaymentStatusSucceeded(refund.Status) {
		return nil
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	q := s.db.Gen(ctx)
	amount := -refund.Amount
	data := billing.NotificationData{
		Amount: &amount, Currency: refund.Currency, Rail: string(refund.Rail),
		PaymentID: billing.PaymentID(refund.ID), RefundedPaymentID: billing.PaymentID(*refund.RefundedPaymentID),
	}
	if refund.SubscriptionID != nil {
		data.SubscriptionID = billing.SubscriptionID(*refund.SubscriptionID)
	}
	if refund.OrderID != nil {
		order, err := q.GetOrder(ctx, gen.GetOrderParams{MerchantID: mid.UUID(), ID: *refund.OrderID})
		if err != nil {
			return fmt.Errorf("refund receipt: order %s: %w", *refund.OrderID, err)
		}
		lines, err := q.ListOrderLines(ctx, gen.ListOrderLinesParams{MerchantID: mid.UUID(), OrderIds: []uuid.UUID{order.ID}})
		if err != nil {
			return fmt.Errorf("refund receipt: order %s lines: %w", order.ID, err)
		}
		data.OrderID, data.ProductName = billing.OrderID(order.ID), OrderItems(lines)
		if order.Number != nil {
			data.OrderNumber = *order.Number
		}
	}
	raw, err := json.Marshal(data)
	if err != nil {
		return err
	}
	return q.CreateNotificationIfAbsent(ctx, gen.CreateNotificationIfAbsentParams{
		ID:         uuid.NewSHA1(refund.ID, []byte(models.NotificationRefundCompleted)),
		MerchantID: mid.UUID(), CustomerID: refund.CustomerID, EventType: string(models.NotificationRefundCompleted),
		Data: raw, CreatedAt: refund.PurchasedAt.UTC(),
	})
}

// OrderItems names what an order's lines bought, as its receipts do.
func OrderItems(lines []gen.BillingOrderLine) string {
	names := make([]string, 0, len(lines))
	for _, l := range lines {
		if l.Quantity != nil && *l.Quantity > 1 {
			names = append(names, fmt.Sprintf("%s × %d", l.Description, *l.Quantity))
			continue
		}
		names = append(names, l.Description)
	}
	return strings.Join(names, ", ")
}
