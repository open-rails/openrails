package service

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/merchant"
)

// An engine-owned subscription's unpaid renewal is an order: the recovery
// names it, and its payment says what the customer can do. A view, never
// authority to charge: paying the order checks everything again.
func (s *Service) engineSubscriptionRecovery(ctx context.Context, sub *models.Subscription) (*billing.PaymentRecovery, error) {
	out := &billing.PaymentRecovery{}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	order, err := s.rt.DB.Gen(ctx).GetOpenRenewalOrder(ctx, gen.GetOpenRenewalOrderParams{MerchantID: mid.UUID(), SubscriptionID: sub.ID})
	if errors.Is(err, pgx.ErrNoRows) {
		out.BlockedReason = "subscription_not_retryable"
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	id := billing.OrderID(order.ID)
	out.OrderID = &id
	switch billing.OrderPaymentStatus(order.PaymentStatus) {
	case billing.OrderPaymentRequiresAction:
		out.BlockedReason = "authentication_required"
	case billing.OrderPaymentProcessing:
		out.BlockedReason = "payment_in_progress"
	default:
		out.Retryable = true
		var failure billing.PaymentFailure
		if len(order.LastPaymentError) > 0 && json.Unmarshal(order.LastPaymentError, &failure) == nil {
			out.LastFailureReason = failure.Reason
		}
	}
	return out, nil
}
