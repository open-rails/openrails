package checkout

import (
	"context"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/internal/cardguard"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/idempotency"
	"github.com/open-rails/openrails/pkg/merchant"
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
	id := idempotentCheckoutSessionID(mid.UUID(), scoped)
	// The claim and the session are read from one snapshot, so no reclaim can
	// land between them. While a request for the key runs its outcome is not
	// known, and a host must not move on to another attempt (#1099).
	var session *models.CheckoutSession
	err = s.db.ReadSnapshot(ctx, func(ctx context.Context, tx pgx.Tx) error {
		rec, err := idempotency.GetInTx(ctx, tx, mid.UUID(), checkoutSessionIdempotencyOp, scoped)
		if err != nil {
			return err
		}
		if rec != nil && rec.Status == idempotency.StatusProcessing && rec.Leased {
			return ErrCheckoutSessionPending
		}
		session, err = NewCheckoutSessionRepo(s.db.NewWithPgxTx(tx)).GetByID(ctx, id)
		return err
	})
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
