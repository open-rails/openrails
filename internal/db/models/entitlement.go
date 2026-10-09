package models

import (
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/gen"
)

// AccessSourceType is where a product access window came from; the grant
// ledger's vocabulary.
type AccessSourceType string

const (
	AccessSourcePurchase     AccessSourceType = "purchase"
	AccessSourceSubscription AccessSourceType = "subscription"
	AccessSourceGrace        AccessSourceType = "grace"
	// AccessSourceGrant is a free product grant: a comp, staff access, an
	// import or a migration.
	AccessSourceGrant AccessSourceType = "grant"
)

// AccessRevokeReason says why a product access window was revoked.
type AccessRevokeReason string

const (
	AccessRevokeAdmin      AccessRevokeReason = "admin"
	AccessRevokeDowngrade  AccessRevokeReason = "downgrade"
	AccessRevokeChargeback AccessRevokeReason = "chargeback"
	AccessRevokeRefund     AccessRevokeReason = "refund"
	AccessRevokeFraud      AccessRevokeReason = "fraud"
	AccessRevokeDunning    AccessRevokeReason = "dunning_failed"
	AccessRevokeSuperseded AccessRevokeReason = "superseded"
)

// ProductAccess is one window [StartsAt, EndsAt) in which a customer holds a
// product, and so the product's keys. It projects one access grant.
type ProductAccess struct {
	ID           uuid.UUID
	MerchantID   uuid.UUID
	CustomerID   uuid.UUID
	ProductID    uuid.UUID
	GrantID      uuid.UUID
	SourceType   AccessSourceType
	SourceID     string
	PaymentID    *uuid.UUID
	StartsAt     time.Time
	EndsAt       *time.Time
	RevokedAt    *time.Time
	RevokeReason *AccessRevokeReason
	CreatedAt    time.Time
	UpdatedAt    time.Time
	DeletedAt    *time.Time
}

// IsActiveAt reports whether the window grants access at t.
func (a *ProductAccess) IsActiveAt(t time.Time) bool {
	return a != nil && a.RevokedAt == nil && a.DeletedAt == nil && !a.StartsAt.After(t) && (a.EndsAt == nil || a.EndsAt.After(t))
}

func ProductAccessFromGen(r gen.BillingProductAccess) *ProductAccess {
	a := &ProductAccess{
		ID: r.ID, MerchantID: r.MerchantID, CustomerID: r.CustomerID, ProductID: r.ProductID, GrantID: r.GrantID,
		SourceType: AccessSourceType(r.SourceType), SourceID: r.SourceID, PaymentID: r.PaymentID,
		StartsAt: r.StartsAt, EndsAt: r.EndsAt, RevokedAt: r.RevokedAt,
		CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt, DeletedAt: r.DeletedAt,
	}
	if r.RevokeReason != nil {
		reason := AccessRevokeReason(*r.RevokeReason)
		a.RevokeReason = &reason
	}
	return a
}
