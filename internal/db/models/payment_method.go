package models

import (
	"time"

	"github.com/google/uuid"
)

// PaymentMethod represents a stored payment method across multiple processors
// This replaces processor-specific payment method tables
type PaymentMethod struct {
	ID uuid.UUID `json:"id"`
	// CustomerID is the OpenRails payable merchant subject for this row (#317).
	// Additive during the hard-cut rollout; writers populate it and readers move to
	// it before user_id is dropped. Join openrails.customers for issuer/subject.
	CustomerID uuid.UUID `json:"customer_id,omitempty"`
	Processor  Processor `json:"processor"` // Processor: mobius, ccbill, solana

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
