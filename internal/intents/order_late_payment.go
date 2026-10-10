package intents

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"

	"github.com/jackc/pgx/v5"
	"github.com/jonboulle/clockwork"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/shared/timeutil"
)

// OrderLatePaymentFindingType is raised when money moved on an order that had
// closed and whose ownership was taken meanwhile: the payment is refunded.
const OrderLatePaymentFindingType = "life.order.late_payment"

const orderLatePaymentRefundKey = "order_late_payment"

// RefundLateOrderPayment queues, in d's transaction, the full refund of a
// payment that arrived for a closed order, and records the finding. It acts
// once per payment.
func RefundLateOrderPayment(ctx context.Context, d *db.DB, payment *models.Payment, clock clockwork.Clock) error {
	if d == nil || d.Pool() != nil || payment == nil || payment.OrderID == nil {
		return errors.New("late order payment refund requires the recording transaction")
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	if _, err := d.Gen(ctx).GetReconciliationFindingByIdentity(ctx, gen.GetReconciliationFindingByIdentityParams{MerchantID: mid.UUID(), FindingType: OrderLatePaymentFindingType, SubjectKey: payment.ID.String()}); err == nil {
		return nil
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	clock = timeutil.FirstClock(clock)
	key := RefundIdempotencyKey(payment.ID, orderLatePaymentRefundKey)
	refund, err := queueFullRefund(ctx, d, mid.UUID(), payment, key, orderLatePaymentRefundKey, "paid after the order closed",
		map[string]any{"order_late_payment": true, "refund_reason": "paid after the order closed"}, clock)
	if err != nil {
		return err
	}
	evidence := map[string]any{
		"payment_id": payment.ID.String(), "order_id": payment.OrderID.String(), "customer_id": payment.CustomerID.String(),
		"rail": string(payment.Rail), "transaction_id": payment.TransactionID, "amount": strconv.FormatInt(payment.Amount, 10), "currency": payment.Currency,
	}
	action := "A payment arrived for an order that had closed, and what it bought is already owned; its full refund is queued. Confirm the refund completes."
	if refund == "" {
		action = "A payment arrived for an order that had closed, and what it bought is already owned; it was not refunded automatically. Refund it at the provider."
	} else {
		evidence["refund_intent_idempotency_key"] = refund
	}
	raw, err := json.Marshal(evidence)
	if err != nil {
		return err
	}
	_, err = d.Gen(ctx).UpsertReconciliationFinding(ctx, gen.UpsertReconciliationFindingParams{
		MerchantID: mid.UUID(), FindingType: OrderLatePaymentFindingType, SubjectKey: payment.ID.String(),
		Severity: "critical", Status: "requires_review", RecommendedAction: &action, Evidence: raw,
	})
	return err
}
