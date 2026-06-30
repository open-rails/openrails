package subscriptions

import (
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/models"
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
	// Terminal forces immediate cancellation (entitlements revoked, no further
	// retries) regardless of the retry count, without incrementing it — no charge
	// was attempted. Used when the dunning window has expired: a months-stale
	// rebill must be downgraded, never retried.
	Terminal bool
	// HardDecline, when true, forces immediate cancellation with no further retry
	// scheduling regardless of dunning mode. Set by dunning when the rail
	// returns a permanent decline (stolen card, do-not-honor, account closed,
	// expired card, pickup card). See ClassifyNMIDecline.
	HardDecline bool
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
