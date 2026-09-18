package service

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails"
	identity "github.com/open-rails/openrails/internal/billingidentity"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/dunning"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/pkg/merchant"
)

// Customer payment recovery (#809): the self routes and the host-mediated
// merchant routes both run through here, so embedded and standalone answers
// are one implementation.

var (
	ErrPaymentRecoveryRailUnsupported  = money.ErrPaymentRecoveryRailUnsupported
	ErrSubscriptionNotRetryable        = dunning.ErrSubscriptionNotRetryable
	ErrSubscriptionRetryInProgress     = dunning.ErrSubscriptionRetryInProgress
	ErrSubscriptionRetryOutcomeUnknown = dunning.ErrSubscriptionRetryOutcomeUnknown
	ErrSubscriptionNotFound            = subscriptions.ErrSubscriptionNotFound
)

// InvoiceDeclined is a pay-now attempt the provider refused: the attempt is
// recorded, the invoice carries the decline doctrine's outcome, and nothing
// was charged. The HTTP surface renders it as the 402 card_declined refusal.
type InvoiceDeclined struct {
	Result openrails.InvoicePayNowResult
}

func (e *InvoiceDeclined) Error() string {
	return fmt.Sprintf("invoice payment declined: %s", derefString(e.Result.Attempt.FailureCode))
}

// SubscriptionDeclined is a retry-now attempt the provider refused.
type SubscriptionDeclined struct {
	Result      openrails.SubscriptionRetryNowResult
	FailureCode string
	Reason      string
}

func (e *SubscriptionDeclined) Error() string {
	return fmt.Sprintf("subscription rebill declined: %s", e.Reason)
}

