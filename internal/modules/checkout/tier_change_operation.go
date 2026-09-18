package checkout

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/intents"
)

// A durable tier change — an NMI upgrade (nmi_upgrade_intent.go) or a Stripe
// tier change (stripe_tier_change_intent.go) — answers the change-tier routes
// with one contract on every rail: the request's Idempotency-Key names the
// operation and replays its stored result; an unresolved provider outcome
// answers "processing" (HTTP 202) with operation_id; another key while one is
// unresolved is refused tier_change_in_flight naming it; a terminal operation
// is a coded tier_change_refused.

// tierChangeSubjectConstraint is the one-unresolved-tier-change-per-
// subscription unique index shared by every durable tier change type.
const tierChangeSubjectConstraint = "uq_rail_intents_tier_change_subscription"

// tierChangeOperationKeys map a client Idempotency-Key onto each durable tier
// change type's ledger key.
var tierChangeOperationKeys = []func(string) string{NMIUpgradeIdempotencyKey, StripeTierChangeIdempotencyKey}

// tierChangeSubject is what a replay must name again: the customer, the
// subscription the change was requested on and the target price.
type tierChangeSubject struct {
	UserID         string
	SubscriptionID uuid.UUID
	RequestedPrice string
	PriceID        uuid.UUID
}

func (p NMIUpgradePayload) subject() tierChangeSubject {
	return tierChangeSubject{UserID: p.UserID, SubscriptionID: p.OldSubscriptionID, RequestedPrice: p.RequestedPrice, PriceID: p.PriceID}
}

func (p StripeTierChangePayload) subject() tierChangeSubject {
	return tierChangeSubject{UserID: p.UserID, SubscriptionID: p.SubscriptionID, RequestedPrice: p.RequestedPrice, PriceID: p.PriceID}
}

func decodeTierChangeSubject(in gen.OpenrailsRailIntent) (tierChangeSubject, error) {
	switch in.IntentType {
	case TypeNMIUpgrade:
		var p NMIUpgradePayload
		if err := json.Unmarshal(in.Payload, &p); err != nil {
			return tierChangeSubject{}, err
		}
		return p.subject(), nil
	case TypeStripeTierChange:
		var p StripeTierChangePayload
		if err := json.Unmarshal(in.Payload, &p); err != nil {
			return tierChangeSubject{}, err
		}
		return p.subject(), nil
	}
	return tierChangeSubject{}, fmt.Errorf("intent %s (%s) is not a tier change", in.ID, in.IntentType)
}

// tierChangeOwnedBy accepts the canonical row an enqueue returned only when it
// is this request's operation. Two requests under one merchant-scoped key can
// both miss the replay lookup; the later enqueue then gets the earlier row
// through ON CONFLICT, and it must neither run nor answer for a different
// customer, subscription or target.
func tierChangeOwnedBy(row gen.OpenrailsRailIntent, want tierChangeSubject) error {
	got, err := decodeTierChangeSubject(row)
	if err != nil || got.UserID != want.UserID || got.SubscriptionID != want.SubscriptionID || got.PriceID != want.PriceID {
		return tierChangeIdempotencyConflict()
	}
	return nil
}

// ReplayTierChange answers a request whose Idempotency-Key names a durable
// tier change before any admission the completed change would fail (its
// subscription has moved and its price may be archived). found=false when the
// key is new. A tier change without a key is refused here, before any
// admission or mutation: the key is the client's only handle on a lost
// response.
func (s *CheckoutService) ReplayTierChange(ctx context.Context, req *TierChangeRequest, user *UserIdentity) (*TierChangeResponse, bool, error) {
	if strings.TrimSpace(req.IdempotencyKey) == "" {
		return nil, false, tierChangeKeyRequired()
	}
	if s.SubscriptionService == nil {
		return nil, false, nil
	}
	store := intents.NewStore(s.SubscriptionService.Database())
	for _, ledgerKey := range tierChangeOperationKeys {
		in, err := store.GetByIdempotencyKey(ctx, ledgerKey(req.IdempotencyKey))
		if db.IsNotFound(err) {
			continue
		}
		if err != nil {
			return nil, false, err
		}
		response, err := s.replayTierChangeOperation(ctx, in, req, user)
		return response, true, err
	}
	return nil, false, nil
}

