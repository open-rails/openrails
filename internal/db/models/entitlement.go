package models

import (
	"time"

	"github.com/google/uuid"
	"github.com/uptrace/bun"
)

// EntitlementSourceType describes where an entitlement came from
type EntitlementSourceType string

const (
	EntitlementSourceSubscription EntitlementSourceType = "subscription"
	EntitlementSourceOneOff       EntitlementSourceType = "one_off"
	EntitlementSourceAdmin        EntitlementSourceType = "admin"
	EntitlementSourceGrace        EntitlementSourceType = "grace"
)

// EntitlementRevokeReason indicates why an entitlement was revoked
type EntitlementRevokeReason string

const (
	EntitlementRevokeAdmin      EntitlementRevokeReason = "admin"
	EntitlementRevokeDowngrade  EntitlementRevokeReason = "downgrade"
	EntitlementRevokeChargeback EntitlementRevokeReason = "chargeback"
	EntitlementRevokeRefund     EntitlementRevokeReason = "refund"
	EntitlementRevokeFraud      EntitlementRevokeReason = "fraud"
	EntitlementRevokeDunning    EntitlementRevokeReason = "dunning_failed"
	EntitlementRevokeSuperseded EntitlementRevokeReason = "superseded"
)

// Entitlement models a temporal access window to a named entitlement (e.g., "premium")
// SCD2-style: [StartAt, EndAt) with optional soft delete and revoke markers.
type Entitlement struct {
	bun.BaseModel `bun:"table:billing.entitlements,alias:ent"`

	ID uuid.UUID `bun:"id,pk,type:uuid,default:uuidv7()" json:"id"`

	// TenantID scopes this row to a tenant / billing namespace (issue #223).
	// Nullzero + DB default: inserts that leave it zero fall back to the
	// default-tenant column default, so single-tenant code keeps working.
	TenantID uuid.UUID `bun:"tenant_id,type:uuid,nullzero" json:"tenant_id"`

	TenantSubjectID uuid.UUID `bun:"tenant_subject_id,type:uuid,nullzero" json:"tenant_subject_id,omitempty"`
	UserID          string    `bun:"user_id,notnull" json:"user_id"`
	Entitlement     string    `bun:"entitlement,notnull" json:"entitlement"`

	StartAt time.Time  `bun:"start_at,notnull" json:"start_at"`
	EndAt   *time.Time `bun:"end_at,nullzero" json:"end_at,omitempty"`

	// Optional polymorphic source reference (e.g. subscription or one-off payment).
	SourceID   *uuid.UUID            `bun:"source_id,type:uuid,nullzero" json:"source_id,omitempty"`
	SourceType EntitlementSourceType `bun:"source_type,notnull" json:"source_type"`

	RevokedAt    *time.Time               `bun:"revoked_at,nullzero" json:"revoked_at,omitempty"`
	RevokeReason *EntitlementRevokeReason `bun:"revoke_reason,nullzero" json:"revoke_reason,omitempty"`

	CreatedAt time.Time  `bun:"created_at,notnull,default:current_timestamp" json:"created_at"`
	UpdatedAt time.Time  `bun:"updated_at,notnull,default:current_timestamp" json:"updated_at"`
	DeletedAt *time.Time `bun:",soft_delete,type:timestamptz" json:"deleted_at,omitempty"`
}
