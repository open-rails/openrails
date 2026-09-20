package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"

	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/internal/modules/collection"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails"
	identity "github.com/open-rails/openrails/internal/billingidentity"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/modules/payments/rails"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/pkg/merchant"
)

// These commands are internal: the HTTP adapter establishes verified customer
// action authority before passing the payer. The public Client always crosses
// that same adapter, including embedded mode.
func (s *Service) PayInvoiceNow(ctx context.Context, payer identity.CustomerID, request openrails.PayInvoiceNowRequest) (*openrails.InvoicePayNowResult, error) {
	rt, err := s.runtime()
	if err != nil {
		return nil, err
	}
	var out *openrails.InvoicePayNowResult
	err = rt.DB.RunInMerchantConn(ctx, func(ctx context.Context) error {
		result, err := s.moneyService().PayInvoiceNow(ctx, rt.IntentRunner(), payer, money.InvoiceCollectionRetryRequest{InvoiceID: request.InvoiceID, PaymentMethodID: request.PaymentMethodID.UUID(), IdempotencyKey: request.IdempotencyKey})
		if err != nil {
			return err
		}
		if err := customerPaymentRefusal(result.Operation); err != nil {
			return err
		}
		out = &openrails.InvoicePayNowResult{Invoice: invoiceToDTO(result.Invoice), Attempt: invoicePaymentAttemptToDTO(result.Attempt), Operation: openrails.PaymentOperation{ID: result.Operation.ID, Status: result.Operation.Status}, Replayed: result.Replayed}
		return nil
	})
	return out, err
}

func (s *Service) RetrySubscriptionNow(ctx context.Context, payer identity.CustomerID, request openrails.RetrySubscriptionNowRequest) (*openrails.SubscriptionRetryNowResult, error) {
	rt, err := s.runtime()
	if err != nil {
		return nil, err
	}
	var out *openrails.SubscriptionRetryNowResult
	err = rt.DB.RunInMerchantConn(ctx, func(ctx context.Context) error {
		var method *uuid.UUID
		if request.PaymentMethodID != nil {
			id := request.PaymentMethodID.UUID()
			method = &id
		}
		h := intents.NewManualRebillHandler(rt.DB, rt.Config, rt.CollectionResolver, rt.Clock)
		h.DeferDelete = rt.DeferredDeletes
		accepted, replayed, err := h.EnqueueCustomer(ctx, request.SubscriptionID.UUID(), payer.UUID(), request.IdempotencyKey, method)
		if err != nil {
			return err
		}
		result, err := rt.IntentRunner().ExecuteByID(ctx, accepted.ID)
		if err != nil {
			return err
		}
		if err := customerPaymentRefusal(result); err != nil {
			return err
		}
		sub, err := rt.UserSubscriptionService.GetUserSubscriptionByID(ctx, payer.UUID().String(), request.SubscriptionID.UUID())
		if err != nil {
			return err
		}
		out = &openrails.SubscriptionRetryNowResult{Subscription: sub.View(), Operation: openrails.PaymentOperation{ID: result.ID, Status: result.Status}, Replayed: replayed}
		return nil
	})
	return out, err
}

