package checkout

import (
	"context"
	"errors"

	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/modules/payments/charge"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
)

// A Stripe one-time sale charges the saved card through the engine
// PaymentIntent. It shares the NMI sale's admission, submission fence,
// qualified receipt and completion; only provider execution and readback differ.

const stripeSaleIntentKey = "stripe_payment_intent_id"

func (h *NMISaleIntentHandler) stripeService(ctx context.Context, in gen.OpenrailsRailIntent) (*subscriptions.StripeService, error) {
	if h.Sale == nil || h.Sale.StripeEngines == nil {
		return nil, errors.New("Stripe sale account resolver unavailable")
	}
	service, found, err := h.Sale.StripeEngines.ResolveStripeEngineService(ctx, in.MerchantID, in.PspID)
	if err != nil {
		return nil, err
	}
	if !found || service == nil {
		return nil, errors.New("Stripe sale account unavailable")
	}
	return service, nil
}

// submitStripeSale runs only for the unique submission-fence winner.
func (h *NMISaleIntentHandler) submitStripeSale(ctx context.Context, in gen.OpenrailsRailIntent) intents.Outcome {
	service, err := h.stripeService(ctx, in)
	if err != nil {
		return intents.Ambiguous("submitted Stripe sale account unavailable: " + err.Error())
	}
	params, err := intents.StripeEngineParams(in)
	if err != nil {
		return intents.Ambiguous(err.Error())
	}
	result, err := service.CreateEnginePayment(ctx, params)
	if errors.Is(err, charge.ErrNotDispatched) {
		evidence := map[string]any{"request_refused": true}
		if err := intents.NewStore(h.database()).RecordProgress(ctx, in.ID, evidence); err != nil {
			return intents.Ambiguous("cannot retain sale refusal: " + err.Error())
		}
		return h.complete(ctx, in, nil, intents.TerminalWithEvidence("provider refused the sale request", evidence))
	}
	if err != nil || result.PaymentIntentID == "" {
		return intents.Ambiguous("Stripe sale outcome requires provider readback")
	}
	if err := intents.NewStore(h.database()).RecordProgress(ctx, in.ID, map[string]any{stripeSaleIntentKey: result.PaymentIntentID}); err != nil {
		return intents.Ambiguous(err.Error())
	}
	return h.recoverStripeSale(ctx, in)
}

// recoverStripeSale is the Execute-lease path after submission: it may cancel
// the SAME declined PaymentIntent (gated) before settling the refusal.
func (h *NMISaleIntentHandler) recoverStripeSale(ctx context.Context, in gen.OpenrailsRailIntent) intents.Outcome {
	current, err := intents.NewStore(h.database()).Get(ctx, in.ID)
	if err != nil {
		return intents.Ambiguous(err.Error())
	}
	if h.Sale.Config == nil {
		return intents.Parked("Stripe execution mode unavailable")
	}
	if blocked, reason := intents.GateExecution(h.Sale.Config, intents.Origin(current.Origin)); blocked {
		return intents.Parked(reason)
	}
	service, err := h.stripeService(ctx, current)
	if err != nil {
		return intents.Parked(err.Error())
	}
	params, err := intents.StripeEngineParams(current)
	if err != nil {
		return intents.Ambiguous(err.Error())
	}
	result, found, err := service.ReadEnginePayment(ctx, params, intents.EvidenceString(current, stripeSaleIntentKey))
	if err != nil || !found {
		return intents.Ambiguous("submitted Stripe sale requires exact readback; no resend")
	}
	if result.State == subscriptions.StripeEngineAuthenticationRequired && h.authenticationAbandoned(current) {
		if err := intents.NewStore(h.database()).RecordProgress(ctx, current.ID, map[string]any{"decline_code": "authentication_required"}); err != nil {
			return intents.Ambiguous(err.Error())
		}
		if _, err := service.CancelAbandonedEnginePayment(ctx, params, result.PaymentIntentID); err != nil {
			return intents.Ambiguous(err.Error())
		}
		if current, err = intents.NewStore(h.database()).Get(ctx, current.ID); err != nil {
			return intents.Ambiguous(err.Error())
		}
	}
	if result.State == subscriptions.StripeEngineDeclined && result.FailureCode != "canceled" {
		// Cancellation erases the decline reason at Stripe; retain it first.
		if err := intents.NewStore(h.database()).RecordProgress(ctx, current.ID, map[string]any{"decline_code": result.FailureCode}); err != nil {
			return intents.Ambiguous(err.Error())
		}
		if _, err := service.FinalizeEngineDecline(ctx, params, result.PaymentIntentID); err != nil {
			return intents.Ambiguous(err.Error())
		}
		if current, err = intents.NewStore(h.database()).Get(ctx, current.ID); err != nil {
			return intents.Ambiguous(err.Error())
		}
	}
	return h.verifyStripeSale(ctx, current)
}

