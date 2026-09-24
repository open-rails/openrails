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
	CancelTypeUpgrade    CancelType = "upgrade"    // Superseded by a tier upgrade
	// CancelTypeEngineTakeover: NMI stopped billing; an engine successor bills from the boundary.
	CancelTypeEngineTakeover CancelType = "engine_takeover"
)

type Subscription struct {
	CollectionPolicy CollectionPolicy `json:"collection_policy"`
	ID               uuid.UUID        `json:"id"`
	// MerchantID is the owning merchant (#336): lets workers pin app.merchant_id when
	// writing on this subscription's behalf.
	MerchantID uuid.UUID `json:"merchant_id"`
	// CustomerID is the OpenRails payable merchant subject for this row (#317).
	// The ID is the host subject UUID within MerchantID; customers stores issuer metadata.
	CustomerID uuid.UUID `json:"customer_id,omitempty"`
	ProductID  uuid.UUID `json:"product_id"` // Denormalized for efficient product-based lookups
	PriceID    uuid.UUID `json:"price_id"`   // Required for all subscriptions

	// Scheduled tier change (for downgrades that take effect at end of period)
	ScheduledPriceID *uuid.UUID `json:"scheduled_price_id,omitempty"`

	EntitlementsSpecSnapshot map[string]*int `json:"entitlements_spec_snapshot,omitempty"`

	Status                SubscriptionStatus `json:"status"`
	StartedAt             time.Time          `json:"started_at"`
	EndedAt               *time.Time         `json:"ended_at"`
	CurrentPeriodStartsAt *time.Time         `json:"current_period_starts_at"`
	CurrentPeriodEndsAt   *time.Time         `json:"current_period_ends_at"`

	// Payment rail information
	Rail               Rail   `json:"rail"`                 // Rail: nmi, ccbill, solana
	RailSubscriptionID string `json:"rail_subscription_id"` // Subscription ID from rail
	// PspID is the PSP that owns this subscription (#641).
	PspID           uuid.UUID  `json:"psp_id"`
	UserEmail       *string    `json:"user_email,omitempty"`
	PaymentMethodID *uuid.UUID `json:"payment_method_id"` // Reference to stored payment method

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

// updateCurrentPeriods starts the next period where the current one ends (or
// now) and makes it exactly one billing cycle long.
func (s *Subscription) updateCurrentPeriods(billingCycle time.Duration) {
	periodStartsAt := time.Now()
	if s.CurrentPeriodEndsAt != nil && !s.CurrentPeriodEndsAt.IsZero() {
		periodStartsAt = *s.CurrentPeriodEndsAt
	}
	periodEndsAt := periodStartsAt.Add(billingCycle)
	s.CurrentPeriodStartsAt = &periodStartsAt
	s.CurrentPeriodEndsAt = &periodEndsAt
}

func (s *Subscription) ActivateWithPrice(price *Price) error {
	cycleHours := price.RecurringCycleHours()
	if cycleHours == nil || *cycleHours <= 0 {
		return fmt.Errorf("recurring price billing cycle is required")
	}
	s.updateCurrentPeriods(time.Duration(*cycleHours) * time.Hour)

	s.EndedAt = nil
	s.CancelType = nil
	s.CancelledAt = nil
	s.PriceID = price.ID
	s.ProductID = price.ProductID // Update product when price changes (upgrade/downgrade)
	s.CancelFeedback = nil
	s.Status = StatusActive

	return nil
}

func (s *Subscription) ClearRetrySchedule() {
	s.LastRetryAt = nil
	s.RetryAttempts = nil
	s.NextRetryAt = nil
	s.GraceEndsAt = nil
}

// Validate checks activation preconditions. amountCents is integer minor units
// (#818) — no monetary value is ever carried as a float.
func (s *Subscription) Validate(amountCents int64) error {
	if s.CurrentPeriodEndsAt != nil && s.CurrentPeriodEndsAt.Before(time.Now()) {
		if s.Status == StatusActive {
			return fmt.Errorf("cannot activate expired subscription without proper renewal")
		}
	}

	if s.Status == StatusActive && amountCents <= 0 {
		return fmt.Errorf("cannot activate subscription with invalid amount: %d cents", amountCents)
	}

	if s.Status == StatusPastDue {
		if s.RetryAttempts != nil && *s.RetryAttempts >= 5 {
			return fmt.Errorf("subscription has exceeded maximum dunning attempts, should be cancelled")
		}
	}

	return nil
}
