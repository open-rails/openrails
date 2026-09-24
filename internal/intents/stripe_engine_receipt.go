package intents

import (
	"context"
	"errors"
	"github.com/google/uuid"

	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/modules/payments/charge"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
)

type StripeEngineServiceResolver interface {
	ResolveStripeEngineService(context.Context, uuid.UUID, *uuid.UUID) (*subscriptions.StripeService, bool, error)
}

// StripeEngineParams derives all provider inputs from the accepted operation.
// A caller cannot substitute the current card, price or customer during recovery.
func StripeEngineParams(in gen.OpenrailsRailIntent) (subscriptions.StripeEnginePaymentParams, error) {
	var params subscriptions.StripeEnginePaymentParams
	if in.Rail != "stripe" || in.PspID == nil {
		return params, errors.New("operation is not Stripe engine payment")
	}
	params.MerchantID = in.MerchantID
	params.PSPID = *in.PspID
	params.OperationID = in.ID
	switch in.IntentType {
	case subscriptions.TypeInitialMembership:
		p, err := subscriptions.DecodeInitialMembershipPayload(in)
		if err != nil {
			return params, err
		}
		if p.Terms.CollectionPolicy != models.CollectionPolicyEngine {
			return params, errors.New("native membership cannot use engine payment")
		}
		minor, err := moneyutil.NativeToRailMinorExact(p.Terms.Currency, p.Terms.Amount)
		if err != nil {
			return params, err
		}
		params.CustomerID = p.Terms.CustomerID
		params.Instrument = p.Instrument
		params.AmountMinor = minor
		params.Currency = p.Terms.Currency
		params.Initial = true
	case payments.TypeNMISale:
		p, err := payments.DecodeNMISalePayload(in)
		if err != nil {
			return params, err
		}
		customer, err := uuid.Parse(p.UserID)
		if err != nil {
			return params, err
		}
		minor, err := moneyutil.NativeToRailMinorExact(p.Currency, p.Amount)
		if err != nil {
			return params, err
		}
		params.CustomerID = customer
		params.Instrument = p.Instrument
		params.AmountMinor = minor
		params.Currency = p.Currency
		params.OneTime = true
	case subscriptions.TypeSubscriptionCollection:
		p, err := subscriptions.DecodeSubscriptionCollectionPayload(in)
		if err != nil {
			return params, err
		}
		params.CustomerInitiated = p.Initiator == charge.InitiatorCustomer
		params.CustomerID = p.Renewal.CustomerID
		params.Instrument = p.Instrument
		params.AmountMinor = p.AmountMinor
		params.Currency = p.Renewal.Currency
	default:
		return params, errors.New("operation has no Stripe engine receipt contract")
	}
	return params, nil
}

// ReadStripeEngineReceipt qualifies provider readback; a submitted candidate or
// browser completion redirect can never create a payment/entitlement by itself.
func ReadStripeEngineReceipt(ctx context.Context, in gen.OpenrailsRailIntent, service *subscriptions.StripeService, reference string) (CollectedReceipt, bool, error) {
	binding, err := collectionBinding(in)
	if err != nil {
		return CollectedReceipt{}, false, err
	}
	params, err := StripeEngineParams(in)
	if err != nil {
		return CollectedReceipt{}, false, err
	}
	if service == nil {
		return CollectedReceipt{}, false, errors.New("Stripe engine receipt service unavailable")
	}
	result, found, err := service.ReadEnginePayment(ctx, params, reference)
	if err != nil || !found {
		return CollectedReceipt{}, found, err
	}
	if result.State != subscriptions.StripeEngineSucceeded || result.Receipt == nil {
		return CollectedReceipt{}, false, nil
	}
	receipt := CollectedReceipt{data: collectedReceipt{Version: 1, Family: "collected_payment", Binding: binding, StripeEngine: result.Receipt}}
	return receipt, true, receipt.Validate(in)
}

// StripeEnginePaymentIntentID returns the qualified original payment identity,
// which is also the initial recurring agreement anchor. It is not a charge ID.
func (r CollectedReceipt) StripeEnginePaymentIntentID() string {
	if r.data.StripeEngine != nil {
		return r.data.StripeEngine.PaymentIntentID
	}
	return ""
}

func (r CollectedReceipt) ReversalKind() string {
	if r.data.StripeEngine != nil {
		return r.data.StripeEngine.ReversalKind()
	}
	return ""
}
