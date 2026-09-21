package checkout

import (
	"context"
	"errors"

	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/modules/payments/charge"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
)

func (h *InitialMembershipIntentHandler) stripeEngineService(ctx context.Context, in gen.OpenrailsRailIntent) (*subscriptions.StripeService, error) {
	resolver, ok := h.Resolver.(intents.StripeEngineServiceResolver)
	if !ok {
		return nil, errors.New("Stripe engine account resolver unavailable")
	}
	service, found, err := resolver.ResolveStripeEngineService(ctx, in.MerchantID, in.PspID)
	if err != nil {
		return nil, err
	}
	if !found || service == nil {
		return nil, errors.New("Stripe engine account unavailable")
	}
	return service, nil
}
func (h *InitialMembershipIntentHandler) executeStripeInitial(ctx context.Context, in gen.OpenrailsRailIntent, p InitialMembershipPayload) intents.Outcome {
	if h.Checkout.Config == nil || h.Checkout.Config.IsProviderReadOnly() {
		return intents.Parked("Stripe engine writes unavailable")
	}
	service, err := h.stripeEngineService(ctx, in)
	if err != nil {
		return intents.Parked(err.Error())
	}
	params, err := intents.StripeEngineParams(in)
	if err != nil {
		return intents.Parked(err.Error())
	}
	proof, first, err := h.fenceInitialMembership(ctx, in, p)
	if err != nil {
		return intents.Ambiguous(err.Error())
	}
	if !first {
		return h.Verify(ctx, in)
	}
	result, err := service.CreateEnginePayment(ctx, params)
	if errors.Is(err, charge.ErrNotDispatched) {
		return h.completeInitialNonexecution(ctx, in, proof)
	}
	if err != nil {
		return intents.Ambiguous("Stripe initial payment requires exact payment recovery")
	}
	if result.PaymentIntentID == "" {
		return intents.Ambiguous("Stripe initial payment has no retained identity")
	}
	if err := intents.NewStore(h.database()).RecordProgress(ctx, in.ID, map[string]any{"stripe_payment_intent_id": result.PaymentIntentID}); err != nil {
		return intents.Ambiguous(err.Error())
	}
	return h.Verify(ctx, in)
}
func (h *InitialMembershipIntentHandler) verifyStripeInitial(ctx context.Context, in gen.OpenrailsRailIntent, p InitialMembershipPayload) intents.Outcome {
	service, err := h.stripeEngineService(ctx, in)
	if err != nil {
		return intents.Ambiguous(err.Error())
	}
	params, err := intents.StripeEngineParams(in)
	if err != nil {
		return intents.Ambiguous(err.Error())
	}
	result, found, err := service.ReadEnginePayment(ctx, params, intents.EvidenceString(in, "stripe_payment_intent_id"))
	if err != nil {
		return intents.Ambiguous(err.Error())
	}
	if !found {
		return intents.Ambiguous("Stripe initial payment is unresolved; no automatic resend")
	}
	if err := intents.NewStore(h.database()).RecordProgress(ctx, in.ID, map[string]any{"stripe_payment_intent_id": result.PaymentIntentID}); err != nil {
		return intents.Ambiguous(err.Error())
	}
	switch result.State {
	case subscriptions.StripeEngineAuthenticationRequired:
		return intents.AmbiguousWithEvidence("Stripe payment requires customer authentication", map[string]any{"authentication_required": true, "stripe_payment_intent_id": result.PaymentIntentID})
	case subscriptions.StripeEngineDeclined:
		if err := intents.NewStore(h.database()).RetainInitialStripeDecline(ctx, in, service, result.PaymentIntentID); err != nil {
			return intents.Ambiguous(err.Error())
		}
		return h.Verify(ctx, in)
	case subscriptions.StripeEngineSucceeded:
		receipt, found, err := intents.ReadStripeEngineReceipt(ctx, in, service, result.PaymentIntentID)
		if err != nil {
			return intents.Ambiguous(err.Error())
		}
		if !found {
			return intents.Ambiguous("Stripe payment has no qualified captured receipt")
		}
		if _, err := intents.NewStore(h.database()).RetainCollectedReceipt(ctx, in, receipt); err != nil {
			return intents.Ambiguous(err.Error())
		}
		return h.complete(ctx, in, intents.Succeeded(nil))
	default:
		return intents.Ambiguous("Stripe payment is still processing")
	}
}
