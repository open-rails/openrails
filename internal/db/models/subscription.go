package models

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/uptrace/bun"
)

type SubscriptionStatus string

const (
	// The status system is designed around a simple question: "Will we attempt to rebill this subscription?"
	// - If rebilling will be attempted → past_due (when payment fails but we're still trying)
	// - If rebilling will NEVER be attempted again → cancelled (user cancelled, max retries reached, etc.)

	StatusPending   SubscriptionStatus = "pending"   // Subscription created, waiting for initial payment confirmation
	StatusActive    SubscriptionStatus = "active"    // Normal good-standing, successful payments, rebill scheduled
	StatusPastDue   SubscriptionStatus = "past_due"  // Payment failed but we're still attempting rebills (will retry)
	StatusCancelled SubscriptionStatus = "cancelled" // Will never rebill again (user cancelled, max retries, admin cancelled, expired)
)

// CancelType represents who/what caused the cancellation
type CancelType string

const (
	CancelTypeUser       CancelType = "user"       // User manually cancelled
	CancelTypeMerchant   CancelType = "merchant"   // We manually cancelled for them
	CancelTypeExpired    CancelType = "expired"    // User failed to rebill
	CancelTypeChargeback CancelType = "chargeback" // Cancelled due to chargeback
)

type Subscription struct {
	bun.BaseModel `bun:"table:billing.subscriptions,alias:sub"`

	ID uuid.UUID `bun:"id,pk,type:uuid" json:"id"`
	// TenantSubjectID is the OpenRails payable tenant subject for this row (#317).
	// Additive during the hard-cut rollout; writers populate it and readers move to
	// it before user_id is dropped. Join billing.tenant_subjects for issuer/subject.
	TenantSubjectID uuid.UUID `bun:"tenant_subject_id,type:uuid,nullzero" json:"tenant_subject_id,omitempty"`
	ProductID       uuid.UUID `bun:"product_id,type:uuid,notnull" json:"product_id"` // Denormalized for efficient product-based lookups
	PriceID         uuid.UUID `bun:"price_id,type:uuid,notnull" json:"price_id"`     // Required for all subscriptions

	// Scheduled tier change (for downgrades that take effect at end of period)
	ScheduledPriceID *uuid.UUID `bun:"scheduled_price_id,type:uuid,nullzero" json:"scheduled_price_id,omitempty"`

	EntitlementsSpecSnapshot map[string]*int `bun:"entitlements_spec_snapshot,type:jsonb,nullzero" json:"entitlements_spec_snapshot,omitempty"`
	CreditsSpecSnapshot      CreditsSpec     `bun:"credits_spec_snapshot,type:jsonb,nullzero" json:"credits_spec_snapshot,omitempty"`

	Status                SubscriptionStatus `bun:"status,notnull,default:'pending'" json:"status"`
	StartedAt             time.Time          `bun:"started_at,notnull" json:"started_at"`
	EndedAt               *time.Time         `bun:"ended_at,nullzero" json:"ended_at"`
	CurrentPeriodStartsAt *time.Time         `bun:"current_period_starts_at,nullzero" json:"current_period_starts_at"`
	CurrentPeriodEndsAt   *time.Time         `bun:"current_period_ends_at,nullzero" json:"current_period_ends_at"`

	// Payment processor information
	Processor               Processor  `bun:"processor,notnull" json:"processor"`                                 // Processor: mobius, ccbill, solana
	ProcessorSubscriptionID string     `bun:"processor_subscription_id,notnull" json:"processor_subscription_id"` // Subscription ID from processor
	UserEmail               *string    `bun:"user_email,nullzero" json:"user_email,omitempty"`
	PaymentMethodID         *uuid.UUID `bun:"payment_method_id,type:uuid,nullzero" json:"payment_method_id"` // Reference to stored payment method

	// Manual rebill attempt fields for NMI
	LastRetryAt   *time.Time `bun:"last_retry_at,nullzero" json:"last_retry_at"`   // Date of last rebill attempt
	RetryAttempts *int       `bun:"retry_attempts,nullzero" json:"retry_attempts"` // Number of retry attempts (nullable for new subscriptions)
	NextRetryAt   *time.Time `bun:"next_retry_at,nullzero" json:"next_retry_at"`   // When to try next rebill
	GraceEndsAt   *time.Time `bun:"grace_ends_at,nullzero" json:"grace_ends_at"`   // Optional grace window end during dunning (processor-specific)

	// Cancellation information
	CancelFeedback *string     `bun:"cancel_feedback,nullzero" json:"cancel_feedback"` // User's cancellation message
	CancelType     *CancelType `bun:"cancel_type,nullzero" json:"cancel_type"`         // Who/what caused cancellation
	CancelledAt    *time.Time  `bun:"cancelled_at,nullzero" json:"cancelled_at"`

	// DeletionScheduledAt is set for NMI-backed cancellations that defer the
	// processor-side delete_subscription until shortly before the paid period
	// ends (issue 216). While non-nil, the cancellation is still reversible (the
	// processor subscription is alive). The River finalizer clears it to nil
	// after calling DeleteRecurringSubscription.
	DeletionScheduledAt *time.Time `bun:"deletion_scheduled_at,nullzero" json:"deletion_scheduled_at,omitempty"`

	// Relationships
	Price         *Price         `bun:"rel:belongs-to,join:price_id=id" json:"price,omitempty"`
	PaymentMethod *PaymentMethod `bun:"rel:belongs-to,join:payment_method_id=id" json:"payment_method,omitempty"`

	// Product relation for description
	Product *Product `bun:"rel:belongs-to,join:product_id=id" json:"product,omitempty"`

	Metadata json.RawMessage `bun:"gateway_response,type:jsonb,nullzero" json:"gateway_response,omitempty"` // Renamed from GatewayResponse - stores arbitrary subscription metadata

	CreatedAt time.Time `bun:"created_at,notnull,default:current_timestamp" json:"created_at"`
	UpdatedAt time.Time `bun:"updated_at,notnull,default:current_timestamp" json:"updated_at"`
}

