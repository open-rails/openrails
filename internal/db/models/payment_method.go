package models

import (
	"time"

	"github.com/google/uuid"
)

// PaymentMethod represents a stored payment method across multiple processors
// This replaces processor-specific payment method tables
type PaymentMethod struct {
	ID uuid.UUID `json:"id"`
	// TenantSubjectID is the OpenRails payable tenant subject for this row (#317).
	// Additive during the hard-cut rollout; writers populate it and readers move to
	// it before user_id is dropped. Join billing.tenant_subjects for issuer/subject.
	TenantSubjectID uuid.UUID `json:"tenant_subject_id,omitempty"`
	Processor       Processor `json:"processor"` // Processor: mobius, ccbill, solana

	// Processor-specific vault/payment method identifiers
	VaultID              string  `json:"-"` // Primary identifier in processor's system
	BillingID            *string `json:"-"` // Secondary identifier (e.g., subscription ID)
	InitialTransactionID string  `json:"-"` // Transaction that created this vault

	// Payment method metadata
	LastFour      *string        `json:"last_four"`      // Last 4 digits of card
	CardType      *string        `json:"card_type"`      // "Visa", "MasterCard", etc.
	ExpiryDate    *string        `json:"expiry_date"`    // "MM/YY" format
	FailureReason *string        `json:"failure_reason"` // Reason if inactive
	Metadata      map[string]any `json:"metadata,omitempty"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`

	// Relationships
	Subscriptions []*Subscription `json:"subscriptions,omitempty"`
}
