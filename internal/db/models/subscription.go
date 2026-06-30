package models

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
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
	// StatusUnknown (#632): locally "active" but the period elapsed with no
	// confirming payment AND we cannot rebill it ourselves (provider-auto-billed:
	// CCBill or vault-less NMI). A needs-provider-verification holding state —
	// convergence will not GUESS; provider-pull (#633) resolves it to the real state.
	StatusUnknown SubscriptionStatus = "unknown"
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
	ID uuid.UUID `json:"id"`
	// MerchantID is the owning merchant (#336): lets workers pin app.merchant_id when
	// writing on this subscription's behalf.
	MerchantID uuid.UUID `json:"merchant_id"`
	// CustomerID is the OpenRails payable merchant subject for this row (#317).
	// Additive during the hard-cut rollout; writers populate it and readers move to
	// it before user_id is dropped. Join openrails.customers for issuer/subject.
	CustomerID uuid.UUID `json:"customer_id,omitempty"`
	ProductID  uuid.UUID `json:"product_id"` // Denormalized for efficient product-based lookups
	PriceID    uuid.UUID `json:"price_id"`   // Required for all subscriptions

	// Scheduled tier change (for downgrades that take effect at end of period)
	ScheduledPriceID *uuid.UUID `json:"scheduled_price_id,omitempty"`

	EntitlementsSpecSnapshot map[string]*int `json:"entitlements_spec_snapshot,omitempty"`
	CreditsSpecSnapshot      CreditsSpec     `json:"credits_spec_snapshot,omitempty"`

	Status                SubscriptionStatus `json:"status"`
	StartedAt             time.Time          `json:"started_at"`
	EndedAt               *time.Time         `json:"ended_at"`
	CurrentPeriodStartsAt *time.Time         `json:"current_period_starts_at"`
	CurrentPeriodEndsAt   *time.Time         `json:"current_period_ends_at"`

	// Payment rail information
	Rail               Rail       `json:"rail"`                 // Rail: nmi, ccbill, solana
	RailSubscriptionID string     `json:"rail_subscription_id"` // Subscription ID from rail
	UserEmail          *string    `json:"user_email,omitempty"`
	PaymentMethodID    *uuid.UUID `json:"payment_method_id"` // Reference to stored payment method

	// Manual rebill attempt fields for NMI
	LastRetryAt   *time.Time `json:"last_retry_at"`  // Date of last rebill attempt
	RetryAttempts *int       `json:"retry_attempts"` // Number of retry attempts (nullable for new subscriptions)
	NextRetryAt   *time.Time `json:"next_retry_at"`  // When to try next rebill
	GraceEndsAt   *time.Time `json:"grace_ends_at"`  // Optional grace window end during dunning (rail-specific)

	// Cancellation information
	CancelFeedback *string     `json:"cancel_feedback"` // User's cancellation message
	CancelType     *CancelType `json:"cancel_type"`     // Who/what caused cancellation
	CancelledAt    *time.Time  `json:"cancelled_at"`

	// DeletionScheduledAt is set for NMI-backed cancellations that defer the
	// rail-side delete_subscription until shortly before the paid period
	// ends (issue 216). While non-nil, the cancellation is still reversible (the
	// rail subscription is alive). The River finalizer clears it to nil
	// after calling DeleteRecurringSubscription.
	DeletionScheduledAt *time.Time `json:"deletion_scheduled_at,omitempty"`

	// Relationships
	Price         *Price         `json:"price,omitempty"`
	PaymentMethod *PaymentMethod `json:"payment_method,omitempty"`

	// Product relation for description
	Product *Product `json:"product,omitempty"`

	Metadata json.RawMessage `json:"gateway_response,omitempty"` // Renamed from GatewayResponse - stores arbitrary subscription metadata

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
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
	cycleHours := price.RecurringCycleHours()
	if cycleHours == nil {
		return fmt.Errorf("recurring price billing cycle is required")
	}

	billingCycle := time.Duration(*cycleHours) * time.Hour
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
