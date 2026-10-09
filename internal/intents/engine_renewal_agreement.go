package intents

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
)

// PrepareEngineRenewalTerms qualifies the exact paid predecessor under the
// admission transaction's subscription lock. A current catalog row is never
// evidence of what this customer accepted. Missing/pruned/ambiguous custody or
// a payment-only completion cannot authorize another recurring charge.
func PrepareEngineRenewalTerms(ctx context.Context, d *db.DB, sub *models.Subscription, now time.Time) (subscriptions.RenewalTerms, error) {
	var agreement subscriptions.RenewalTerms
	if d == nil || d.Pool() != nil || sub == nil || sub.CollectionPolicy != models.CollectionPolicyEngine || sub.CurrentPeriodEndsAt == nil {
		return agreement, fmt.Errorf("%w: engine agreement requires a locked current obligation", ErrRebillNotRetryable)
	}
	rows, err := d.Gen(ctx).ListPaidEngineAgreementsAtBoundary(ctx, gen.ListPaidEngineAgreementsAtBoundaryParams{MerchantID: sub.MerchantID, SubscriptionID: sub.ID, PeriodEnd: *sub.CurrentPeriodEndsAt})
	if err != nil {
		return agreement, err
	}
	if len(rows) != 1 {
		return agreement, fmt.Errorf("%w: engine paid agreement is missing or ambiguous", ErrRebillNotRetryable)
	}
	op := rows[0]
	var accepted subscriptions.InitialMembershipTerms
	var payment gen.BillingPayment
	switch op.IntentType {
	case subscriptions.TypeInitialMembership:
		if err := ValidateInitialMembershipTerminal(op); err != nil {
			return agreement, fmt.Errorf("%w: %w", ErrRebillNotRetryable, err)
		}
		p, err := subscriptions.DecodeInitialMembershipPayload(op)
		if err != nil {
			return agreement, fmt.Errorf("%w: %w", ErrRebillNotRetryable, err)
		}
		if p.Terms.CollectionPolicy != models.CollectionPolicyEngine || op.Origin != string(OriginUser) || op.Actor == nil || *op.Actor != p.Terms.CustomerID.String() {
			return agreement, fmt.Errorf("%w: engine initial agreement lacks its customer acceptance", ErrRebillNotRetryable)
		}
		accepted = p.Terms
		agreement = subscriptions.RenewalTerms{PSPID: accepted.PSPID, SubscriptionID: accepted.SubscriptionID, CustomerID: accepted.CustomerID, FromPriceID: accepted.PriceID, FromProductID: accepted.ProductID, PriceID: accepted.PriceID, ProductID: accepted.ProductID, ProductName: accepted.ProductName, Quantity: accepted.Quantity, Amount: accepted.RecurringAmount, Currency: accepted.Currency, PeriodStart: accepted.PeriodStart, PeriodEnd: accepted.PeriodEnd, AccessDurationHours: accepted.AccessDurationHours}
		payment, err = d.Gen(ctx).GetPaymentByID(ctx, gen.GetPaymentByIDParams{MerchantID: op.MerchantID, ID: accepted.PaymentID})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return agreement, fmt.Errorf("%w: paid agreement payment is missing", ErrRebillNotRetryable)
			}
			return agreement, err
		}
		if payment.AttemptKind == nil || *payment.AttemptKind != payments.AttemptInitial {
			return agreement, fmt.Errorf("%w: engine predecessor is not its initial payment", ErrRebillNotRetryable)
		}
	case subscriptions.TypeSubscriptionCollection:
		if err := ValidateSubscriptionCollectionTerminal(op); err != nil {
			return agreement, fmt.Errorf("%w: %w", ErrRebillNotRetryable, err)
		}
		p, err := subscriptions.DecodeSubscriptionCollectionPayload(op)
		if err != nil {
			return agreement, fmt.Errorf("%w: %w", ErrRebillNotRetryable, err)
		}
		agreement = p.Renewal
		receipt, _, err := LoadCollectedReceipt(op)
		if err != nil {
			return agreement, fmt.Errorf("%w: %w", ErrRebillNotRetryable, err)
		}
		payment, err = d.Gen(ctx).GetPaymentByPSPTransactionID(ctx, gen.GetPaymentByPSPTransactionIDParams{MerchantID: op.MerchantID, PspID: op.PspID, Channel: string(models.ChannelRail), Rail: &op.Rail, TransactionID: receipt.TransactionID()})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return agreement, fmt.Errorf("%w: paid agreement payment is missing", ErrRebillNotRetryable)
			}
			return agreement, err
		}
		accepted = subscriptions.InitialMembershipTerms{PaymentID: payment.ID, PSPID: agreement.PSPID, SubscriptionID: agreement.SubscriptionID, CustomerID: agreement.CustomerID, PriceID: agreement.PriceID, ProductID: agreement.ProductID, Amount: agreement.Amount, RecurringAmount: agreement.Amount, Currency: agreement.Currency}
		if payment.AttemptKind == nil || *payment.AttemptKind != payments.AttemptRenewal {
			return agreement, fmt.Errorf("%w: engine predecessor is not a recurring payment", ErrRebillNotRetryable)
		}
	case payments.TypeNMISale:
		agreement, payment, err = orderAgreement(ctx, d, sub, op)
		if err != nil {
			return agreement, err
		}
	default:
		return agreement, fmt.Errorf("%w: engine predecessor is not an accepted membership operation", ErrRebillNotRetryable)
	}
	receipt, paid, err := LoadCollectedReceipt(op)
	if err != nil {
		return agreement, fmt.Errorf("%w: %w", ErrRebillNotRetryable, err)
	}
	if !paid {
		return agreement, fmt.Errorf("%w: engine predecessor has no paid receipt", ErrRebillNotRetryable)
	}
	observed, err := models.PaymentFromGen(payment)
	if err != nil {
		return agreement, fmt.Errorf("%w: %w", ErrRebillNotRetryable, err)
	}
	if _, moneyOnly := observed.Metadata["refund_review"]; moneyOnly {
		return agreement, fmt.Errorf("%w: payment-only completion is not a recurring agreement", ErrRebillNotRetryable)
	}
	if op.IntentType == payments.TypeNMISale {
		if observed.TransactionID != receipt.TransactionID() || observed.Rail != models.Rail(op.Rail) {
			return agreement, fmt.Errorf("%w: order payment contradicts its receipt", ErrRebillNotRetryable)
		}
	} else if err := subscriptions.ValidateInitialMembershipPayment(accepted, observed, models.Rail(op.Rail), receipt.TransactionID()); err != nil {
		return agreement, fmt.Errorf("%w: %w", ErrRebillNotRetryable, err)
	}
	terms, err := subscriptions.PrepareRenewalTerms(ctx, d, sub, now, &agreement)
	if errors.Is(err, subscriptions.ErrEngineAgreementMismatch) {
		return terms, fmt.Errorf("%w: %w", ErrRebillNotRetryable, err)
	}
	return terms, err
}

