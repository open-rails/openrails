package subscriptions

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/shared/normalize"
)

type CreateMembershipParams struct {
	UserID                  string
	PriceID                 uuid.UUID
	Processor               models.Processor
	ProcessorSubscriptionID *string
	UserEmail               *string
	CurrentPeriodEndsAt     *time.Time
	TransactionID           string
	Amount                  int64
	Currency                string
	PaymentMetadata         map[string]any
}

type RenewMembershipParams struct {
	Processor                 models.Processor
	ProcessorSubscriptionID   string
	CurrentPeriodEndsAt       *time.Time
	TransactionID             string
	Amount                    int64
	Currency                  string
	PaymentMetadata           map[string]any
	AllowTerminalReactivation bool
}

type ReactivateMembershipParams struct {
	Processor                 models.Processor
	ProcessorSubscriptionID   string
	CurrentPeriodEndsAt       *time.Time
	AllowTerminalReactivation bool
}

var ErrTerminalTransitionBlocked = errors.New("terminal-to-active transition blocked by lifecycle policy")

type TerminalTransitionBlockedError struct {
	SubscriptionID uuid.UUID
	Processor      models.Processor
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
	return fmt.Sprintf("%v: trigger=%s subscription_id=%s processor=%s from=%s to=%s cancel_type=%s reason=%s",
		ErrTerminalTransitionBlocked,
		e.Trigger,
		e.SubscriptionID,
		e.Processor,
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
	SubscriptionID          *uuid.UUID
	Processor               *models.Processor
	ProcessorSubscriptionID *string
	CancelType              models.CancelType
	CancelFeedback          *string
	RevokeAccess            bool
}

type FailMembershipParams struct {
	Processor      models.Processor
	SubscriptionID *uuid.UUID
	FailureReason  *string
	FailureCode    *string
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
	if subscription.CancelType != nil && *subscription.CancelType == models.CancelTypeChargeback {
		return "cancel_type=chargeback", true
	}
	feedback := normalize.FromPtr(subscription.CancelFeedback)
	if feedback != "" && strings.Contains(strings.ToUpper(feedback), "CHARGEBACK") {
		return "legacy_chargeback_feedback", true
	}
	return "", false
}
