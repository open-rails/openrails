package checkout

import (
	"context"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/idempotency"
)

// LookupSession finds only a buyer-bound idempotent session and validates the
// original selector/resource assertion; it never resolves a provider or writes.
func (s *CheckoutAttemptService) LookupSession(ctx context.Context, req *CheckoutAttemptCreateRequest, user *UserIdentity) (*CheckoutAttemptResponse, error) {
	if req == nil || user == nil || strings.TrimSpace(req.IdempotencyKey) == "" {
		return nil, ErrCheckoutAttemptValidation
	}
	// A lookup never uses a card; it only names the request being asked about.
	defer describeCard(req)()
	if err := validateCheckoutPriceSelector(req.PriceID, req.PriceKey); err != nil {
		return nil, err
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	scoped := scopeIdempotencyKey(user.ID, req.IdempotencyKey)
	id := idempotentCheckoutAttemptID(mid.UUID(), scoped)
	// The claim and the session are read from one snapshot, so no reclaim can
	// land between them. While a request for the key runs its outcome is not
	// known, and a host must not move on to another attempt (#1099).
	var session *models.CheckoutAttempt
	err = s.db.ReadSnapshot(ctx, func(ctx context.Context, tx pgx.Tx) error {
		rec, err := idempotency.GetInTx(ctx, tx, mid.UUID(), checkoutAttemptIdempotencyOp, scoped)
		if err != nil {
			return err
		}
		if rec != nil && rec.Status == idempotency.StatusProcessing && rec.Leased {
			return ErrCheckoutAttemptPending
		}
		session, err = NewCheckoutAttemptRepo(s.db.NewWithPgxTx(tx)).GetByID(ctx, id)
		return err
	})
	if db.IsNotFound(err) {
		return nil, ErrCheckoutAttemptNotFound
	}
	if err != nil {
		return nil, err
	}
	if session.CustomerID.String() != user.ID {
		return nil, ErrCheckoutAttemptNotFound
	}
	response, found, err := s.acceptedOperationSessionResponse(ctx, session)
	if err != nil {
		return nil, err
	}
	if !found {
		response = s.sessionToResponse(session)
	}
	canonicalizeCheckoutPaymentName(&req.Payment)
	stored, _ := session.RailState[checkoutAttemptFingerprintKey].(string)
	fingerprint := checkoutAttemptRequestFingerprintForRail(req, user, string(session.Rail))
	if stored == "" || stored != fingerprint {
		// #1104: a buyer retrying a finished attempt with other details (a new
		// card after a decline) must learn it finished, or its host never moves
		// on. The answer is the session's identity and terminal status only.
		if terminalCheckoutStatus(response.Status) {
			return &CheckoutAttemptResponse{Object: response.Object, ID: response.ID, Status: response.Status}, nil
		}
		return nil, billing.ErrIdempotencyKeyReused
	}
	return response, nil
}

func terminalCheckoutStatus(status string) bool {
	switch models.CheckoutAttemptStatus(status) {
	case models.CheckoutAttemptStatusSucceeded, models.CheckoutAttemptStatusFailed, models.CheckoutAttemptStatusExpired, models.CheckoutAttemptStatusCanceled:
		return true
	}
	return false
}
