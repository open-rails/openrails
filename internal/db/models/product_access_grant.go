package models

import (
	"time"

	"github.com/google/uuid"
)

// ProductAccessSourceType describes where a product access grant came from.
// DISTINCT from EntitlementSourceType: product access is durable ownership of a
// specific product, not a feature window.
type ProductAccessSourceType string

const (
	ProductAccessSourcePurchase     ProductAccessSourceType = "purchase"
	ProductAccessSourceSubscription ProductAccessSourceType = "subscription"
	ProductAccessSourceAdmin        ProductAccessSourceType = "admin"
)

// ProductAccessStatus is the lifecycle state of a grant.
type ProductAccessStatus string

const (
	ProductAccessStatusActive  ProductAccessStatus = "active"
	ProductAccessStatusRevoked ProductAccessStatus = "revoked"
)

// ProductAccessRevokeReason indicates why a grant was revoked. Mirrors the
// payment-reversal vocabulary so refund/chargeback/admin revocations are
// consistent across product access and feature entitlements.
type ProductAccessRevokeReason string

const (
	ProductAccessRevokeRefund     ProductAccessRevokeReason = "refund"
	ProductAccessRevokeChargeback ProductAccessRevokeReason = "chargeback"
	ProductAccessRevokeAdmin      ProductAccessRevokeReason = "admin"
	ProductAccessRevokeFraud      ProductAccessRevokeReason = "fraud"
)

// ProductAccessGrant is a durable, application-facing record that a user owns /
// has access to a specific PRODUCT (issue #250). It answers "does this user own
// product X?" and powers purchased-library views without walking payment history.
//
// It is DISTINCT from openrails.entitlements (feature access). A product may carry
// EntitlementsSpec and/or CreditsSpec AND produce a grant.
type ProductAccessGrant struct {
	ID uuid.UUID `json:"id"`

	// TenantID scopes this row to a tenant (issue #223/#227). Nullzero + DB
	// default lets single-tenant inserts fall back to the resolved tenant.
	TenantID uuid.UUID `json:"tenant_id"`

	// TenantSubjectID is the OpenRails payable tenant subject for this row (#317).
	// Additive during the hard-cut rollout; writers populate it and readers move to
	// it before user_id is dropped. Join openrails.tenant_subjects for issuer/subject.
	TenantSubjectID uuid.UUID `json:"tenant_subject_id,omitempty"`
	ProductID       uuid.UUID `json:"product_id"`

	SourceType ProductAccessSourceType `json:"source_type"`
	// SourceID is the idempotency key component (payment id, admin grant id,
	// subscription id as text). Stored non-null (default '') so the idempotency
	// index treats it uniformly.
	SourceID  string     `json:"source_id"`
	PaymentID *uuid.UUID `json:"payment_id,omitempty"`

	Status ProductAccessStatus `json:"status"`

	StartsAt time.Time  `json:"starts_at"`
	EndsAt   *time.Time `json:"ends_at,omitempty"`

	RevokedAt    *time.Time                 `json:"revoked_at,omitempty"`
	RevokeReason *ProductAccessRevokeReason `json:"revoke_reason,omitempty"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// IsActiveAt reports whether the grant currently confers access at time t:
// status active, started, not ended, not revoked.
func (g *ProductAccessGrant) IsActiveAt(t time.Time) bool {
	if g == nil {
		return false
	}
	if g.Status != ProductAccessStatusActive || g.RevokedAt != nil {
		return false
	}
	if g.StartsAt.After(t) {
		return false
	}
	if g.EndsAt != nil && !g.EndsAt.After(t) {
		return false
	}
	return true
}
