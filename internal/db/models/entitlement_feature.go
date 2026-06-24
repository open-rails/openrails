package models

import (
	"time"

	"github.com/google/uuid"
)

// EntitlementFeature is a Stripe-shaped first-class feature definition (issue
// #245). It sits ON TOP of OpenRails' temporal entitlement-window ledger
// (openrails.entitlements) — the window ledger remains the source of truth for
// active access; this row just names a feature and pins its stable LookupKey.
//
// LookupKey is the value carried in AuthKit JWT entitlements and host-app
// checks (e.g. "premium", "api_access"). It is unique per merchant.
type EntitlementFeature struct {
	ID uuid.UUID `json:"id"`

	// MerchantID scopes this row to a merchant (issue #223/#227). Nullzero + DB
	// default: inserts that leave it zero fall back to the default merchant.
	MerchantID uuid.UUID `json:"merchant_id"`

	LookupKey string         `json:"lookup_key"`
	Name      string         `json:"name"`
	Metadata  map[string]any `json:"metadata,omitempty"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// ProductEntitlementFeature attaches an EntitlementFeature to a product (Stripe's
// product_feature) (issue #245). When the product is purchased the attached
// feature is granted for DurationDays (nil = indefinite).
type ProductEntitlementFeature struct {
	ID uuid.UUID `json:"id"`

	MerchantID uuid.UUID `json:"merchant_id"`

	ProductID            uuid.UUID `json:"product_id"`
	EntitlementFeatureID uuid.UUID `json:"entitlement_feature_id"`

	// DurationDays is the access window granted on purchase. nil = indefinite.
	DurationDays *int           `json:"duration_days,omitempty"`
	Metadata     map[string]any `json:"metadata,omitempty"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`

	// Relationships
	Feature *EntitlementFeature `json:"feature,omitempty"`
}
