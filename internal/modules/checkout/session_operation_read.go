package checkout

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/modules/payments"
)

// acceptedOperationSessionResponse reads existing financial authority, never
// current catalog state, provider state, or a token-bearing creation request.
func (s *CheckoutSessionService) acceptedOperationSessionResponse(ctx context.Context, session *models.CheckoutSession) (*CheckoutSessionResponse, bool, error) {
	if (session.Rail != models.RailNMI && session.Rail != models.RailStripe) || session.Mode != models.CheckoutSessionModeOneOff || s.db == nil {
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
		if authenticationRequired(operation) {
			projection.Status = models.CheckoutSessionStatusRequiresAction
		}
	default:
		return nil, true, fmt.Errorf("unrecognized sale operation status %q", operation.Status)
	}
	response := s.sessionToResponse(&projection)
	response.Operation = &openrails.PaymentOperation{ID: operation.ID, Status: operation.Status}
	response.NextAction = nil
	if operation.Status == intents.StatusFailedTerminal {
		response.Failure = operationFailure(operation)
	}
	return response, true, nil
}

// authenticationRequired reports a card payment waiting on the customer's
// provider challenge (3-D Secure); the browser completes it in the page.
func authenticationRequired(operation gen.OpenrailsRailIntent) bool {
	var evidence struct {
		AuthenticationRequired bool `json:"authentication_required"`
	}
	return json.Unmarshal(operation.ResultEvidence, &evidence) == nil && evidence.AuthenticationRequired
}
