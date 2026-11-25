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

// An engine-owned subscription's unpaid renewal is an invoice: the recovery
// names it, and its payment says what the customer can do. A view, never
// authority to charge: paying the invoice checks everything again.
func (s *Service) engineSubscriptionRecovery(ctx context.Context, sub *models.Subscription) (*billing.PaymentRecovery, error) {
	out := &billing.PaymentRecovery{}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	invoice, err := s.rt.DB.Gen(ctx).GetOpenRenewalInvoice(ctx, gen.GetOpenRenewalInvoiceParams{MerchantID: mid.UUID(), SubscriptionID: sub.ID})
	if errors.Is(err, pgx.ErrNoRows) {
		out.BlockedReason = "subscription_not_retryable"
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	id := billing.InvoiceID(invoice.ID)
	out.InvoiceID = &id
	switch billing.InvoicePaymentStatus(invoice.PaymentStatus) {
	case billing.InvoicePaymentRequiresAction:
		out.BlockedReason = "authentication_required"
	case billing.InvoicePaymentProcessing:
		out.BlockedReason = "payment_in_progress"
	default:
		out.Retryable = true
		var failure billing.PaymentFailure
		if len(invoice.LastPaymentError) > 0 && json.Unmarshal(invoice.LastPaymentError, &failure) == nil {
			out.LastFailureReason = failure.Reason
		}
	}
	s.holdRecoveryForWritePosture(ctx, sub.MerchantID, out)
	return out, nil
}
