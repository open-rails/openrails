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

	ID        uuid.UUID `bun:"id,pk,type:uuid" json:"id"`
	UserID    string    `bun:"user_id,notnull" json:"user_id"`
	ProductID uuid.UUID `bun:"product_id,type:uuid,notnull" json:"product_id"` // Denormalized for efficient product-based lookups
	PriceID   uuid.UUID `bun:"price_id,type:uuid,notnull" json:"price_id"`     // Required for all subscriptions

	// Scheduled tier change (for downgrades that take effect at end of period)
	ScheduledPriceID *uuid.UUID `bun:"scheduled_price_id,type:uuid,nullzero" json:"scheduled_price_id,omitempty"`

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
		nt := now.Add(-2 * time.Minute)
		// Ensure endedAt is before cancelledAt to satisfy DB constraint
		s.EndedAt = &nt
	}

	return nil
}

func (s *Subscription) Cancel(reason string, cancelType *CancelType) error {
	now := time.Now()
	if err := s.ResetCurrentPeriods(); err != nil {
		return err
	}

	s.CancelledAt = &now
	s.CancelType = cancelType
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
