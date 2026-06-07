package models

import (
	"time"

	"github.com/google/uuid"
	"github.com/uptrace/bun"
)

// PaymentMethod represents a stored payment method across multiple processors
// This replaces processor-specific payment method tables
type PaymentMethod struct {
	bun.BaseModel `bun:"table:billing.payment_methods,alias:pm"`

	ID uuid.UUID `bun:"id,pk,type:uuid" json:"id"`
	// TenantSubjectID is the OpenRails payable tenant subject for this row (#317).
	// Additive during the hard-cut rollout; writers populate it and readers move to
	// it before user_id is dropped. Join billing.tenant_subjects for issuer/subject.
	TenantSubjectID uuid.UUID `bun:"tenant_subject_id,type:uuid,nullzero" json:"tenant_subject_id,omitempty"`
	Processor       Processor `bun:"processor,notnull" json:"processor"` // Processor: mobius, ccbill, solana

	// Processor-specific vault/payment method identifiers
	VaultID              string  `bun:"vault_id,notnull" json:"-"`               // Primary identifier in processor's system
	BillingID            *string `bun:"billing_id,nullzero" json:"-"`            // Secondary identifier (e.g., subscription ID)
	InitialTransactionID string  `bun:"initial_transaction_id,notnull" json:"-"` // Transaction that created this vault

	// Payment method metadata
	LastFour      *string        `bun:"last_four,nullzero" json:"last_four"`           // Last 4 digits of card
	CardType      *string        `bun:"card_type,nullzero" json:"card_type"`           // "Visa", "MasterCard", etc.
	ExpiryDate    *string        `bun:"expiry_date,nullzero" json:"expiry_date"`       // "MM/YY" format
	FailureReason *string        `bun:"failure_reason,nullzero" json:"failure_reason"` // Reason if inactive
	Metadata      map[string]any `bun:"metadata,type:jsonb,nullzero" json:"metadata,omitempty"`

	CreatedAt time.Time `bun:"created_at,notnull,default:current_timestamp" json:"created_at"`
	UpdatedAt time.Time `bun:"updated_at,notnull,default:current_timestamp" json:"updated_at"`

	// Relationships
	Subscriptions []*Subscription `bun:"rel:has-many,join:id=payment_method_id" json:"subscriptions,omitempty"`
}
