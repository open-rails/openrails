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
	"github.com/open-rails/openrails/internal/billingfact"
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
	if refund.InvoiceID != nil {
		invoice, err := q.GetInvoice(ctx, gen.GetInvoiceParams{MerchantID: mid.UUID(), ID: *refund.InvoiceID})
		if err != nil {
			return fmt.Errorf("refund receipt: invoice %s: %w", *refund.InvoiceID, err)
		}
		lines, err := q.ListInvoiceLines(ctx, gen.ListInvoiceLinesParams{MerchantID: mid.UUID(), InvoiceIds: []uuid.UUID{invoice.ID}})
		if err != nil {
			return fmt.Errorf("refund receipt: invoice %s lines: %w", invoice.ID, err)
		}
		data.InvoiceID, data.ProductName = billing.InvoiceID(invoice.ID), InvoiceItems(lines)
		if invoice.Number != nil {
			data.InvoiceNumber = *invoice.Number
		}
	}
	raw, err := json.Marshal(data)
	if err != nil {
		return err
	}
	_, err = billingfact.Record(ctx, q, billingfact.Fact{
		ID:         uuidutil.ChildV7(refund.ID, string(models.NotificationRefundCompleted)),
		MerchantID: mid.UUID(), CustomerID: refund.CustomerID, EventType: string(models.NotificationRefundCompleted),
		Data: raw, CreatedAt: refund.PurchasedAt.UTC(),
	})
	return err
}

// InvoiceItems names what an invoice's lines bought, as its receipts do.
func InvoiceItems(lines []gen.BillingInvoiceLine) string {
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