// orderAgreement is the first period a paid order bought for the subscription
// its recurring line made: the line's frozen price, seats and interval from
// the moment the order was paid.
func orderAgreement(ctx context.Context, d *db.DB, sub *models.Subscription, op gen.BillingProviderIntent) (subscriptions.RenewalTerms, gen.BillingPayment, error) {
	var agreement subscriptions.RenewalTerms
	var payment gen.BillingPayment
	if err := ValidateNMISaleTerminal(op); err != nil {
		return agreement, payment, fmt.Errorf("%w: %w", ErrRebillNotRetryable, err)
	}
	p, err := payments.DecodeNMISalePayload(op)
	if err != nil || p.OrderID == uuid.Nil || !p.Recurring || op.Origin != string(OriginUser) {
		return agreement, payment, fmt.Errorf("%w: engine predecessor is not an order's customer payment", ErrRebillNotRetryable)
	}
	q := d.Gen(ctx)
	line, err := q.GetOrderLineBySubscription(ctx, gen.GetOrderLineBySubscriptionParams{MerchantID: sub.MerchantID, SubscriptionID: sub.ID})
	if err != nil {
		return agreement, payment, fmt.Errorf("%w: %w", ErrRebillNotRetryable, err)
	}
	order, err := q.GetOrder(ctx, gen.GetOrderParams{MerchantID: sub.MerchantID, ID: p.OrderID})
	if err != nil {
		return agreement, payment, fmt.Errorf("%w: %w", ErrRebillNotRetryable, err)
	}
	if line.OrderID != order.ID || order.PaidAt == nil || order.PaymentID == nil || *order.PaymentID != p.PaymentID || line.BillingIntervalHours == nil || order.CustomerID != sub.CustomerID {
		return agreement, payment, fmt.Errorf("%w: order line contradicts its subscription", ErrRebillNotRetryable)
	}
	if payment, err = q.GetPaymentByID(ctx, gen.GetPaymentByIDParams{MerchantID: op.MerchantID, ID: p.PaymentID}); err != nil {
		return agreement, payment, fmt.Errorf("%w: paid agreement payment is missing", ErrRebillNotRetryable)
	}
	if payment.OrderID == nil || *payment.OrderID != order.ID || payment.AttemptKind == nil || *payment.AttemptKind != payments.AttemptInitial || payment.Amount != order.Total || payment.Status != payments.PaymentStatusSucceededValue {
		return agreement, payment, fmt.Errorf("%w: order payment contradicts its order", ErrRebillNotRetryable)
	}
	var access *int
	if line.AccessDurationHours != nil {
		hours := int(*line.AccessDurationHours)
		access = &hours
	}
	start := *order.PaidAt
	agreement = subscriptions.RenewalTerms{PSPID: *op.PspID, SubscriptionID: sub.ID, CustomerID: order.CustomerID, FromPriceID: line.PriceID, FromProductID: line.ProductID,
		PriceID: line.PriceID, ProductID: line.ProductID, ProductName: line.Description, Amount: line.Amount, Currency: order.Currency,
		PeriodStart: start, PeriodEnd: start.Add(time.Duration(*line.BillingIntervalHours) * time.Hour), AccessDurationHours: access}
	return agreement, payment, nil
}
