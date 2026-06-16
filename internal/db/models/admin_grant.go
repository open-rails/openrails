package models

import (
	"time"

	"github.com/google/uuid"
)

// EntitlementGrant represents an admin-initiated product grant to a user.
// This is used for comps, contest winners, manual payments (PayPal/cash), partnerships, etc.
// The admin picks a Price/Product, and the system derives entitlements from Product.EntitlementsSpec.
type EntitlementGrant struct {
	ID uuid.UUID `json:"id"`
	// CustomerID is the OpenRails payable merchant subject for this row (#317).
	// Additive during the hard-cut rollout; writers populate it and readers move to
	// it before user_id is dropped. Join openrails.customers for issuer/subject.
	CustomerID uuid.UUID  `json:"customer_id,omitempty"`
	PriceID    *uuid.UUID `json:"price_id,omitempty"` // Optional: Price/Product being granted
	GrantedBy  string     `json:"granted_by"`         // Admin user ID who made the grant

	// Reason for the grant (e.g., "comp", "contest_winner", "refund_compensation", "partnership", "manual_payment")
	Reason string `json:"reason"`

	// Optional payment linkage (only if money was received)
	PaymentID *uuid.UUID `json:"payment_id,omitempty"`

	// Optional duration override in days (nil = use Product.EntitlementsSpec, 0 = indefinite)
	DurationDays *int `json:"duration_days,omitempty"`

	CreatedAt time.Time `json:"created_at"`

	// Relationships
	Price   *Price   `json:"price,omitempty"`
	Payment *Payment `json:"payment,omitempty"`
}
