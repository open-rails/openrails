package models

import (
	"time"

	"github.com/google/uuid"
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
	ID uuid.UUID `json:"id"`

	// MerchantID scopes this row to a merchant / billing namespace (issue #223).
	// Nullzero + DB default: inserts that leave it zero fall back to the
	// default-merchant column default, so single-merchant code keeps working.
	MerchantID uuid.UUID `json:"merchant_id"`

	CustomerID  uuid.UUID `json:"customer_id,omitempty"`
	Entitlement string    `json:"entitlement"`

	StartAt time.Time  `json:"start_at"`
	EndAt   *time.Time `json:"end_at,omitempty"`

	// Optional polymorphic source reference (e.g. subscription or one-off payment).
	SourceID   *uuid.UUID            `json:"source_id,omitempty"`
	SourceType EntitlementSourceType `json:"source_type"`

	RevokedAt    *time.Time               `json:"revoked_at,omitempty"`
	RevokeReason *EntitlementRevokeReason `json:"revoke_reason,omitempty"`

	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
	DeletedAt *time.Time `json:"deleted_at,omitempty"`
}
