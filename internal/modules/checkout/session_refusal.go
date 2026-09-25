package checkout

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/modules/paymentmethods"
	"github.com/open-rails/openrails/internal/shared/apperr"
	"github.com/open-rails/openrails/pkg/merchant"
)

// A checkout session's status means one thing (#1099): created may still run;
// failed is final for its key and is written only for a definite refusal,
// and only while no provider operation was admitted for the session.
// Ambiguous errors leave it created, so its key resumes it.

const (
	failureKindPaymentMethod = "payment_method"
	failureKindStale         = "payment_method_stale"
	failureKindRefused       = "refused"
)

// definiteRefusal reports whether err proves the attempt cannot succeed as
// asked, and how to answer a replay of it.
func definiteRefusal(err error) (kind, code string, ok bool) {
	var method *paymentmethods.PaymentMethodError
	var refusal *apperr.Error
	switch {
	case err == nil, errors.Is(err, ErrCheckoutSessionPending), errors.Is(err, ErrCheckoutProcessing):
		return "", "", false
	case errors.As(err, &method):
		return failureKindPaymentMethod, method.LocalizationID, true
	case errors.Is(err, ErrPaymentMethodStale):
		return failureKindStale, "", true
	case errors.Is(err, ErrCheckoutSessionValidation), errors.Is(err, ErrCheckoutSessionConflict):
		return failureKindRefused, "", true
	case errors.As(err, &refusal) && refusal.Status >= 400 && refusal.Status < 500:
		return failureKindRefused, refusal.Code, true
	}
	return "", "", false
}

// failedSessionError answers a replay of a failed session with its refusal.
func failedSessionError(session *models.CheckoutSession) error {
	reason, _ := session.RailState["failure_reason"].(string)
	if reason == "" {
		reason = "checkout session failed"
	}
	code, _ := session.RailState["failure_code"].(string)
	switch session.RailState["failure_kind"] {
	case failureKindPaymentMethod:
		return &paymentmethods.PaymentMethodError{Err: errors.New(reason), LocalizationID: code, Message: reason, Rail: string(session.Rail)}
	case failureKindStale:
		return fmt.Errorf("%w: %s", ErrPaymentMethodStale, reason)
	}
	return fmt.Errorf("%w: previous attempt was refused: %s", ErrCheckoutSessionConflict, reason)
}

// sessionIntentKeys are every provider operation a session can admit.
func sessionIntentKeys(id uuid.UUID) []string {
	native := "checkout_native_session:" + id.String()
	return []string{NMISaleIdempotencyKey(native), CustodianSaleIdempotencyKey(native), InitialMembershipIdempotencyKey("checkout_session:" + id.String())}
}

// markInitializationFailed fails a created session on a definite refusal,
// unless a provider operation was admitted for it: that operation's outcome
// is the session's.
func (s *CheckoutSessionService) markInitializationFailed(ctx context.Context, session *models.CheckoutSession, failure error) error {
	kind, code, definite := definiteRefusal(failure)
	if !definite || s.db == nil {
		return nil
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	if _, accepted := session.RailState[acceptedPurchaseTermsKey]; accepted && session.Rail == models.RailStripe {
		_, err = s.db.Gen(ctx).FailHostedPurchaseInitialization(ctx, gen.FailHostedPurchaseInitializationParams{MerchantID: mid.UUID(), ID: session.ID, Reason: failure.Error(), Now: s.now()})
		return err
	}
	return s.db.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		q := gen.New(tx)
		if _, err := q.LockCheckoutSessionForAdmission(ctx, gen.LockCheckoutSessionForAdmissionParams{MerchantID: mid.UUID(), ID: session.ID}); err != nil {
			return err
		}
		_, err := q.FailCheckoutSessionInitialization(ctx, gen.FailCheckoutSessionInitializationParams{
			MerchantID: mid.UUID(), ID: session.ID, Now: s.now(), Reason: failure.Error(), Kind: kind, Code: code,
			IntentKeys: sessionIntentKeys(session.ID),
		})
		return err
	})
}

// admitForSession locks the session an operation is admitted for and refuses
// a terminal one, so a superseded request can never charge a session another
// request already settled. No session (a non-session checkout) admits.
func admitForSession(ctx context.Context, tx pgx.Tx, merchantID uuid.UUID, checkoutSessionID string) error {
	if checkoutSessionID == "" {
		return nil
	}
	id, err := openrails.ParseCheckoutSessionID(checkoutSessionID)
	if err != nil {
		return fmt.Errorf("%w: invalid checkout session", ErrCheckoutSessionValidation)
	}
	status, err := gen.New(tx).LockCheckoutSessionForAdmission(ctx, gen.LockCheckoutSessionForAdmissionParams{MerchantID: merchantID, ID: id.UUID()})
	if db.IsNotFound(err) {
		return ErrCheckoutSessionNotFound
	}
	if err != nil {
		return err
	}
	switch models.CheckoutSessionStatus(status) {
	case models.CheckoutSessionStatusSucceeded, models.CheckoutSessionStatusFailed, models.CheckoutSessionStatusExpired, models.CheckoutSessionStatusCanceled:
		return apperr.New(http.StatusConflict, "checkout_session_closed", "checkout session is "+status)
	}
	return nil
}

// saleRefusal is the refusal a definite decline of the session's sale
// answered with, so a replay answers the same way.
func (s *CheckoutSessionService) saleRefusal(ctx context.Context, session *models.CheckoutSession) error {
	if s.db == nil || session.Mode != models.CheckoutSessionModeOneOff {
		return nil
	}
	for _, key := range sessionIntentKeys(session.ID)[:2] {
		operation, err := intents.NewStore(s.db).GetByIdempotencyKey(ctx, key)
		if db.IsNotFound(err) {
			continue
		}
		if err != nil {
			return err
		}
		if operation.Status == intents.StatusFailedTerminal {
			return terminalCheckoutError(operation, "payment failed")
		}
		return nil
	}
	return nil
}
