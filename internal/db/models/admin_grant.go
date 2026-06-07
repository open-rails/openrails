package models

import (
	"time"

	"github.com/google/uuid"
	"github.com/uptrace/bun"
)

// AdminGrant represents an admin-initiated product grant to a user.
// This is used for comps, contest winners, manual payments (PayPal/cash), partnerships, etc.
// The admin picks a Price/Product, and the system derives entitlements from Product.EntitlementsSpec.
type AdminGrant struct {
	bun.BaseModel `bun:"table:billing.admin_grants,alias:ag"`

	ID uuid.UUID `bun:"id,pk,type:uuid,default:uuidv7()" json:"id"`
	// TenantSubjectID is the OpenRails payable tenant subject for this row (#317).
	// Additive during the hard-cut rollout; writers populate it and readers move to
	// it before user_id is dropped. Join billing.tenant_subjects for issuer/subject.
	TenantSubjectID uuid.UUID  `bun:"tenant_subject_id,type:uuid,nullzero" json:"tenant_subject_id,omitempty"`
	PriceID         *uuid.UUID `bun:"price_id,type:uuid,nullzero" json:"price_id,omitempty"` // Optional: Price/Product being granted
	GrantedBy       string     `bun:"granted_by,notnull" json:"granted_by"`                  // Admin user ID who made the grant

	// Reason for the grant (e.g., "comp", "contest_winner", "refund_compensation", "partnership", "manual_payment")
	Reason string `bun:"reason,notnull" json:"reason"`

	// Optional payment linkage (only if money was received)
	PaymentID *uuid.UUID `bun:"payment_id,type:uuid,nullzero" json:"payment_id,omitempty"`

	// Optional duration override in days (nil = use Product.EntitlementsSpec, 0 = indefinite)
	DurationDays *int `bun:"duration_days,nullzero" json:"duration_days,omitempty"`

	CreatedAt time.Time `bun:"created_at,notnull,default:current_timestamp" json:"created_at"`

	// Relationships
	Price   *Price   `bun:"rel:belongs-to,join:price_id=id" json:"price,omitempty"`
	Payment *Payment `bun:"rel:belongs-to,join:payment_id=id" json:"payment,omitempty"`
}
