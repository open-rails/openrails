package checkout

import (
	"context"
	"fmt"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/modules/payments"
)

// acceptedOperationSessionResponse reads existing financial authority, never
// current catalog state, provider state, or a token-bearing creation request.
func (s *CheckoutSessionService) acceptedOperationSessionResponse(ctx context.Context, session *models.CheckoutSession) (*CheckoutSessionResponse, bool, error) {
	if session.Rail != models.RailNMI || session.Mode != models.CheckoutSessionModeOneOff || s.db == nil {
		return s.initialMembershipSessionResponse(ctx, session)
	}
	operation, err := intents.NewStore(s.db).GetByIdempotencyKey(ctx, NMISaleIdempotencyKey("checkout_native_session:"+session.ID.String()))
	if db.IsNotFound(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, true, err
	}
	terms, err := payments.DecodeNMISalePayload(operation)
	if err != nil {
		return nil, true, err
	}
	if terms.CheckoutSessionID != session.ID || terms.UserID != session.CustomerID.String() || session.PriceID == nil || terms.PriceID != *session.PriceID || terms.Instrument.PSPID != session.PspID {
		return nil, true, fmt.Errorf("sale receipt contradicts checkout identity")
	}
	projection := *session
	projection.PaymentID, projection.SubscriptionID, projection.TransactionID = nil, nil, nil
	projection.ExpiresAt = nil
	switch operation.Status {
	case intents.StatusSucceeded:
		if err := intents.ValidateNMISaleTerminal(operation); err != nil {
			return nil, true, err
		}
		result, err := renderSaleOperation(operation)
		if err != nil {
			return nil, true, err
		}
		if err := s.applyCheckoutResponse(&projection, result); err != nil {
			return nil, true, err
		}
	case intents.StatusFailedTerminal:
		if err := intents.ValidateNMISaleTerminal(operation); err != nil {
			return nil, true, err
		}
		projection.Status = models.CheckoutSessionStatusFailed
	case intents.StatusExpired:
		projection.Status = models.CheckoutSessionStatusExpired
	case intents.StatusSuperseded:
		projection.Status = models.CheckoutSessionStatusCanceled
	case intents.StatusPending, intents.StatusInFlight, intents.StatusFailedRetryable, intents.StatusUnknownNeedsVerify:
		projection.Status = models.CheckoutSessionStatus("processing")
	default:
		return nil, true, fmt.Errorf("unrecognized sale operation status %q", operation.Status)
	}
	response := s.sessionToResponse(&projection)
	response.Operation = &openrails.PaymentOperation{ID: operation.ID, Status: operation.Status}
	response.NextAction = nil
	return response, true, nil
}
