package models

import (
	"time"

	"github.com/google/uuid"
	"github.com/uptrace/bun"
)

// EntitlementFeature is a Stripe-shaped first-class feature definition (issue
// #245). It sits ON TOP of OpenRails' temporal entitlement-window ledger
// (billing.entitlements) — the window ledger remains the source of truth for
// active access; this row just names a feature and pins its stable LookupKey.
//
// LookupKey is the value carried in AuthKit JWT entitlements and host-app
// checks (e.g. "premium", "api_access"). It is unique per tenant.
type EntitlementFeature struct {
	bun.BaseModel `bun:"table:billing.entitlement_features,alias:ef"`

	ID uuid.UUID `bun:"id,pk,type:uuid,default:uuidv7()" json:"id"`

	// TenantID scopes this row to a tenant (issue #223/#227). Nullzero + DB
	// default: inserts that leave it zero fall back to the default tenant.
	TenantID uuid.UUID `bun:"tenant_id,type:uuid,nullzero" json:"tenant_id"`

	LookupKey string         `bun:"lookup_key,notnull" json:"lookup_key"`
	Name      string         `bun:"name,notnull" json:"name"`
	Active    bool           `bun:"active,notnull,default:true" json:"active"`
	Metadata  map[string]any `bun:"metadata,type:jsonb,nullzero" json:"metadata,omitempty"`

	CreatedAt time.Time `bun:"created_at,notnull,default:current_timestamp" json:"created_at"`
	UpdatedAt time.Time `bun:"updated_at,notnull,default:current_timestamp" json:"updated_at"`
}

// ProductEntitlementFeature attaches an EntitlementFeature to a product (Stripe's
// product_feature) (issue #245). When the product is purchased the attached
// feature is granted for DurationDays (nil = indefinite).
type ProductEntitlementFeature struct {
	bun.BaseModel `bun:"table:billing.product_entitlement_features,alias:pef"`

	ID uuid.UUID `bun:"id,pk,type:uuid,default:uuidv7()" json:"id"`

	TenantID uuid.UUID `bun:"tenant_id,type:uuid,nullzero" json:"tenant_id"`

	ProductID            uuid.UUID `bun:"product_id,notnull,type:uuid" json:"product_id"`
	EntitlementFeatureID uuid.UUID `bun:"entitlement_feature_id,notnull,type:uuid" json:"entitlement_feature_id"`

	// DurationDays is the access window granted on purchase. nil = indefinite.
	DurationDays *int           `bun:"duration_days,nullzero" json:"duration_days,omitempty"`
	Metadata     map[string]any `bun:"metadata,type:jsonb,nullzero" json:"metadata,omitempty"`

	CreatedAt time.Time `bun:"created_at,notnull,default:current_timestamp" json:"created_at"`
	UpdatedAt time.Time `bun:"updated_at,notnull,default:current_timestamp" json:"updated_at"`

	// Relationships
	Feature *EntitlementFeature `bun:"rel:belongs-to,join:entitlement_feature_id=id" json:"feature,omitempty"`
}
