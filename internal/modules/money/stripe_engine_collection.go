package money

import (
	"context"
	"errors"

	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/failpoint"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/modules/payments/charge"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
)

func (h *SubscriptionCollectionHandler) stripeEngineService(ctx context.Context, in gen.OpenrailsRailIntent) (*subscriptions.StripeService, error) {
	resolver, ok := h.Resolver.(intents.StripeEngineServiceResolver)
	if !ok {
		return nil, errors.New("Stripe engine resolver unavailable")
	}
	service, armed, err := resolver.ResolveStripeEngineService(ctx, in.MerchantID, in.PspID)
	if err != nil {
		return nil, err
	}
	if !armed || service == nil {
		return nil, errors.New("Stripe engine account unavailable")
	}
	return service, nil
}

func (h *SubscriptionCollectionHandler) executeStripeEngine(ctx context.Context, in gen.OpenrailsRailIntent, p subscriptions.SubscriptionCollectionPayload) intents.Outcome {
	service, err := h.stripeEngineService(ctx, in)
	if err != nil {
		return intents.Parked(err.Error())
	}
	params, err := intents.StripeEngineParams(in)
	if err != nil {
		return intents.Parked(err.Error())
	}
	_, proof, first, err := h.validateAndFence(ctx, in, p, h.firstSubmission(in))
	if err != nil {
		if errors.Is(err, errEngineObligationChanged) || errors.Is(err, charge.ErrInstrumentChanged) {
			return h.completeNotExecuted(ctx, in, p, "instrument_changed", err.Error())
		}
		return intents.Parked(err.Error())
	}
	if !first {
		return h.executeStripeEngineDecline(ctx, in)
	}
	if err := h.hit(ctx, in, failpoint.AfterFence); err != nil {
		return intents.Ambiguous(err.Error())
	}
	return h.dispatchStripe(ctx, in, p, service, params, proof)
}

// dispatchStripe creates the PaymentIntent under the operation's idempotency
// key. Only the writer of a fresh submission or resend fence calls it.
func (h *SubscriptionCollectionHandler) dispatchStripe(ctx context.Context, in gen.OpenrailsRailIntent, p subscriptions.SubscriptionCollectionPayload, service *subscriptions.StripeService, params subscriptions.StripeEnginePaymentParams, proof intents.CollectionNonexecutionProof) intents.Outcome {
	if err := h.hit(ctx, in, failpoint.BeforeProvider); err != nil {
		return intents.Ambiguous(err.Error())
	}
	result, err := service.CreateEnginePayment(ctx, params)
	if hitErr := h.hit(ctx, in, failpoint.AfterProvider); hitErr != nil {
		return intents.Ambiguous(hitErr.Error())
	}
	if errors.Is(err, charge.ErrNotDispatched) {
		return h.completeNotExecuted(ctx, in, p, "not_dispatched", charge.ErrNotDispatched.Error(), proof)
	}
	if err != nil {
		return intents.Ambiguous("Stripe engine submission requires receipt recovery")
	}
	if result.PaymentIntentID == "" {
		return intents.Ambiguous("Stripe engine response lacks accepted payment identity")
	}
	if err := intents.NewStore(h.DB).RetainCollectionCandidate(ctx, in, intents.CollectionCandidate{TransactionID: result.PaymentIntentID}); err != nil {
		return intents.Ambiguous(err.Error())
	}
	return h.executeStripeEngineDecline(ctx, in)
}

func (h *SubscriptionCollectionHandler) verifyStripeEngine(ctx context.Context, in gen.OpenrailsRailIntent, p subscriptions.SubscriptionCollectionPayload, reference string) intents.Outcome {
	service, err := h.stripeEngineService(ctx, in)
	if err != nil {
		return intents.Ambiguous(err.Error())
	}
	params, err := intents.StripeEngineParams(in)
	if err != nil {
		return intents.Ambiguous(err.Error())
	}
	result, found, err := service.ReadEnginePayment(ctx, params, reference)
	if err != nil {
		return h.unresolved(ctx, in, p, "Stripe engine receipt did not qualify: "+err.Error())
	}
	if !found {
		return h.lostSubmission(ctx, in, p)
	}
	if result.PaymentIntentID != "" && reference == "" {
		if err := intents.NewStore(h.DB).RetainCollectionCandidate(ctx, in, intents.CollectionCandidate{TransactionID: result.PaymentIntentID}); err != nil {
			return intents.Ambiguous(err.Error())
		}
	}
	switch result.State {
	case subscriptions.StripeEngineSucceeded:
		receipt, found, err := intents.ReadStripeEngineReceipt(ctx, in, service, result.PaymentIntentID)
		if err != nil || !found {
			return intents.Ambiguous("Stripe captured payment did not qualify")
		}
		return h.completePaid(ctx, in, p, receipt)
	case subscriptions.StripeEngineAuthenticationRequired:
		deadline, err := p.AuthenticationDeadline()
		if err != nil {
			return intents.Ambiguous(err.Error())
		}
		if h.now().After(deadline) {
			return intents.Retryable("abandoned authentication requires gated cancellation of the existing payment")
		}
		return intents.AmbiguousWithEvidence("Stripe engine payment requires customer authentication of the existing payment", map[string]any{"authentication_required": true, "stripe_payment_intent_id": result.PaymentIntentID})
	case subscriptions.StripeEngineDeclined:
		if result.FailureCode != "canceled" {
			return intents.Retryable("Stripe decline requires gated cancellation of the existing payment")
		}
		if err := intents.NewStore(h.DB).RetainStripeRecurringDecline(ctx, in, service, result.PaymentIntentID); err != nil {
			return intents.Ambiguous(err.Error())
		}
		return h.Verify(ctx, in)
	default:
		return intents.Ambiguous("Stripe engine payment is still processing")
	}
}

