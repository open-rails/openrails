package intents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/open-rails/openrails/billing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jonboulle/clockwork"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/modules/purchasedcredits"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
	"github.com/open-rails/openrails/internal/shared/timeutil"
)

// ChargeAfterCancelFindingType is the standing finding a charge collected on
// an already canceled subscription raises. Nothing resolves it automatically.
const ChargeAfterCancelFindingType = "life.charge_after_cancel"

const chargeAfterCancelRefundKey = "charge_after_cancel"

// ChargeAfterCancelRefundWindow bounds the automatic refund to a charge that
// just landed; an older one found later raises the finding only.
const ChargeAfterCancelRefundWindow = 30 * 24 * time.Hour

// RefundChargeAfterCancel records, in d's transaction, that payment was
// collected on a subscription canceled before it, and queues the payment's
// full refund. It acts once per payment: a recorded finding, whatever its
// status, means the charge was already handled. CCBill refunds cannot target
// a charge, so a CCBill charge raises the finding only.
func RefundChargeAfterCancel(ctx context.Context, d *db.DB, payment *models.Payment, clock clockwork.Clock) error {
	if d == nil || d.Pool() != nil || payment == nil {
		return errors.New("charge-after-cancel refund requires the recording transaction")
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	if _, err := d.Gen(ctx).GetReconciliationFindingByIdentity(ctx, gen.GetReconciliationFindingByIdentityParams{MerchantID: mid.UUID(), FindingType: ChargeAfterCancelFindingType, SubjectKey: payment.ID.String()}); err == nil {
		return nil
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	clock = timeutil.FirstClock(clock)
	refund := ""
	if clock.Now().Sub(payment.PurchasedAt) <= ChargeAfterCancelRefundWindow {
		key := RefundIdempotencyKey(payment.ID, chargeAfterCancelRefundKey)
		if refund, err = queueChargeAfterCancelRefund(ctx, d, mid.UUID(), payment, key, clock); err != nil {
			return err
		}
	}
	evidence := map[string]any{
		"payment_id": payment.ID.String(), "customer_id": payment.CustomerID.String(), "rail": string(payment.Rail),
		"transaction_id": payment.TransactionID, "amount": strconv.FormatInt(payment.Amount, 10), "currency": payment.Currency,
	}
	if payment.SubscriptionID != nil {
		evidence["subscription_id"] = payment.SubscriptionID.String()
	}
	action := "A charge landed on a subscription that was already canceled; its full refund is queued. Confirm the refund completes."
	if refund == "" {
		action = "A charge landed on a subscription that was already canceled and was not refunded automatically (its rail cannot, or it is too old); refund it at the provider."
	} else {
		evidence["refund_intent_idempotency_key"] = refund
	}
	raw, err := json.Marshal(evidence)
	if err != nil {
		return err
	}
	_, err = d.Gen(ctx).UpsertReconciliationFinding(ctx, gen.UpsertReconciliationFindingParams{
		MerchantID: mid.UUID(), FindingType: ChargeAfterCancelFindingType, SubjectKey: payment.ID.String(),
		Severity: "critical", Status: "requires_review", RecommendedAction: &action, Evidence: raw,
	})
	return err
}

// QueueChargeAfterCancelRefund queues, in d's transaction, the full refund of a
// charge collected after its subscription was canceled, under key: an approved
// proposal names the key. It answers key, or "" when the rail cannot refund
// automatically or nothing remains to refund.
func QueueChargeAfterCancelRefund(ctx context.Context, d *db.DB, payment *models.Payment, key string, clock clockwork.Clock) (string, error) {
	if d == nil || d.Pool() != nil || payment == nil {
		return "", errors.New("charge-after-cancel refund requires the approving transaction")
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return "", err
	}
	return queueChargeAfterCancelRefund(ctx, d, mid.UUID(), payment, key, timeutil.FirstClock(clock))
}

// queueChargeAfterCancelRefund reserves and enqueues the refund; it answers the
// intent key, or "" when the rail cannot refund automatically.
func queueChargeAfterCancelRefund(ctx context.Context, d *db.DB, merchantID uuid.UUID, payment *models.Payment, key string, clock clockwork.Clock) (string, error) {
	queued, _, err := queueFullRefund(ctx, d, merchantID, payment, key, chargeAfterCancelRefundKey, "charged after cancellation", billing.RefundChargedAfterCancel,
		map[string]any{"charge_after_cancel": true, "refund_reason": "charged after cancellation"}, clock)
	return queued, err
}

const fraudWarningRefundKey = "fraud_warning"

// QueueFraudWarningRefund queues, in d's transaction, the full refund of a
// charge an issuer reported as fraud. It answers the reservation the refund
// completes, or nil when the rail cannot refund automatically or nothing
// remains to refund.
func QueueFraudWarningRefund(ctx context.Context, d *db.DB, payment *models.Payment, clock clockwork.Clock) (*uuid.UUID, error) {
	if d == nil || d.Pool() != nil || payment == nil {
		return nil, errors.New("fraud warning refund requires the reporting transaction")
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	// A charge no refund can reach (no money moved at the rail, a usage
	// invoice's settlement, a rail that refunds nothing, no provider target)
	// has nothing to refund; what it paid for still ends.
	svc := payments.NewPaymentService(d, clock)
	refunded, err := svc.GetRefundTotalByPaymentID(ctx, payment.ID)
	if err != nil {
		return nil, err
	}
	remaining := payment.Amount - refunded
	if remaining <= 0 || svc.ValidateRefund(ctx, payment, remaining) != nil {
		return nil, nil
	}
	// Credits bought and spent cannot be given back.
	if err := purchasedcredits.New(d, clock).ValidateRefund(ctx, payment.ID, remaining); errors.Is(err, billing.ErrConflict) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	if _, _, _, err := RefundIntentFor(payment, fraudWarningRefundKey); err != nil {
		return nil, nil
	}
	if payment.Rail == models.RailStripe {
		if _, err := subscriptions.ResolveStripeRefundTarget(payment); err != nil {
			return nil, nil
		}
	}
	key := RefundIdempotencyKey(payment.ID, fraudWarningRefundKey)
	queued, reservation, err := queueFullRefund(ctx, d, mid.UUID(), payment, key, fraudWarningRefundKey, "the issuer reported fraud", billing.RefundFraudWarning,
		map[string]any{"fraud_warning": true}, timeutil.FirstClock(clock))
	if err != nil || queued == "" {
		return nil, err
	}
	return &reservation, nil
}

// queueFullRefund reserves and enqueues the refund of what remains of
// payment; it answers the intent key and the reservation, or "" when the rail
// cannot refund automatically or nothing remains.
func queueFullRefund(ctx context.Context, d *db.DB, merchantID uuid.UUID, payment *models.Payment, key, kind, reason string, why billing.RefundReason, metadata map[string]any, clock clockwork.Clock) (string, uuid.UUID, error) {
	if payment.Rail == models.RailCCBill || payment.PspID == nil {
		return "", uuid.Nil, nil
	}
	svc := payments.NewPaymentService(d, clock)
	refunded, err := svc.GetRefundTotalByPaymentID(ctx, payment.ID)
	if err != nil {
		return "", uuid.Nil, err
	}
	amount := payment.Amount - refunded
	if amount <= 0 {
		return "", uuid.Nil, nil
	}
	cents, err := moneyutil.NativeToRailMinorExact(payment.Currency, amount)
	if err != nil {
		return "", uuid.Nil, fmt.Errorf("%s refund amount: %w", kind, err)
	}
	target := payment.TransactionID
	if payment.Rail == models.RailStripe {
		if target, err = subscriptions.ResolveStripeRefundTarget(payment); err != nil {
			return "", uuid.Nil, err
		}
	}
	reservation, err := svc.ReserveRefund(ctx, payment.ID, kind+"_refund:"+payment.ID.String(), amount, why, metadata)
	if err != nil {
		return "", uuid.Nil, fmt.Errorf("reserve %s refund: %w", kind, err)
	}
	intentType, provider, _, err := RefundIntentFor(payment, kind)
	if err != nil {
		return "", uuid.Nil, err
	}
	_, err = NewStore(d).Enqueue(ctx, EnqueueParams{
		MerchantID: merchantID, Provider: provider, IntentType: intentType,
		SubscriptionID: payment.SubscriptionID, PaymentID: &payment.ID, PspID: *payment.PspID,
		Payload: RefundPayload{OriginalPaymentID: payment.ID, ReservationID: reservation.ID, AmountCents: cents,
			Currency: payment.Currency, Reason: reason, RefundReason: why, ProviderTarget: target},
		IdempotencyKey: key, NextAttemptAt: clock.Now().UTC(), Origin: OriginSystem, OriginReason: reason,
	})
	if err != nil {
		return "", uuid.Nil, fmt.Errorf("enqueue %s refund: %w", kind, err)
	}
	return key, reservation.ID, nil
}
