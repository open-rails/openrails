package checkout

import (
	"context"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/internal/cardguard"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/modules/idempotency"
	"github.com/open-rails/openrails/pkg/merchant"
	"strings"
)

// LookupSession finds only a buyer-bound idempotent session and validates the
// original selector/resource assertion; it never resolves a provider or writes.
func (s *CheckoutSessionService) LookupSession(ctx context.Context, req *CheckoutSessionCreateRequest, user *UserIdentity) (*CheckoutSessionResponse, error) {
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
	scoped := scopeIdempotencyKey(user.ID, req.IdempotencyKey)
	// While a request for the key is running, its outcome is not known: a host
	// must not move on to another attempt (#1099).
	if s.idempotencyService != nil {
		rec, err := s.idempotencyService.Get(ctx, checkoutSessionIdempotencyOp, scoped)
		if err != nil {
			return nil, err
		}
		if rec != nil && rec.Status == idempotency.StatusProcessing && rec.Leased {
			return nil, ErrCheckoutSessionPending
		}
	}
	id := idempotentCheckoutSessionID(mid.UUID(), scoped)
	session, err := s.repo.GetByID(ctx, id)
	if db.IsNotFound(err) {
		return nil, ErrCheckoutSessionNotFound
	}
	if err != nil {
		return nil, err
	}
	if session.CustomerID.String() != user.ID {
		return nil, ErrCheckoutSessionNotFound
	}
	canonicalizeCheckoutPaymentName(&req.Payment)
	stored, _ := session.RailState[checkoutSessionFingerprintKey].(string)
	fingerprint := checkoutSessionRequestFingerprintForRail(req, user, string(session.Rail))
	if stored == "" || stored != fingerprint {
		return nil, openrails.ErrIdempotencyKeyReused
	}
	if response, found, err := s.acceptedOperationSessionResponse(ctx, session); found || err != nil {
		return response, err
	}
	return s.sessionToResponse(session), nil
}

// GetSessionByKey is an ownership read; the secret-bearing creation request is
// neither needed nor accepted. The resource assertion is immutable admission data.
func (s *CheckoutSessionService) GetSessionByKey(ctx context.Context, key, entitlement string, user *UserIdentity) (*CheckoutSessionResponse, error) {
	if user == nil || strings.TrimSpace(user.ID) == "" || strings.TrimSpace(key) == "" || strings.TrimSpace(entitlement) == "" || len(key) > 255 || cardguard.ContainsPAN(key) {
		return nil, ErrCheckoutSessionValidation
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	id := idempotentCheckoutSessionID(mid.UUID(), scopeIdempotencyKey(user.ID, key))
	session, err := s.repo.GetByID(ctx, id)
	if db.IsNotFound(err) {
		return nil, ErrCheckoutSessionNotFound
	}
	if err != nil {
		return nil, err
	}
	resource, _ := session.RailState["requested_entitlement"].(string)
	fingerprint, _ := session.RailState[checkoutSessionFingerprintKey].(string)
	if session.CustomerID.String() != user.ID || resource != entitlement || fingerprint == "" {
		return nil, ErrCheckoutSessionNotFound
	}
	if response, found, err := s.acceptedOperationSessionResponse(ctx, session); found || err != nil {
		return response, err
	}
	return s.sessionToResponse(session), nil
}
