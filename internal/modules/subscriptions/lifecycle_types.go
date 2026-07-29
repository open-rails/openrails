package subscriptions

import (
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/collection"
)

type CreateMembershipParams struct {
	UserID                string
	PriceID               uuid.UUID
	Rail                  models.Rail
	RailSubscriptionID    *string
	UserEmail             *string
	CurrentPeriodStartsAt *time.Time
	CurrentPeriodEndsAt   *time.Time
	TransactionID         string
	Amount                int64
	AmountProvided        bool
	Currency              string
	// PurchasedAt is the provider's transaction time (#651). nil => the payment
	// row records now() (provider supplied no timestamp).
	PurchasedAt     *time.Time
	PaymentMetadata map[string]any
}

type RenewMembershipParams struct {
	Rail                  models.Rail
	RailSubscriptionID    string
	CurrentPeriodStartsAt *time.Time
	CurrentPeriodEndsAt   *time.Time
	TransactionID         string
	Amount                int64
	AmountProvided        bool
	Currency              string
	// PurchasedAt is the provider's transaction time (#651). nil => the payment
	// row records now() (provider supplied no timestamp).
	PurchasedAt               *time.Time
	PaymentMetadata           map[string]any
	AllowTerminalReactivation bool
}

type ReactivateMembershipParams struct {
	Rail                      models.Rail
	RailSubscriptionID        string
	CurrentPeriodEndsAt       *time.Time
	AllowTerminalReactivation bool
}

var ErrTerminalTransitionBlocked = errors.New("terminal-to-active transition blocked by lifecycle policy")

type TerminalTransitionBlockedError struct {
	SubscriptionID uuid.UUID
	Rail           models.Rail
	FromStatus     models.SubscriptionStatus
	ToStatus       models.SubscriptionStatus
	CancelType     string
	Trigger        string
	Reason         string
}

func (e *TerminalTransitionBlockedError) Error() string {
	if e == nil {
		return ErrTerminalTransitionBlocked.Error()
	}
	return fmt.Sprintf("%v: trigger=%s subscription_id=%s rail=%s from=%s to=%s cancel_type=%s reason=%s",
		ErrTerminalTransitionBlocked,
		e.Trigger,
		e.SubscriptionID,
		e.Rail,
		e.FromStatus,
		e.ToStatus,
		e.CancelType,
		e.Reason,
	)
}

func (e *TerminalTransitionBlockedError) Unwrap() error {
	return ErrTerminalTransitionBlocked
}

func IsTerminalTransitionBlocked(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrTerminalTransitionBlocked) {
		return true
	}
	var blockedErr *TerminalTransitionBlockedError
	return errors.As(err, &blockedErr)
}

type CancelMembershipParams struct {
	SubscriptionID     *uuid.UUID
	Rail               *models.Rail
	RailSubscriptionID *string
	CancelType         models.CancelType
	CancelFeedback     *string
	RevokeAccess       bool
}

type FailMembershipParams struct {
	Rail           models.Rail
	SubscriptionID *uuid.UUID
	FailureReason  *string
	FailureCode    *string
	// Terminal requests immediate cancellation (entitlements revoked, no further
	// retries) regardless of the retry count, without incrementing it. It is a
	// REQUEST, not a command: TerminalCertainty must name the evidence, or the
	// row parks as `unknown` instead (#839).
	Terminal bool
	// Decline (or#870) is the three-way meaning of the rail's decline code, from
	// the ONE classifier collection.ClassifyDecline. The zero value
	// (DeclineRetry) is the safe default for every caller that has no code:
	//
	//   DeclineRetry            keep the dunning schedule
	//   DeclineFixPaymentMethod stop charging, KEEP the subscription and its
	//                           entitlements, notify the customer to update the
	//                           card. Never cancels, never revokes.
	//   DeclineNonRecoverable   terminal: cancel the schedule at the rail. Still
	//                           gated on TerminalCertainty + TerminalBlocked.
	//
	// No value of this field ever deletes a stored payment method.
	Decline collection.DeclineOutcome
	// TerminalCertainty (#821/#839/#840) names the evidence leg that justifies a
	// terminal outcome — one of the collection.Certainty* constants. A terminal
	// cancel revokes entitlements AND queues the irreversible cancellation of the
	// recurring SCHEDULE at the rail — never the customer's stored payment
	// method (or#870: nothing on this path can delete one) — so it requires
	// certainty: provider truth, a non-retryable decline,
	// or genuinely exhausted dunning ATTEMPTS. A date comparison, a lapsed
	// window, and the absence of one of our own rows are NOT evidence. Empty ⇒
	// FailMembership refuses to terminate and parks the row as `unknown`, access
	// intact, for the provider-verification plane to resolve.
	TerminalCertainty string
	// AttemptRecorded marks that a REAL charge attempt underlies this failure and
	// was recorded as a payments row by the CALLER (RecordFailedAttempt records
	// it here instead; either satisfies this). It is what lets schedule
	// exhaustion count as a certainty leg (#840): attempts we never made — because
	// our own data was missing — can never exhaust anything.
	AttemptRecorded bool
	// TerminalBlocked (#836), when non-empty, is the operator kill-switch reason
	// that forbids destructive outcomes for this merchant. It overrides any
	// certainty: the row parks instead of cancelling, and no provider delete is
	// queued. Callers set it from destructive.Gate.Check.
	TerminalBlocked string
	// RecordFailedAttempt writes a status='failed' payments row for this decline
	// in the same tx (#733: attempt_kind=renewal, failure_code verbatim +
	// normalized failure_reason). Callers set it when a real charge attempt was
	// declined and no failed row was recorded elsewhere.
	RecordFailedAttempt bool
}

func NormalizeCancelType(cancelType *models.CancelType) string {
	if cancelType == nil {
		return ""
	}
	return string(*cancelType)
}

func TerminalCancelReason(subscription *models.Subscription) (string, bool) {
	if subscription == nil {
		return "", false
	}
	if subscription.Status != models.StatusCancelled {
		return "", false
	}
	if subscription.CancelType != nil {
		switch *subscription.CancelType {
		case models.CancelTypeChargeback, models.CancelTypeUser, models.CancelTypeMerchant:
			return fmt.Sprintf("cancel_type=%s", *subscription.CancelType), true
		}
	}
	return "", false
}
