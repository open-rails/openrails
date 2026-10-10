package checkout

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/paymentmethods"
	"github.com/open-rails/openrails/internal/shared/apperr"
)

// A checkout attempt's status means one thing: created may still run; failed
// is final for its key and is written only for a definite refusal, and only
// while no provider operation was admitted for the session. Ambiguous errors
// leave it created, so its key resumes it.

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
	case err == nil, errors.Is(err, ErrCheckoutAttemptPending), errors.Is(err, ErrCheckoutProcessing):
		return "", "", false
	case errors.As(err, &method):
		return failureKindPaymentMethod, method.LocalizationID, true
	case errors.Is(err, ErrPaymentMethodStale):
		return failureKindStale, "", true
	case errors.Is(err, ErrCheckoutAttemptValidation), errors.Is(err, ErrCheckoutAttemptConflict):
		return failureKindRefused, "", true
	case errors.As(err, &refusal) && refusal.Status >= 400 && refusal.Status < 500:
		return failureKindRefused, refusal.Code, true
	}
	return "", "", false
}

// failedSessionError answers a replay of a failed session with its refusal.
func failedSessionError(session *models.CheckoutAttempt) error {
	reason, _ := session.RailState["failure_reason"].(string)
	if reason == "" {
		reason = "checkout attempt failed"
	}
	code, _ := session.RailState["failure_code"].(string)
	switch session.RailState["failure_kind"] {
	case failureKindPaymentMethod:
		return &paymentmethods.PaymentMethodError{Err: errors.New(reason), LocalizationID: code, Message: reason, Rail: string(session.Rail)}
	case failureKindStale:
		return fmt.Errorf("%w: %s", ErrPaymentMethodStale, reason)
	}
	return fmt.Errorf("%w: previous attempt was refused: %s", ErrCheckoutAttemptConflict, reason)
}

// sessionIntentKeys are every provider operation a session can admit.
func sessionIntentKeys(id uuid.UUID) []string {
	native := "checkout_native_session:" + id.String()
	return []string{NMISaleIdempotencyKey(native), CustodianSaleIdempotencyKey(native), InitialMembershipIdempotencyKey("checkout_attempt:" + id.String())}
}

// markInitializationFailed fails a created session on a definite refusal,
// unless a provider operation was admitted for it: that operation's outcome
// is the session's.
func (s *CheckoutAttemptService) markInitializationFailed(ctx context.Context, session *models.CheckoutAttempt, failure error) error {
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
		if _, err := q.LockCheckoutAttemptForAdmission(ctx, gen.LockCheckoutAttemptForAdmissionParams{MerchantID: mid.UUID(), ID: session.ID}); err != nil {
			return err
		}
		_, err := q.FailCheckoutAttemptInitialization(ctx, gen.FailCheckoutAttemptInitializationParams{
			MerchantID: mid.UUID(), ID: session.ID, Now: s.now(), Reason: failure.Error(), Kind: kind, Code: code,
			IntentKeys: sessionIntentKeys(session.ID),
		})
		return err
	})
}

// admitForSession locks the session an operation is admitted for and refuses
// a terminal one, so a superseded request can never charge a session another
// request already settled. No session (a non-session checkout) admits.
func admitForSession(ctx context.Context, tx pgx.Tx, merchantID uuid.UUID, checkoutAttemptID string) error {
	if checkoutAttemptID == "" {
		return nil
	}
	id, err := billing.ParseCheckoutAttemptID(checkoutAttemptID)
	if err != nil {
		return fmt.Errorf("%w: invalid checkout attempt", ErrCheckoutAttemptValidation)
	}
	status, err := gen.New(tx).LockCheckoutAttemptForAdmission(ctx, gen.LockCheckoutAttemptForAdmissionParams{MerchantID: merchantID, ID: id.UUID()})
	if db.IsNotFound(err) {
		return ErrCheckoutAttemptNotFound
	}
	if err != nil {
		return err
	}
	switch models.CheckoutAttemptStatus(status) {
	case models.CheckoutAttemptStatusSucceeded, models.CheckoutAttemptStatusFailed, models.CheckoutAttemptStatusExpired, models.CheckoutAttemptStatusCanceled:
		return apperr.New(http.StatusConflict, "checkout_attempt_closed", "checkout attempt is "+status)
	}
	return nil
}

// saleRefusal is the refusal a definite decline of the session's sale
// answered with, so a replay answers the same way.
func (s *CheckoutAttemptService) saleRefusal(ctx context.Context, session *models.CheckoutAttempt) error {
	if s.db == nil || session.Mode != models.CheckoutAttemptModeOneOff {
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