// replayTierChangeOperation answers a request that names an existing
// operation: its stored result, or its live state after claiming committed
// work that is still runnable. The frozen payload is never refreshed.
func (s *CheckoutService) replayTierChangeOperation(ctx context.Context, in gen.OpenrailsRailIntent, req *TierChangeRequest, user *UserIdentity) (*TierChangeResponse, error) {
	subject, err := decodeTierChangeSubject(in)
	if err != nil {
		return nil, err
	}
	price := strings.TrimSpace(req.PriceID)
	if user == nil || subject.UserID != user.ID ||
		(req.SubscriptionID != uuid.Nil && req.SubscriptionID != subject.SubscriptionID) ||
		(price != subject.RequestedPrice && price != openrails.PriceID(subject.PriceID).String()) {
		return nil, tierChangeIdempotencyConflict()
	}
	if s.Intents != nil && (in.Status == intents.StatusPending || in.Status == intents.StatusFailedRetryable) {
		psp := derefUUID(in.PspID)
		ctx = db.WithPSPID(ctx, psp)
		if in, err = s.Intents.EnqueueAndExecute(ctx, intents.EnqueueParams{MerchantID: in.MerchantID, Provider: in.Rail, PspID: psp, SubscriptionID: in.SubscriptionID, PriceID: in.PriceID, IntentType: in.IntentType, Payload: json.RawMessage(in.Payload), IdempotencyKey: in.IdempotencyKey, NextAttemptAt: s.now(), Origin: intents.OriginUser, OriginReason: "resume tier change"}); err != nil {
			return nil, err
		}
	}
	return tierChangeResponse(in)
}

// tierChangeResponse renders an operation's durable state: the stored
// result, its coded refusal, or "processing" naming it while the provider
// outcome is unresolved.
func tierChangeResponse(in gen.OpenrailsRailIntent) (*TierChangeResponse, error) {
	switch in.IntentType {
	case TypeNMIUpgrade:
		return nmiUpgradeTierChangeResponse(in)
	case TypeStripeTierChange:
		return stripeTierChangeResponse(in)
	}
	return nil, fmt.Errorf("intent %s (%s) is not a tier change", in.ID, in.IntentType)
}

// tierChangeProcessing marks a rendered operation as accepted but unresolved
// (HTTP 202): the same Idempotency-Key reads the result once it settles.
func tierChangeProcessing(resp *TierChangeResponse) (*TierChangeResponse, error) {
	resp.Status = "processing"
	resp.Message = "Tier change is being confirmed with the provider; retry with the same Idempotency-Key to read the result"
	return resp, nil
}

// tierChangeRefused renders a terminal operation as tier_change_refused. A
// provider payment refusal (402) keeps its decline code, another provider
// refusal is a 400, and an operator-attested non-execution or a refusal
// before submission (providerStatus 0) is a 409: the change did not happen
// and a new request needs a new key.
func tierChangeRefused(in gen.OpenrailsRailIntent, providerStatus int, declineCode string) error {
	refusal := &TierChangeError{HTTPStatus: http.StatusConflict, Code: openrails.CodeTierChangeRefused, Message: "tier change was not executed"}
	if in.LastFailureReason != nil && *in.LastFailureReason != "" {
		refusal.Message = *in.LastFailureReason
	}
	switch {
	case providerStatus == 0:
	case providerStatus == http.StatusPaymentRequired:
		refusal.HTTPStatus = http.StatusPaymentRequired
		if declineCode != "" {
			refusal.Code = declineCode
		}
	default:
		refusal.HTTPStatus = http.StatusBadRequest
	}
	return refusal
}

// refuseTierChangeInFlight points a new request at the unresolved tier change
// that owns the subscription, before anything reads provider state it may be
// moving.
func (s *CheckoutService) refuseTierChangeInFlight(ctx context.Context, subscriptionID uuid.UUID) error {
	live, err := intents.NewStore(s.SubscriptionService.Database()).LiveTierChange(ctx, subscriptionID)
	switch {
	case err == nil:
		return &TierChangeInFlightError{OperationID: live.ID}
	case db.IsNotFound(err):
		return nil
	default:
		return err
	}
}

// tierChangeInFlight answers an enqueue that lost the subject index race: the
// conflict proves an operation owned the subscription, so the answer is a
// conflict even if it settled since.
func (s *CheckoutService) tierChangeInFlight(ctx context.Context, subscriptionID uuid.UUID) error {
	live, err := intents.NewStore(s.SubscriptionService.Database()).LiveTierChange(ctx, subscriptionID)
	if err != nil {
		return ErrTierChangePending
	}
	return &TierChangeInFlightError{OperationID: live.ID}
}

func derefUUID(id *uuid.UUID) uuid.UUID {
	if id == nil {
		return uuid.Nil
	}
	return *id
}
