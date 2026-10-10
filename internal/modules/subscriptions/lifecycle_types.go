package subscriptions

import (
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/decline"
)

type CreateMembershipParams struct {
	// InitialPaymentReversal records a captured initial engine payment whose
	// refund/dispute already exists at first readback. It creates accounting and
	// a canceled agreement without granting access or announcing activation.
	InitialPaymentReversal string

	// PaymentCustodian is the frozen credential custody of the accepted payment.
	PaymentCustodian string
	// Prepared is supplied by a qualified durable initial-enrollment operation.
	Prepared              *InitialMembershipTerms
	UserID                string
	PriceID               uuid.UUID
	Rail                  models.Rail
	RailSubscriptionID    *string
	CurrentPeriodStartsAt *time.Time
	CurrentPeriodEndsAt   *time.Time
	TransactionID         string
	Amount                int64
	AmountProvided        bool
	Currency              string
	// PurchasedAt is the provider's transaction time; nil => the payment row
	// records now() (no provider timestamp).
	PurchasedAt     *time.Time
	PaymentMetadata map[string]any
}

type RenewMembershipParams struct {
	// PreviousPeriodEnd is the accepted engine obligation boundary. A recovery
	// purchase may start later; native renewals leave this nil and retain their
	// existing PeriodStart fence.
	PreviousPeriodEnd *time.Time
	// PaymentCustodian stamps the credential form actually used by an accepted
	// engine charge; empty preserves the native provider default.
	PaymentCustodian string
	// Prepared is supplied only by an accepted durable recurring-charge operation.
	// Observed provider renewals use the ordinary catalog-convergence path.
	Prepared              *RenewalTerms
	Rail                  models.Rail
	RailSubscriptionID    string
	CurrentPeriodStartsAt *time.Time
	CurrentPeriodEndsAt   *time.Time
	TransactionID         string
	Amount                int64
	AmountProvided        bool
	Currency              string
	// PurchasedAt is the provider's transaction time; nil => the payment row
	// records now() (no provider timestamp).
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

type ResumeMembershipParams struct {
	SubscriptionID uuid.UUID
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
	// RefuseOwnedRenewal refuses a customer or merchant cancel of an engine
	// membership while an accepted renewal payment is unresolved.
	RefuseOwnedRenewal bool
}

type FailMembershipParams struct {
	// Prepared preserves the accepted engine charge cadence for retry policy.
	// Native provider failures leave this nil.
	Prepared       *RenewalTerms
	Rail           models.Rail
	SubscriptionID *uuid.UUID
	FailureReason  *string
	FailureCode    *string
	// DeclinedAt is when the decline happened; zero means now. The retry
	// schedule runs from it, so a decline seen late keeps the same retry days.
	DeclinedAt time.Time
	// Terminal requests immediate cancellation (entitlements revoked, no
	// further retries) without incrementing the retry count. It is a request:
	// without TerminalCertainty the row parks as `unknown` instead.
	Terminal bool
	// Decline is the dunning action of the rail's decline code
	// (decline.Classify). The zero value retries, the safe default without a
	// code. FixPaymentMethod stops charging but keeps the subscription and its
	// entitlements; NonRecoverable is terminal, still gated on
	// TerminalCertainty and TerminalBlocked. No value deletes a stored payment
	// method.
	Decline decline.Action
	// TerminalCertainty names the evidence for a terminal outcome (a
	// collection.Certainty* constant). A terminal cancel revokes entitlements
	// and queues the irreversible cancel of the rail schedule (never the stored
	// payment method), so it needs provider truth, a non-retryable decline or
	// exhausted real attempts. Empty ⇒ FailMembership parks the row as
	// `unknown`, access intact, for provider verification.
	TerminalCertainty string
	// AttemptRecorded marks a real, recorded charge attempt (payment_attempts)
	// under this failure; only such attempts can exhaust the schedule.
	AttemptRecorded bool
	// TerminalBlocked, when non-empty, is the operator kill-switch reason
	// (destructive.Gate.Check). It overrides any certainty: the row parks and no
	// provider delete is queued.
	TerminalBlocked string
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
	if subscription.Status != models.StatusCanceled {
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
