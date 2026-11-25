package intents

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"

	"github.com/open-rails/openrails/billing"

	"github.com/jackc/pgx/v5"
	"github.com/jonboulle/clockwork"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/shared/timeutil"
)

// InvoiceLatePaymentFindingType is raised when money moved on an invoice another
// payment completed, or that had closed and whose ownership was taken
// meanwhile: the payment is refunded.
const InvoiceLatePaymentFindingType = "life.invoice.late_payment"

const invoiceLatePaymentRefundKey = "order_late_payment"

// RefundLateInvoicePayment queues, in d's transaction, the full refund of a
// payment that arrived for an invoice that no longer takes it, and records the
// finding. It acts once per payment.
func RefundLateInvoicePayment(ctx context.Context, d *db.DB, payment *models.Payment, clock clockwork.Clock) error {
	if d == nil || d.Pool() != nil || payment == nil || payment.InvoiceID == nil {
		return errors.New("late invoice payment refund requires the recording transaction")
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	if _, err := d.Gen(ctx).GetReconciliationFindingByIdentity(ctx, gen.GetReconciliationFindingByIdentityParams{MerchantID: mid.UUID(), FindingType: InvoiceLatePaymentFindingType, SubjectKey: payment.ID.String()}); err == nil {
		return nil
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	clock = timeutil.FirstClock(clock)
	key := RefundIdempotencyKey(payment.ID, invoiceLatePaymentRefundKey)
	refund, _, err := queueFullRefund(ctx, d, mid.UUID(), payment, key, invoiceLatePaymentRefundKey, "paid after the invoice closed", billing.RefundDuplicate,
		map[string]any{"order_late_payment": true, "refund_reason": "paid after the invoice closed"}, clock)
	if err != nil {
		return err
	}
	evidence := map[string]any{
		"payment_id": payment.ID.String(), "invoice_id": payment.InvoiceID.String(), "customer_id": payment.CustomerID.String(),
		"rail": string(payment.Rail), "transaction_id": payment.TransactionID, "amount": strconv.FormatInt(payment.Amount, 10), "currency": payment.Currency,
	}
	action := "A payment arrived for an invoice already paid, or closed while what it bought was owned; its full refund is queued. Confirm the refund completes."
	if refund == "" {
		action = "A payment arrived for an invoice already paid, or closed while what it bought was owned; it was not refunded automatically. Refund it at the provider."
	} else {
		evidence["refund_intent_idempotency_key"] = refund
	}
	raw, err := json.Marshal(evidence)
	if err != nil {
		return err
	}
	_, err = d.Gen(ctx).UpsertReconciliationFinding(ctx, gen.UpsertReconciliationFindingParams{
		MerchantID: mid.UUID(), FindingType: InvoiceLatePaymentFindingType, SubjectKey: payment.ID.String(),
		Severity: "critical", Status: "requires_review", RecommendedAction: &action, Evidence: raw,
	})
	return err
}
