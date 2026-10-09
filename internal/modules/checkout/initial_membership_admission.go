package checkout

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/shared/apperr"
)

func ownsInitialMembership(in gen.BillingProviderIntent, user string, price uuid.UUID, fingerprint string, sessionID *uuid.UUID) error {
	p, err := subscriptions.DecodeInitialMembershipPayload(in)
	if err != nil {
		return err
	}
	if (p.CheckoutAttemptID == nil) != (sessionID == nil) || (sessionID != nil && *p.CheckoutAttemptID != *sessionID) {
		return apperr.Conflictf("checkout key belongs to another session binding")
	}
	if p.Terms.CustomerID.String() != user || p.Terms.PriceID != price || p.RequestFingerprint != fingerprint {
		return apperr.Conflictf("checkout key belongs to another accepted enrollment")
	}
	return nil
}

func initialMembershipReplayParams(in gen.BillingProviderIntent) intents.EnqueueParams {
	return intents.EnqueueParams{MerchantID: in.MerchantID, Provider: in.Rail, PspID: *in.PspID, IntentType: in.IntentType, PriceID: in.PriceID, Payload: json.RawMessage(in.Payload), IdempotencyKey: in.IdempotencyKey, NextAttemptAt: in.NextAttemptAt, Origin: intents.Origin(in.Origin)}
}

func (s *CheckoutService) replayInitialMembership(ctx context.Context, req *CheckoutRequest, user *UserIdentity) (*CheckoutResponse, bool, error) {
	if req == nil || user == nil || req.IdempotencyKey == "" || s.SubscriptionService == nil {
		return nil, false, nil
	}
	prior, err := intents.NewStore(s.SubscriptionService.Database()).GetByIdempotencyKey(ctx, InitialMembershipIdempotencyKey(req.IdempotencyKey))
	if db.IsNotFound(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, true, err
	}
	p, err := subscriptions.DecodeInitialMembershipPayload(prior)
	if err != nil {
		return nil, true, err
	}
	target := railTarget{PSP: p.PSP, Rail: "nmi"}
	fingerprint := saleRequestFingerprint(req, user, p.Terms.PriceID, target)
	if err := ownsInitialMembership(prior, user.ID, p.Terms.PriceID, fingerprint, nil); err != nil {
		return nil, true, err
	}
	if s.Intents == nil {
		return nil, true, errors.New("enrollment executor unavailable")
	}
	current, err := s.Intents.EnqueueOwnedAndExecute(ctx, initialMembershipReplayParams(prior), func(in gen.BillingProviderIntent) error {
		return ownsInitialMembership(in, user.ID, p.Terms.PriceID, fingerprint, nil)
	})
	if err != nil {
		return nil, true, err
	}
	response, err := initialMembershipResponseFromIntent(current)
	return response, true, err
}