func (s *Subscription) updateCurrentPeriods(billingCycle *time.Duration) {
	var periodStartsAt, periodEndsAt time.Time

	if s.CurrentPeriodEndsAt != nil && !s.CurrentPeriodEndsAt.IsZero() {
		periodStartsAt = *s.CurrentPeriodEndsAt
		if billingCycle != nil {
			periodEndsAt = periodStartsAt.Add(*billingCycle)
		} else {
			periodEndsAt = periodStartsAt.Add(30 * 24 * time.Hour)
		}
	} else {
		periodStartsAt = time.Now()
		if billingCycle != nil {
			periodEndsAt = periodStartsAt.Add(*billingCycle)
		} else {
			periodEndsAt = periodStartsAt.Add(30 * 24 * time.Hour)
		}
	}

	s.CurrentPeriodStartsAt = &periodStartsAt
	s.CurrentPeriodEndsAt = &periodEndsAt
}

func (s *Subscription) ActivateWithPrice(price *Price) error {
	if price.BillingCycleDays == nil {
		return fmt.Errorf("recurring price billing cycle is required")
	}

	billingCycle := time.Duration(*price.BillingCycleDays) * 24 * time.Hour
	s.updateCurrentPeriods(&billingCycle)

	s.EndedAt = nil
	s.CancelType = nil
	s.CancelledAt = nil
	s.PriceID = price.ID
	s.ProductID = price.ProductID // Update product when price changes (upgrade/downgrade)
	s.CancelFeedback = nil
	s.Status = StatusActive

	return nil
}

func (s *Subscription) ResetCurrentPeriods() error {
	now := time.Now()
	if s.CurrentPeriodEndsAt == nil || s.CurrentPeriodEndsAt.IsZero() {
		return fmt.Errorf("invalid subscription period end date")
	}

	if s.CurrentPeriodEndsAt.Equal(now) || s.CurrentPeriodEndsAt.Before(now) {
		emptyTime := time.Time{}
		s.CurrentPeriodStartsAt = &emptyTime
		s.CurrentPeriodEndsAt = &emptyTime
		// The period has already expired, so the subscription ends now.
		// The chk_ended_not_before_cancelled constraint requires
		// ended_at >= cancelled_at; using the true wall-clock instant
		// (rather than fudging it backwards) keeps that ordering correct
		// by construction: Cancel() captures its cancelled_at timestamp
		// before invoking this method, so cancelled_at <= ended_at always.
		s.EndedAt = &now
	}

	return nil
}

func (s *Subscription) ClearRetrySchedule() {
	s.LastRetryAt = nil
	s.RetryAttempts = nil
	s.NextRetryAt = nil
	s.GraceEndsAt = nil
}

func (s *Subscription) Cancel(reason string, cancelType *CancelType) error {
	// Capture cancelled_at BEFORE ResetCurrentPeriods sets ended_at. Reset
	// reads its own (>= now) wall-clock instant for ended_at, so this ordering
	// guarantees cancelled_at <= ended_at, satisfying the
	// chk_ended_not_before_cancelled constraint (ended_at >= cancelled_at)
	// without any artificial time fudging.
	now := time.Now()
	if err := s.ResetCurrentPeriods(); err != nil {
		return err
	}

	s.CancelledAt = &now
	s.CancelType = cancelType
	s.ClearRetrySchedule()
	if reason != "" {
		s.CancelFeedback = &reason
	}

	s.Status = StatusCancelled
	return nil
}

func (s *Subscription) Validate(amount float64) error {
	if s.CurrentPeriodEndsAt != nil && s.CurrentPeriodEndsAt.Before(time.Now()) {
		if s.Status == StatusActive {
			return fmt.Errorf("cannot activate expired subscription without proper renewal")
		}
	}

	if s.Status == StatusActive && amount <= 0 {
		return fmt.Errorf("cannot activate subscription with invalid amount: %.2f", amount)
	}

	if s.Status == StatusPastDue {
		if s.RetryAttempts != nil && *s.RetryAttempts >= 5 {
			return fmt.Errorf("subscription has exceeded maximum dunning attempts, should be cancelled")
		}
	}

	return nil
}