// PayInvoiceNow charges one of the payer's open or past-due invoices now
// through a saved method they own. A refused charge returns *InvoiceDeclined;
// an unresolved outcome returns normally with Operation.Unresolved().
func (s *Service) PayInvoiceNow(ctx context.Context, payer identity.CustomerID, request openrails.PayInvoiceNowRequest) (*openrails.InvoicePayNowResult, error) {
	rt, err := s.runtime()
	if err != nil {
		return nil, err
	}
	if rt.MoneyCharger == nil {
		return nil, fmt.Errorf("invoice collection charger not configured")
	}
	if payer.IsZero() {
		return nil, fmt.Errorf("payer required")
	}
	var out *openrails.InvoicePayNowResult
	err = s.rt.DB.RunInMerchantConn(ctx, func(ctx context.Context) error {
		result, err := s.moneyService().PayInvoiceNow(ctx, rt.IntentRunner(), payer, money.InvoiceCollectionRetryRequest{
			InvoiceID: request.InvoiceID, IdempotencyKey: request.IdempotencyKey, PaymentMethodID: request.PaymentMethodID.UUID(),
		})
		if err != nil {
			return err
		}
		operation, err := s.rt.DB.Gen(ctx).GetRailIntent(ctx, result.OperationID)
		if err != nil {
			return fmt.Errorf("load collection operation: %w", err)
		}
		invoice := invoiceToDTO(result.Invoice)
		recovery, err := s.moneyService().InvoiceRecovery(ctx, payer, []*models.Invoice{result.Invoice})
		if err != nil {
			return err
		}
		invoice.Recovery = recovery[result.Invoice.ID]
		out = &openrails.InvoicePayNowResult{
			Invoice: invoice, Attempt: invoicePaymentAttemptToDTO(result.Attempt),
			Operation: openrails.PaymentOperation{ID: operation.ID, Status: operation.Status}, Replayed: result.Replayed,
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if out.Attempt.Status == "failed" {
		return nil, &InvoiceDeclined{Result: *out}
	}
	return out, nil
}

// RetrySubscriptionNow rebills one of the payer's past-due subscriptions now.
// A refused charge returns *SubscriptionDeclined; an unresolved outcome
// returns normally with Operation.Unresolved().
func (s *Service) RetrySubscriptionNow(ctx context.Context, payer identity.CustomerID, request openrails.RetrySubscriptionNowRequest) (*openrails.SubscriptionRetryNowResult, error) {
	rt, err := s.runtime()
	if err != nil {
		return nil, err
	}
	if rt.SubscriptionLifecycleService == nil || rt.UserSubscriptionService == nil {
		return nil, fmt.Errorf("subscription lifecycle not configured")
	}
	if payer.IsZero() {
		return nil, fmt.Errorf("payer required")
	}
	var out *openrails.SubscriptionRetryNowResult
	var declined *SubscriptionDeclined
	err = s.rt.DB.RunInMerchantConn(ctx, func(ctx context.Context) error {
		retry := &dunning.RetryNow{DB: rt.DB, Runner: rt.IntentRunner(), Lifecycle: rt.SubscriptionLifecycleService, Clock: rt.Clock}
		var method *uuid.UUID
		if request.PaymentMethodID != nil {
			id := request.PaymentMethodID.UUID()
			method = &id
		}
		result, err := retry.Run(ctx, dunning.RetryNowRequest{Payer: payer, SubscriptionID: request.SubscriptionID.UUID(), PaymentMethodID: method, IdempotencyKey: request.IdempotencyKey})
		if err != nil {
			return err
		}
		view, err := s.customerSubscription(ctx, payer, result.Subscription.ID)
		if err != nil {
			return err
		}
		out = &openrails.SubscriptionRetryNowResult{
			Subscription: *view,
			Operation:    openrails.PaymentOperation{ID: result.Intent.ID, Status: result.Intent.Status},
			Replayed:     result.Replayed,
		}
		if result.TransactionID != "" {
			payment, err := s.confirmedRenewal(ctx, result.Subscription, result.TransactionID)
			if err != nil {
				return err
			}
			out.Payment = payment
		}
		if result.Declined != nil {
			declined = &SubscriptionDeclined{Result: *out, FailureCode: result.Declined.FailureCode, Reason: result.Declined.Reason}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if declined != nil {
		return nil, declined
	}
	return out, nil
}

// customerSubscription is the payer's own view of one subscription, with its
// recovery state.
func (s *Service) customerSubscription(ctx context.Context, payer identity.CustomerID, id uuid.UUID) (*openrails.Subscription, error) {
	resp, err := s.rt.UserSubscriptionService.GetUserSubscriptionByID(ctx, payer.String(), id)
	if err != nil {
		return nil, err
	}
	view := resp.View()
	recovery, err := s.SubscriptionRecovery(ctx, resp.Subscription)
	if err != nil {
		return nil, err
	}
	view.Recovery = recovery
	return &view, nil
}

// SubscriptionRecovery projects the retry-now state onto a subscription the
// caller already loaded for its owner.
func (s *Service) SubscriptionRecovery(ctx context.Context, sub *models.Subscription) (*openrails.PaymentRecovery, error) {
	rt, err := s.runtime()
	if err != nil {
		return nil, err
	}
	var out *openrails.PaymentRecovery
	err = rt.DB.RunInMerchantConn(ctx, func(ctx context.Context) error {
		out, err = dunning.RecoveryView{DB: rt.DB, Money: rt.MoneyService, Clock: rt.Clock}.Subscription(ctx, sub)
		return err
	})
	return out, err
}

// confirmedRenewal is the payment row the confirmed rebill wrote.
func (s *Service) confirmedRenewal(ctx context.Context, sub *models.Subscription, transactionID string) (*openrails.SubscriptionPayment, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	psp := sub.PspID
	row, err := s.rt.DB.Gen(ctx).GetPaymentByPSPTransactionID(ctx, gen.GetPaymentByPSPTransactionIDParams{MerchantID: mid.UUID(), PspID: &psp, Rail: string(sub.Rail), TransactionID: transactionID})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("load confirmed renewal payment: %w", err)
	}
	return &openrails.SubscriptionPayment{
		ID: openrails.PaymentID(row.ID), Status: string(row.Status), Amount: row.Amount, Currency: row.Currency,
		Rail: string(row.Rail), TransactionID: row.TransactionID, PurchasedAt: row.PurchasedAt,
	}, nil
}

func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
