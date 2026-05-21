package models

import (
	"time"

	"github.com/google/uuid"
	"github.com/uptrace/bun"
)

// CatalogDriftProvider names the upstream payment processor a drift event
// concerns. It disambiguates the shared field_drift kind across providers.
//
// CCBill is deliberately absent: CCBill has no catalog-list API (FlexForms are
// write-only redirect URLs, DataLink exports members not forms), so CCBill
// catalog reconciliation is structurally impossible. CCBill stays manual-only.
type CatalogDriftProvider string

const (
	CatalogDriftProviderStripe CatalogDriftProvider = "stripe"
	CatalogDriftProviderNMI    CatalogDriftProvider = "nmi"
)

// CatalogDriftKind classifies a row in billing.catalog_drift_events. The
// orphan/missing kinds are provider-scoped; field_drift is shared and
// disambiguated by the Provider column.
//
//   - CatalogDriftOrphanInStripe / CatalogDriftOrphanInNMI: an upstream
//     Product/Price/Plan carries (or lacks) the OpenRails ownership marker but
//     has no matching OpenRails row. Surfaced so operators can decide whether to
//     import or delete it. Alert-only.
//   - CatalogDriftMissingInStripe / CatalogDriftMissingInNMI: an OpenRails row
//     stores an upstream object ID that was not present in the pulled list
//     (deleted or archived upstream). Resolve via the per-price reconcile action.
//   - CatalogDriftFieldDrift: an OpenRails row and its upstream mirror disagree
//     on a mutable field (name/description/amount/currency/active/...). The
//     Provider column says which processor drifted. Resolve via per-price
//     reconcile.
type CatalogDriftKind string

const (
	CatalogDriftOrphanInStripe  CatalogDriftKind = "orphan_in_stripe"
	CatalogDriftMissingInStripe CatalogDriftKind = "missing_in_stripe"
	CatalogDriftOrphanInNMI     CatalogDriftKind = "orphan_in_nmi"
	CatalogDriftMissingInNMI    CatalogDriftKind = "missing_in_nmi"
	CatalogDriftFieldDrift      CatalogDriftKind = "field_drift"
)

// CatalogDriftResourceType identifies which catalog object a drift event concerns.
type CatalogDriftResourceType string

const (
	CatalogDriftResourceProduct CatalogDriftResourceType = "product"
	CatalogDriftResourcePrice   CatalogDriftResourceType = "price"
)

// CatalogDriftEvent is an alert-only record produced by the catalog
// reconciliation loop (issue #209). The loop never mutates Stripe or the
// catalog rows; it only records divergence here. An event is "open" while
// ResolvedAt IS NULL. Rows dedupe on (kind, stripe_resource_id,
// openrails_resource_id, field) so reruns are idempotent.
type CatalogDriftEvent struct {
	bun.BaseModel `bun:"table:billing.catalog_drift_events,alias:cde"`

	ID   uuid.UUID        `bun:"id,pk,type:uuid,default:gen_random_uuid()" json:"id"`
	Kind CatalogDriftKind `bun:"kind,notnull" json:"kind"`

	// OpenRailsResourceType is "product" or "price". Always set.
	OpenRailsResourceType CatalogDriftResourceType `bun:"openrails_resource_type,notnull" json:"openrails_resource_type"`
	// OpenRailsResourceID is the UUID of the OpenRails row (empty for pure
	// orphans that have no OpenRails counterpart).
	OpenRailsResourceID string `bun:"openrails_resource_id,nullzero" json:"openrails_resource_id,omitempty"`
	// StripeResourceID is the Stripe object id (empty for missing_in_stripe when
	// the stored id is what is missing — in that case it carries the id we
	// failed to find, so it is set for all kinds in practice).
	StripeResourceID string `bun:"stripe_resource_id,nullzero" json:"stripe_resource_id,omitempty"`

	// Field is the diverged field name for field_drift; empty for orphan/missing.
	Field string `bun:"field,nullzero" json:"field,omitempty"`
	// OpenRailsValue / StripeValue carry the stringified diverging values for
	// field_drift; empty otherwise.
	OpenRailsValue string `bun:"openrails_value,nullzero" json:"openrails_value,omitempty"`
	StripeValue    string `bun:"stripe_value,nullzero" json:"stripe_value,omitempty"`

	DetectedAt time.Time  `bun:"detected_at,notnull,default:current_timestamp" json:"detected_at"`
	ResolvedAt *time.Time `bun:"resolved_at,nullzero" json:"resolved_at,omitempty"`
}
