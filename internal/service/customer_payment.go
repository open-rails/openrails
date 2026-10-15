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
	"github.com/open-rails/openrails/internal/modules/mandates"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/internal/billingauth"
	identity "github.com/open-rails/openrails/internal/billingidentity"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/payments/rails"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
)

// RetrySubscriptionNow charges a provider-scheduled subscription's missed
// renewal now, in the customer's presence. An engine-owned one's renewal is
// an order the customer pays instead.
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
		if owned.CollectionPolicy == models.CollectionPolicyEngine {
			// Its renewal is an order: the customer pays that.
			return fmt.Errorf("%w: an engine subscription's renewal is paid as its order (recovery.order_id)", intents.ErrRebillUnsupported)
		}
		h := intents.NewManualRebillHandler(rt.DB, rt.Config, rt.CollectionResolver, rt.Clock)
		h.DeferDelete = rt.DeferredDeletes
		accepted, replayed, err := h.EnqueueCustomer(ctx, request.SubscriptionID.UUID(), payer.UUID(), request.IdempotencyKey, method)
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
		status := "processing"
		if result.Status == intents.StatusSucceeded {
			status = "succeeded"
		}
		out = &billing.SubscriptionRetryNowResult{Subscription: sub.View(), Status: status, Replayed: replayed}
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
	_, err = s.rt.DB.Gen(ctx).GetUnresolvedManualRebill(ctx, gen.GetUnresolvedManualRebillParams{MerchantID: mid.UUID(), SubscriptionID: id})
	if err == nil {
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
	charged, err := subscriptions.PaymentMethodOf(ctx, s.rt.DB.Gen(ctx), sub)
	if err != nil {
		return nil, err
	}
	if !rails.IsNMI(sub.Rail) || charged == nil {
		out.BlockedReason = "customer_payment_unsupported"
		return out, nil
	}
	method, err := s.rt.DB.Gen(ctx).GetPaymentMethodByID(ctx, gen.GetPaymentMethodByIDParams{ID: *charged, MerchantID: mid.UUID()})
	if err != nil {
		return nil, err
	}
	if sub.CollectionPolicy != models.CollectionPolicyNMISchedule || method.Custodian != models.CustodianPSP {
		out.BlockedReason = "customer_payment_unsupported"
		return out, nil
	}
	if _, err := mandates.ForSubscription(ctx, s.rt.DB.Gen(ctx), mid.UUID(), sub.CustomerID, sub.ID, method.ID, sub.PspID); err != nil {
		if !errors.Is(err, mandates.ErrMissing) && !errors.Is(err, mandates.ErrNotActive) {
			return nil, err
		}
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
	s.holdRecoveryForWritePosture(ctx, sub.MerchantID, out)
	return out, nil
}

// A recovery view must not offer a new customer payment while the executor
// holds user-origin writes. Preserve more specific existing recovery states.
func (s *Service) holdRecoveryForWritePosture(ctx context.Context, merchantID uuid.UUID, out *billing.PaymentRecovery) {
	if !out.Retryable {
		return
	}
	if blocked, _ := intents.GateExecution(ctx, s.rt.Posture(), merchantID, intents.OriginUser); blocked {
		out.Retryable = false
		out.BlockedReason = billing.CodeProviderWritesDisabled
	}
}

// CustomerPaymentRefusal is a definitive accepted operation result, rendered
// through the standard card-error envelope. It is never inferred from current
// subscription/invoice state, so an old key keeps its original refusal.
type CustomerPaymentRefusal struct {
	Code string
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

	default:
		return fmt.Errorf("unexpected customer payment operation %q", row.IntentType)
	}
	if evidence.FailureCode == "" {
		return errors.New("payment refusal has no code")
	}
	return &CustomerPaymentRefusal{Code: evidence.FailureCode}
}