func (s *Service) InvoiceRecovery(ctx context.Context, payer identity.CustomerID, id uuid.UUID) (*openrails.PaymentRecovery, error) {
	ctx, release, err := s.pin(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	invoice, err := s.moneyService().GetInvoiceByID(ctx, payer, id)
	if err != nil {
		return nil, err
	}
	out := &openrails.PaymentRecovery{}
	if invoice.CollectionIntentID != nil {
		row, err := intents.NewStore(s.rt.DB).Get(ctx, *invoice.CollectionIntentID)
		if err != nil {
			return nil, err
		}
		out.Operation = &openrails.PaymentOperation{ID: row.ID, Status: row.Status}
		out.BlockedReason = "payment_in_progress"
	} else if invoice.AmountDue <= 0 || (invoice.Status != "open" && invoice.Status != "past_due" && invoice.Status != "uncollectible") {
		out.BlockedReason = "invoice_not_payable"
	} else {
		out.Retryable = true
		// Existing attempts are the invoice's authoritative collection history. Read
		// only its newest payer-scoped attempt; a later pending/settled attempt does
		// not inherit an older decline.
		mid, err := merchant.Require(ctx)
		if err != nil {
			return nil, err
		}
		attempts, err := s.rt.DB.Gen(ctx).ListInvoicePaymentAttemptsByPayer(ctx, gen.ListInvoicePaymentAttemptsByPayerParams{MerchantID: mid.UUID(), CustomerID: payer.UUID(), InvoiceID: id, Limit: 1})
		if err != nil {
			return nil, err
		}
		if len(attempts) > 0 && attempts[0].Status == "failed" && attempts[0].Rail != nil && attempts[0].FailureCode != nil {
			out.LastFailureReason = payments.NormalizeFailureReason(*attempts[0].Rail, *attempts[0].FailureCode)
		}
	}
	return out, nil
}

func (s *Service) SubscriptionRecovery(ctx context.Context, payer identity.CustomerID, id uuid.UUID) (*openrails.PaymentRecovery, error) {
	ctx, release, err := s.pin(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	sub, err := subscriptions.NewSubscriptionRepo(s.rt.DB).GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if sub.CustomerID != payer.UUID() {
		return nil, pgx.ErrNoRows
	}
	out := &openrails.PaymentRecovery{}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	row, err := s.rt.DB.Gen(ctx).GetUnresolvedManualRebill(ctx, gen.GetUnresolvedManualRebillParams{MerchantID: mid.UUID(), SubscriptionID: id})
	if err == nil {
		out.Operation = &openrails.PaymentOperation{ID: row.ID, Status: row.Status}
		out.BlockedReason = "payment_in_progress"
		return out, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	if sub.Status != models.StatusPastDue {
		out.BlockedReason = "subscription_not_retryable"
		return out, nil
	}
	if sub.CurrentPeriodEndsAt != nil {
		latest, err := s.rt.DB.Gen(ctx).GetLatestManualRebillForPeriod(ctx, gen.GetLatestManualRebillForPeriodParams{MerchantID: mid.UUID(), SubscriptionID: id, PeriodStart: *sub.CurrentPeriodEndsAt})
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return nil, err
		}
		if err == nil && latest.Status == intents.StatusFailedTerminal {
			accepted, err := intents.DecodeManualRebillPayload(latest)
			if err != nil {
				return nil, err
			}
			if accepted.Renewal.CustomerID != sub.CustomerID || accepted.Renewal.PSPID != sub.PspID || accepted.Rail != string(sub.Rail) || accepted.RailSubscriptionID != sub.RailSubscriptionID || !accepted.Renewal.PeriodStart.Equal(*sub.CurrentPeriodEndsAt) {
				return nil, errors.New("latest rebill refusal does not match this subscription period")
			}
			var refusal *CustomerPaymentRefusal
			err = customerPaymentRefusal(latest)
			if errors.As(err, &refusal) {
				out.LastFailureReason = payments.NormalizeFailureReason(accepted.Rail, refusal.Code)
			} else if err != nil && !errors.Is(err, intents.ErrRebillNotRetryable) {
				return nil, err
			}
		}
	}
	if !rails.IsNMI(sub.Rail) || sub.PaymentMethodID == nil {
		out.BlockedReason = "customer_payment_unsupported"
		return out, nil
	}
	method, err := s.rt.DB.Gen(ctx).GetPaymentMethodByID(ctx, *sub.PaymentMethodID)
	if err != nil {
		return nil, err
	}
	if method.RebillDriver != models.RebillDriverOpenRails || method.Custodian != models.CustodianPSP || method.StoredCredentialRecurringRef == "" {
		out.BlockedReason = "customer_payment_unsupported"
		return out, nil
	}
	price, err := catalog.NewPriceService(s.rt.DB).GetByID(ctx, sub.PriceID)
	if err != nil {
		return nil, err
	}
	cycle := price.RecurringCycleHours()
	if cycle == nil || *cycle <= 0 || *cycle%24 != 0 || sub.CurrentPeriodEndsAt == nil {
		out.BlockedReason = "customer_payment_unsupported"
		return out, nil
	}
	if !sub.CurrentPeriodEndsAt.Add(collection.Window(*cycle)).After(s.now().UTC()) {
		out.BlockedReason = "subscription_not_retryable"
		return out, nil
	}
	out.Retryable = true
	return out, nil
}

// CustomerPaymentRefusal is a definitive accepted operation result, rendered
// through the standard card-error envelope. It is never inferred from current
// subscription/invoice state, so an old key keeps its original refusal.
type CustomerPaymentRefusal struct {
	OperationID uuid.UUID
	Code        string
}

func (r *CustomerPaymentRefusal) Error() string { return "customer payment was refused" }

func customerPaymentRefusal(row gen.OpenrailsRailIntent) error {
	if row.Status != intents.StatusFailedTerminal {
		return nil
	}
	var evidence struct {
		Declined     bool   `json:"declined"`
		FailureCode  string `json:"failure_code"`
		ResponseCode int    `json:"response_code"`
	}
	if err := json.Unmarshal(row.ResultEvidence, &evidence); err != nil {
		return err
	}
	switch row.IntentType {
	case intents.TypeManualRebill:
		if err := intents.ValidateManualRebillTerminal(row); err != nil {
			return err
		}
		if !evidence.Declined {
			return intents.ErrRebillNotRetryable
		}
		evidence.FailureCode = strconv.Itoa(evidence.ResponseCode)
	case money.TypeInvoiceCollection:
		if _, err := intents.DecodeInvoiceCollectionPayload(row); err != nil {
			return err
		}
		if !evidence.Declined {
			return money.ErrInvoiceNotRetryable
		}
	default:
		return fmt.Errorf("unexpected customer payment operation %q", row.IntentType)
	}
	if evidence.FailureCode == "" {
		return errors.New("payment refusal has no code")
	}
	return &CustomerPaymentRefusal{OperationID: row.ID, Code: evidence.FailureCode}
}
