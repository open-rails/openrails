package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/decline"
	"github.com/open-rails/openrails/internal/modules/catalog"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/internal/billingauth"
	identity "github.com/open-rails/openrails/internal/billingidentity"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/internal/modules/payments/rails"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
)

func (s *Service) RetrySubscriptionNow(ctx context.Context, payer identity.CustomerID, request billing.RetrySubscriptionNowParams, principal billingauth.Payer) (*billing.SubscriptionRetryNowResult, error) {
	rt, err := s.runtime()
	if err != nil {
		return nil, err
	}
	var out *billing.SubscriptionRetryNowResult
	err = rt.DB.RunInMerchantConn(ctx, func(ctx context.Context) error {
		var method *uuid.UUID
		if request.PaymentMethodID != nil {
			id := request.PaymentMethodID.UUID()
			method = &id
		}
		owned, err := rt.UserSubscriptionService.GetUserSubscriptionByID(ctx, payer.UUID().String(), request.SubscriptionID.UUID())
		if err != nil {
			return err
		}
		var accepted gen.BillingProviderIntent
		var replayed bool
		if owned.CollectionPolicy == models.CollectionPolicyEngine {
			accepted, replayed, err = s.moneyService().AdmitCustomerSubscriptionCollection(ctx, owned.ID, payer.UUID(), request.IdempotencyKey, method, principal)
		} else {
			h := intents.NewManualRebillHandler(rt.DB, rt.Config, rt.CollectionResolver, rt.Clock)
			h.DeferDelete = rt.DeferredDeletes
			accepted, replayed, err = h.EnqueueCustomer(ctx, request.SubscriptionID.UUID(), payer.UUID(), request.IdempotencyKey, method)
		}
		if err != nil {
			return err
		}
		var result gen.BillingProviderIntent
		if accepted.Status == intents.StatusUnknownNeedsVerify {
			result, err = rt.IntentRunner().VerifyByID(ctx, accepted.ID)
		} else {
			result, err = rt.IntentRunner().ExecuteByID(ctx, accepted.ID)
		}
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
		out = &billing.SubscriptionRetryNowResult{Subscription: sub.View(), Operation: billing.PaymentOperation{ID: billing.PaymentOperationID(result.ID), Status: result.Status}, Replayed: replayed}
		return nil
	})
	return out, err
}

func (s *Service) SubscriptionRecovery(ctx context.Context, payer identity.CustomerID, id uuid.UUID) (*billing.PaymentRecovery, error) {
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
	if sub.CollectionPolicy == models.CollectionPolicyEngine {
		return s.engineSubscriptionRecovery(ctx, sub)
	}
	out := &billing.PaymentRecovery{}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	row, err := s.rt.DB.Gen(ctx).GetUnresolvedManualRebill(ctx, gen.GetUnresolvedManualRebillParams{MerchantID: mid.UUID(), SubscriptionID: id})
	if err == nil {
		out.Operation = &billing.PaymentOperation{ID: billing.PaymentOperationID(row.ID), Status: row.Status}
		out.BlockedReason = "payment_in_progress"
		return out, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	if sub.Status != models.StatusPastDue && sub.Status != models.StatusAwaitingMethod {
		out.BlockedReason = "subscription_not_retryable"
		return out, nil
	}
	if sub.CurrentPeriodEndsAt != nil {
		latest, err := s.rt.DB.Gen(ctx).GetLatestManualRebillForPeriod(ctx, gen.GetLatestManualRebillForPeriodParams{MerchantID: mid.UUID(), SubscriptionID: id, PeriodStart: *sub.CurrentPeriodEndsAt})
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return nil, err
		}
		if err == nil && latest.Status == intents.StatusFailedTerminal {
			accepted, err := subscriptions.DecodeManualRebillPayload(latest)
			if err != nil {
				return nil, err
			}
			if accepted.Renewal.CustomerID != sub.CustomerID || !accepted.Renewal.PeriodStart.Equal(*sub.CurrentPeriodEndsAt) {
				return nil, errors.New("latest rebill refusal does not match this subscription period")
			}
			var refusal *CustomerPaymentRefusal
			err = customerPaymentRefusal(latest)
			if errors.As(err, &refusal) {
				// A valid historical account/refusal remains valid custody after a cutover,
				// but it is not a failure of the replacement provider binding.
				if accepted.Renewal.PSPID == sub.PspID && accepted.Rail == string(sub.Rail) && accepted.RailSubscriptionID == sub.RailSubscriptionID {
					out.LastFailureReason = decline.ReasonFor(accepted.Rail, refusal.Code)
				}
			} else if err != nil && !errors.Is(err, intents.ErrRebillNotRetryable) {
				return nil, err
			}
		}
	}
	if !rails.IsNMI(sub.Rail) || sub.PaymentMethodID == nil {
		out.BlockedReason = "customer_payment_unsupported"
		return out, nil
	}
	method, err := s.rt.DB.Gen(ctx).GetPaymentMethodByID(ctx, gen.GetPaymentMethodByIDParams{ID: *sub.PaymentMethodID, MerchantID: mid.UUID()})
	if err != nil {
		return nil, err
	}
	if sub.CollectionPolicy != models.CollectionPolicyNMISchedule || method.Custodian != models.CustodianPSP || method.StoredCredentialRecurringRef == nil {
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
	policy, err := subscriptions.CasePolicy(ctx, s.rt.DB, sub)
	if err != nil {
		return nil, err
	}
	window, err := policy.Window(*cycle)
	if err != nil {
		return nil, err
	}
	if !sub.CurrentPeriodEndsAt.Add(window).After(s.now().UTC()) {
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

func customerPaymentRefusal(row gen.BillingProviderIntent) error {
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
	case subscriptions.TypeSubscriptionCollection:
		if err := intents.ValidateSubscriptionCollectionTerminal(row); err != nil {
			return err
		}
		if !evidence.Declined {
			return intents.ErrRebillNotRetryable
		}
		if row.Rail != "stripe" {
			evidence.FailureCode = strconv.Itoa(evidence.ResponseCode)
		}
	case subscriptions.TypeManualRebill:
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
