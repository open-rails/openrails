package checkout

import (
	"context"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/pkg/merchant"
	"strings"
)

type CheckoutSessionLookupRequest struct{ PriceID, PriceKey, Entitlement, IdempotencyKey string }

// LookupSession finds only a buyer-bound idempotent session and validates the
// original selector/resource assertion; it never resolves a provider or writes.
func (s *CheckoutSessionService) LookupSession(ctx context.Context, req *CheckoutSessionLookupRequest, user *UserIdentity) (*CheckoutSessionResponse, error) {
	if req == nil || user == nil || strings.TrimSpace(req.IdempotencyKey) == "" {
		return nil, ErrCheckoutSessionValidation
	}
	if err := validateCheckoutPriceSelector(req.PriceID, req.PriceKey); err != nil {
		return nil, err
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	id := idempotentCheckoutSessionID(mid.UUID(), scopeIdempotencyKey(user.ID, req.IdempotencyKey))
	session, err := s.repo.GetByID(ctx, id)
	if db.IsNotFound(err) {
		return nil, openrails.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if session.CustomerID.String() != user.ID {
		return nil, openrails.ErrNotFound
	}
	request := &CheckoutSessionCreateRequest{PriceID: req.PriceID, PriceKey: req.PriceKey, Entitlement: req.Entitlement, IdempotencyKey: req.IdempotencyKey}
	stored, _ := session.RailState[checkoutSessionFingerprintKey].(string)
	fingerprint := checkoutSessionRequestFingerprintForRail(request, user, string(session.Rail))
	if stored == "" || stored != fingerprint {
		return nil, openrails.ErrIdempotencyKeyReused
	}
	if req.Entitlement != "" {
		if value, _ := session.RailState[acceptedPurchaseTermsKey].(map[string]any); value != nil {
			_ = value
		}
	}
	return s.sessionToResponse(session), nil
}