// verifyStripeSale never writes to the provider.
func (h *NMISaleIntentHandler) verifyStripeSale(ctx context.Context, in gen.OpenrailsRailIntent) intents.Outcome {
	service, err := h.stripeService(ctx, in)
	if err != nil {
		return intents.Ambiguous(err.Error())
	}
	params, err := intents.StripeEngineParams(in)
	if err != nil {
		return intents.Ambiguous(err.Error())
	}
	result, found, err := service.ReadEnginePayment(ctx, params, intents.EvidenceString(in, stripeSaleIntentKey))
	if err != nil {
		return intents.Ambiguous(err.Error())
	}
	if !found {
		return intents.Ambiguous("Stripe sale is unresolved; no automatic resend")
	}
	store := intents.NewStore(h.database())
	progress := map[string]any{stripeSaleIntentKey: result.PaymentIntentID, "authentication_required": result.State == subscriptions.StripeEngineAuthenticationRequired}
	if err := store.RecordProgress(ctx, in.ID, progress); err != nil {
		return intents.Ambiguous(err.Error())
	}
	switch result.State {
	case subscriptions.StripeEngineAuthenticationRequired:
		if h.authenticationAbandoned(in) {
			return intents.Retryable("abandoned authentication requires gated cancellation of the same payment")
		}
		return intents.Ambiguous("Stripe sale requires customer authentication")
	case subscriptions.StripeEngineDeclined:
		if result.FailureCode != "canceled" {
			return intents.Retryable("Stripe decline requires gated cancellation of the same payment")
		}
		code := result.DeclineCode
		if retained := intents.EvidenceString(in, "decline_code"); retained != "" {
			code = retained
		}
		evidence := map[string]any{"declined": true, "decline_code": code}
		if err := store.RecordProgress(ctx, in.ID, evidence); err != nil {
			return intents.Ambiguous("cannot retain sale refusal: " + err.Error())
		}
		return h.complete(ctx, in, nil, intents.TerminalWithEvidence("sale declined", evidence))
	case subscriptions.StripeEngineSucceeded:
		receipt, found, err := intents.ReadStripeEngineReceipt(ctx, in, service, result.PaymentIntentID)
		if err != nil || !found {
			return intents.Ambiguous("Stripe sale has no qualified captured receipt")
		}
		receipt, err = store.RetainCollectedReceipt(ctx, in, receipt)
		if err != nil {
			return intents.Ambiguous("cannot retain sale receipt: " + err.Error())
		}
		return h.complete(ctx, in, &receipt, intents.Succeeded(nil))
	default:
		return intents.Ambiguous("Stripe sale is still processing")
	}
}

func (h *NMISaleIntentHandler) resolveStripeSale(ctx context.Context, in gen.OpenrailsRailIntent, reference string) (intents.Outcome, error) {
	service, err := h.stripeService(ctx, in)
	if err != nil {
		return intents.Outcome{}, err
	}
	receipt, found, err := intents.ReadStripeEngineReceipt(ctx, in, service, reference)
	if err != nil || !found {
		return intents.Outcome{}, intents.RejectResolution("exact sale receipt is unavailable or contradicts accepted terms")
	}
	receipt, err = intents.NewStore(h.database()).RetainCollectedReceipt(ctx, in, receipt)
	if err != nil {
		return intents.Outcome{}, err
	}
	return h.complete(ctx, in, &receipt, intents.Succeeded(nil)), nil
}

// authenticationAbandoned: a buyer who started an issuer challenge and never
// finished it releases the purchase after the engine's authentication window.
func (h *NMISaleIntentHandler) authenticationAbandoned(in gen.OpenrailsRailIntent) bool {
	if h.Sale == nil || h.Sale.PurchaseService == nil {
		return false
	}
	p, err := payments.DecodeNMISalePayload(in)
	if err != nil {
		return false
	}
	return h.Sale.PurchaseService.now().After(p.AcceptedAt.Add(subscriptions.EngineAuthenticationWindow))
}