// Cancellation uses the original submission fence and the normal Execute lease.
// Recovery always reads the same payment before considering another cancel.
func (h *SubscriptionCollectionHandler) executeStripeEngineDecline(ctx context.Context, in gen.OpenrailsRailIntent) intents.Outcome {
	current, err := intents.NewStore(h.DB).Get(ctx, in.ID)
	if err != nil {
		return intents.Ambiguous(err.Error())
	}
	if h.Config == nil {
		return intents.Parked("Stripe execution mode unavailable")
	}
	if blocked, reason := intents.GateExecution(h.Config, intents.Origin(current.Origin)); blocked {
		return intents.Parked(reason)
	}
	service, err := h.stripeEngineService(ctx, current)
	if err != nil {
		return intents.Parked(err.Error())
	}
	params, err := intents.StripeEngineParams(current)
	if err != nil {
		return intents.Ambiguous(err.Error())
	}
	reference := ""
	if candidate, found, err := intents.LoadCollectionCandidate(current); err != nil {
		return intents.Ambiguous(err.Error())
	} else if found {
		reference = candidate.TransactionID
	}
	result, found, err := service.ReadEnginePayment(ctx, params, reference)
	if err != nil {
		return h.Verify(ctx, current)
	}
	if !found {
		return h.resendLostStripeSubmission(ctx, current, service, params)
	}
	if result.State == subscriptions.StripeEngineDeclined && result.FailureCode != "canceled" {
		// Cancellation erases the decline at Stripe; retain it first.
		if err := intents.NewStore(h.DB).RecordProgress(ctx, current.ID, map[string]any{"decline_code": result.FailureCode}); err != nil {
			return intents.Ambiguous(err.Error())
		}
		if _, err := service.FinalizeEngineDecline(ctx, params, result.PaymentIntentID); err != nil {
			return intents.Ambiguous(err.Error())
		}
	}
	if result.State == subscriptions.StripeEngineAuthenticationRequired {
		p, err := subscriptions.DecodeSubscriptionCollectionPayload(current)
		if err != nil {
			return intents.Ambiguous(err.Error())
		}
		deadline, err := p.AuthenticationDeadline()
		if err != nil {
			return intents.Ambiguous(err.Error())
		}
		if h.now().After(deadline) {
			if _, err := service.CancelAbandonedEnginePayment(ctx, params, result.PaymentIntentID); err != nil {
				return intents.Ambiguous(err.Error())
			}
		}
	}
	return h.Verify(ctx, current)
}

// resendLostStripeSubmission sends an armed resend with the original
// idempotency key: Stripe replays the first PaymentIntent if it ever existed.
func (h *SubscriptionCollectionHandler) resendLostStripeSubmission(ctx context.Context, in gen.OpenrailsRailIntent, service *subscriptions.StripeService, params subscriptions.StripeEnginePaymentParams) intents.Outcome {
	attempt := armedResend(in)
	if attempt == 0 {
		return h.Verify(ctx, in)
	}
	p, err := subscriptions.DecodeSubscriptionCollectionPayload(in)
	if err != nil {
		return intents.Ambiguous(err.Error())
	}
	if h.Config.EngineAdmissionHold {
		return intents.Parked("new engine payment submission is held")
	}
	_, proof, first, err := h.validateAndFence(ctx, in, p, h.resendSubmission(in, attempt))
	if err != nil {
		return h.closeChangedResend(ctx, in, p, attempt, err)
	}
	if !first {
		return h.Verify(ctx, in)
	}
	if err := h.hit(ctx, in, failpoint.AfterFence); err != nil {
		return intents.Ambiguous(err.Error())
	}
	return h.dispatchStripe(ctx, in, p, service, params, proof)
}
