package money

import (
	"context"
	"errors"

	"github.com/open-rails/openrails/internal/db/gen"
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
	_, proof, first, err := h.validateAndFence(ctx, in, p, true)
	if err != nil {
		if errors.Is(err, errEngineObligationChanged) || errors.Is(err, charge.ErrInstrumentChanged) {
			return h.completeNotExecuted(ctx, in, p, "instrument_changed", err.Error())
		}
		return intents.Parked(err.Error())
	}
	if !first {
		return h.Verify(ctx, in)
	}
	result, err := service.CreateEnginePayment(ctx, params)
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
	return h.Verify(ctx, in)
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
		return intents.Ambiguous("Stripe engine receipt did not qualify")
	}
	if !found {
		return intents.Ambiguous("submitted Stripe payment has no exact readback; no automatic resend")
	}
	if result.PaymentIntentID != "" && reference == "" {
		if err := intents.NewStore(h.DB).RetainCollectionCandidate(ctx, in, intents.CollectionCandidate{TransactionID: result.PaymentIntentID}); err != nil {
			return intents.Ambiguous(err.Error())
		}
	}
	if err := intents.NewStore(h.DB).RecordProgress(ctx, in.ID, map[string]any{"stripe_payment_intent_id": result.PaymentIntentID, "authentication_required": result.State == subscriptions.StripeEngineAuthenticationRequired}); err != nil {
		return intents.Ambiguous(err.Error())
	}
	switch result.State {
	case subscriptions.StripeEngineSucceeded:
		receipt, found, err := intents.ReadStripeEngineReceipt(ctx, in, service, result.PaymentIntentID)
		if err != nil || !found {
			return intents.Ambiguous("Stripe captured payment did not qualify")
		}
		return h.completePaid(ctx, in, p, receipt)
	case subscriptions.StripeEngineAuthenticationRequired:
		return intents.AmbiguousWithEvidence("Stripe engine payment requires customer authentication of the existing payment", map[string]any{"authentication_required": true, "stripe_payment_intent_id": result.PaymentIntentID})
	case subscriptions.StripeEngineDeclined:
		if err := intents.NewStore(h.DB).RetainStripeRecurringDecline(ctx, in, service, result.PaymentIntentID); err != nil {
			return intents.Ambiguous(err.Error())
		}
		return h.Verify(ctx, in)
	default:
		return intents.Ambiguous("Stripe engine payment is still processing")
	}
}
