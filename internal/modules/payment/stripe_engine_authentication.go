package payment

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/billingauth"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/shared/apperr"
)

// stripeAuthentication is the issuer challenge a Stripe engine payment waits
// on: its PaymentIntent and the client secret Stripe.js completes it with.
type stripeAuthentication struct {
	PaymentIntentID string
	ClientSecret    string
}

func (s *PurchaseService) ownedStripeEngineOperation(ctx context.Context, id uuid.UUID, principal billingauth.Payer) (gen.BillingProviderIntent, error) {
	var empty gen.BillingProviderIntent
	mid, err := merchant.Require(ctx)
	if err != nil {
		return empty, err
	}
	if billingauth.ValidatePayer(&principal) != nil || principal.CredentialClass != billingauth.CredentialClassUserSession || principal.Invoker != "" || principal.MerchantID != mid {
		return empty, apperr.New(403, "customer_session_required", "payment authentication requires an interactive customer session")
	}
	if s == nil || s.SubscriptionService == nil {
		return empty, errors.New("payment authentication unavailable")
	}
	in, err := intents.NewStore(s.SubscriptionService.Database()).Get(ctx, id)
	if err != nil {
		return empty, apperr.New(404, "payment_not_found", "payment not found")
	}
	params, err := intents.StripeEngineParams(in)
	if err != nil || in.MerchantID != mid.UUID() || params.CustomerID.String() != principal.SubjectID {
		return empty, apperr.New(404, "payment_not_found", "payment not found")
	}
	return in, nil
}

// stripeAuthentication reads an unresolved payment's client secret, for its
// interactive payer only; zero when it waits on nothing. Credential scopes and
// the immutable operation, not request fields, select the account and card.
// The secret is read from Stripe on every answer and never stored.
func (s *PurchaseService) stripeAuthentication(ctx context.Context, id uuid.UUID, principal billingauth.Payer, resolver intents.StripeEngineServiceResolver) (stripeAuthentication, error) {
	in, err := s.ownedStripeEngineOperation(ctx, id, principal)
	if err != nil {
		return stripeAuthentication{}, err
	}
	if in.Status == intents.StatusSucceeded || in.Status == intents.StatusFailedTerminal {
		return stripeAuthentication{}, nil
	}
	if resolver == nil {
		return stripeAuthentication{}, errors.New("Stripe payment resolver unavailable")
	}
	service, found, err := resolver.ResolveStripeEngineService(ctx, in.MerchantID, in.PspID)
	if err != nil {
		return stripeAuthentication{}, err
	}
	if !found || service == nil {
		return stripeAuthentication{}, errors.New("Stripe payment account unavailable")
	}
	params, err := intents.StripeEngineParams(in)
	if err != nil {
		return stripeAuthentication{}, err
	}
	reference := intents.EvidenceString(in, "stripe_payment_intent_id")
	if reference == "" {
		if candidate, found, err := intents.LoadCollectionCandidate(in); err != nil {
			return stripeAuthentication{}, err
		} else if found {
			reference = candidate.TransactionID
		}
	}
	if reference == "" {
		return stripeAuthentication{}, apperr.Conflictf("payment identity is still being recovered")
	}
	secret, err := service.EngineAuthenticationSecret(ctx, params, reference, params.CustomerID)
	if err != nil {
		return stripeAuthentication{}, apperr.Conflictf("payment does not require customer authentication")
	}
	return stripeAuthentication{PaymentIntentID: reference, ClientSecret: secret}, nil
}

// authenticateAction is the next action of a Stripe engine payment awaiting
// its payer's authentication; nil when none is outstanding.
func (s *PurchaseService) authenticateAction(ctx context.Context, in gen.BillingProviderIntent, principal billingauth.Payer, resolver intents.StripeEngineServiceResolver) (*billing.NextAction, error) {
	auth, err := s.stripeAuthentication(ctx, in.ID, principal, resolver)
	if err != nil || auth.ClientSecret == "" {
		return nil, err
	}
	psp := billing.PSPID(uuidOrNil(in.PspID))
	return &billing.NextAction{Type: "authenticate", PSPID: &psp, Transactions: []string{},
		Payload: map[string]string{"client_secret": auth.ClientSecret, "payment_intent_id": auth.PaymentIntentID}}, nil
}

// confirmStripeAuthentication ignores browser outcome assertions and drives
// the verifier against the original provider payment identity.
func (s *PurchaseService) confirmStripeAuthentication(ctx context.Context, id uuid.UUID, principal billingauth.Payer) (gen.BillingProviderIntent, error) {
	in, err := s.ownedStripeEngineOperation(ctx, id, principal)
	if err != nil {
		return gen.BillingProviderIntent{}, err
	}
	verifier, ok := s.Intents.(interface {
		VerifyByID(context.Context, uuid.UUID) (gen.BillingProviderIntent, error)
	})
	if !ok {
		return gen.BillingProviderIntent{}, errors.New("payment verifier unavailable")
	}
	return verifier.VerifyByID(ctx, in.ID)
}
