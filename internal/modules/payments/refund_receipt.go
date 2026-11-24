package payments

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
)

// RefundReceiptWindow is how recent a refund the provider reported must be,
// by its time, for its receipt: one found later (a backfill, a late refresh)
// is old news to the customer.
const RefundReceiptWindow = 7 * 24 * time.Hour

// recent reports whether a refund made at refundedAt, by the provider's
// clock, is news at now: within RefundReceiptWindow.
func recent(refundedAt, now time.Time) bool { return now.Sub(refundedAt) <= RefundReceiptWindow }

// voidPrefix marks the reversal a void records: a void is a refund of the
// whole charge before it settled.
const voidPrefix = "void:"

// VoidTransactionID is the reversal's transaction id for the void of the
// charge transactionID.
func VoidTransactionID(transactionID string) string { return voidPrefix + transactionID }

// queueRefundReceipt records refund's refund_completed fact in the
// transaction that settles it. Its id is the refund's, so it is written once
// however often the settlement is replayed. A chargeback or dispute reversal
// has none, nor a refund the provider reported (reported) older than
// RefundReceiptWindow. A refund OpenRails completes always has one.
func (s *PaymentService) queueRefundReceipt(ctx context.Context, refund *models.Payment, reported bool) error {
	if refund.ReversalKind == nil || *refund.ReversalKind != ReversalRefund || refund.RefundedPaymentID == nil ||
		refund.Amount >= 0 || !PaymentStatusSucceeded(refund.Status) || reported && !recent(refund.PurchasedAt, s.now()) {
		return nil
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	q := s.db.Gen(ctx)
	amount := -refund.Amount
	data := billing.NotificationData{
		Amount: &amount, Currency: catalog.Currency(refund.Currency), Rail: billing.Rail(refund.Rail),
		PaymentID: billing.PaymentID(refund.ID), RefundedPaymentID: billing.PaymentID(*refund.RefundedPaymentID),
		Voided: strings.HasPrefix(refund.TransactionID, voidPrefix),
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
		ID:         uuidutil.ChildV7(refund.ID, string(models.NotificationRefundCompleted)),
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
