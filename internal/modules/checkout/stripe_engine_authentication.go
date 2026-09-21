package checkout

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/shared/apperr"
	"github.com/open-rails/openrails/pkg/billingauth"
	"github.com/open-rails/openrails/pkg/merchant"
)

type StripeEngineAuthentication struct {
	Operation       openrails.PaymentOperation `json:"operation"`
	PaymentIntentID string                     `json:"payment_intent_id,omitempty"`
	ClientSecret    string                     `json:"client_secret,omitempty"`
}

func (s *CheckoutService) ownedStripeEngineOperation(ctx context.Context, id uuid.UUID, principal billingauth.DelegatedPrincipal) (gen.OpenrailsRailIntent, error) {
	var empty gen.OpenrailsRailIntent
	mid, err := merchant.Require(ctx)
	if err != nil {
		return empty, err
	}
	if principal.Validate() != nil || principal.CredentialClass != billingauth.CredentialClassUserSession || principal.Invoker != "" || principal.MerchantID != mid.String() {
		return empty, apperr.New(403, "customer_session_required", "payment authentication requires an interactive customer session")
	}
	if s == nil || s.SubscriptionService == nil {
		return empty, errors.New("payment authentication unavailable")
	}
	in, err := intents.NewStore(s.SubscriptionService.Database()).Get(ctx, id)
	if err != nil {
		return empty, apperr.New(404, "payment_not_found", "payment operation not found")
	}
	params, err := intents.StripeEngineParams(in)
	if err != nil || in.MerchantID != mid.UUID() || params.CustomerID.String() != principal.SubjectID {
		return empty, apperr.New(404, "payment_not_found", "payment operation not found")
	}
	return in, nil
}

// StripePaymentAuthentication exposes an original PI only to its interactive
// payer. Credential scopes and the immutable operation, not request body fields,
// select the account and card. The HTTP adapter must send Cache-Control:no-store.
func (s *CheckoutService) StripePaymentAuthentication(ctx context.Context, id uuid.UUID, principal billingauth.DelegatedPrincipal, resolver intents.StripeEngineServiceResolver) (StripeEngineAuthentication, error) {
	in, err := s.ownedStripeEngineOperation(ctx, id, principal)
	if err != nil {
		return StripeEngineAuthentication{}, err
	}
	out := StripeEngineAuthentication{Operation: openrails.PaymentOperation{ID: in.ID, Status: in.Status}}
	if in.Status == intents.StatusSucceeded || in.Status == intents.StatusFailedTerminal {
		return out, nil
	}
	if resolver == nil {
		return out, errors.New("Stripe payment resolver unavailable")
	}
	service, found, err := resolver.ResolveStripeEngineService(ctx, in.MerchantID, in.PspID)
	if err != nil {
		return out, err
	}
	if !found || service == nil {
		return out, errors.New("Stripe payment account unavailable")
	}
	params, err := intents.StripeEngineParams(in)
	if err != nil {
		return out, err
	}
	reference := intents.EvidenceString(in, "stripe_payment_intent_id")
	if reference == "" {
		if candidate, found, err := intents.LoadCollectionCandidate(in); err != nil {
			return out, err
		} else if found {
			reference = candidate.TransactionID
		}
	}
	if reference == "" {
		return out, apperr.Conflictf("payment identity is still being recovered")
	}
	secret, err := service.EngineAuthenticationSecret(ctx, params, reference, params.CustomerID)
	if err != nil {
		return out, apperr.Conflictf("payment does not require customer authentication")
	}
	out.PaymentIntentID = reference
	out.ClientSecret = secret
	return out, nil
}

// ConfirmStripePaymentAuthentication ignores browser outcome assertions and
// drives the existing verifier against the original provider payment identity.
func (s *CheckoutService) ConfirmStripePaymentAuthentication(ctx context.Context, id uuid.UUID, principal billingauth.DelegatedPrincipal) (openrails.PaymentOperation, error) {
	in, err := s.ownedStripeEngineOperation(ctx, id, principal)
	if err != nil {
		return openrails.PaymentOperation{}, err
	}
	verifier, ok := s.Intents.(interface {
		VerifyByID(context.Context, uuid.UUID) (gen.OpenrailsRailIntent, error)
	})
	if !ok {
		return openrails.PaymentOperation{}, errors.New("payment verifier unavailable")
	}
	current, err := verifier.VerifyByID(ctx, in.ID)
	if err != nil {
		return openrails.PaymentOperation{}, err
	}
	return openrails.PaymentOperation{ID: current.ID, Status: current.Status}, nil
}
