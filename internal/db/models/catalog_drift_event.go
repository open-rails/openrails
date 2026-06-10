package models

import (
	"time"

	"github.com/google/uuid"
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
	CatalogDriftProviderSolana CatalogDriftProvider = "solana"
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
	// CatalogDriftMissingInSolana: an OpenRails price stores an on-chain plan PDA
	// whose Plan account is no longer present on chain (deleted / wrong PDA). There
	// is no orphan_in_solana kind: the Subscriptions program has no "list all plans"
	// API, so Solana drift is detected per-stored-price, not by enumerating chain.
	CatalogDriftMissingInSolana CatalogDriftKind = "missing_in_solana"
	CatalogDriftFieldDrift      CatalogDriftKind = "field_drift"
)

// CatalogDriftResourceType identifies which catalog object a drift event concerns.
type CatalogDriftResourceType string

const (
	CatalogDriftResourceProduct CatalogDriftResourceType = "product"
	CatalogDriftResourcePrice   CatalogDriftResourceType = "price"
)

// CatalogDriftEvent is an alert-only record produced by the catalog
// reconciliation loop (issue #209). The loop never mutates Stripe, NMI, or the
// catalog rows; it only records divergence here. An event is "open" while
// ResolvedAt IS NULL. Rows dedupe on (provider, kind, openrails_resource_type,
// openrails_resource_id, external_resource_id, field) so reruns are idempotent.
//
// The external_resource_id column generalizes across providers: it holds the
// Stripe object id for the Stripe pass and the NMI plan_id for the NMI pass.
type CatalogDriftEvent struct {
	ID uuid.UUID `json:"id"`
	// Provider is "stripe" or "nmi"; it disambiguates the shared field_drift kind.
	Provider CatalogDriftProvider `json:"provider"`
	Kind     CatalogDriftKind     `json:"kind"`

	// OpenRailsResourceType is "product" or "price". Always set.
	OpenRailsResourceType CatalogDriftResourceType `json:"openrails_resource_type"`
	// OpenRailsResourceID is the UUID of the OpenRails row (empty for pure
	// orphans that have no OpenRails counterpart).
	OpenRailsResourceID string `json:"openrails_resource_id,omitempty"`
	// ExternalResourceID is the upstream object id: a Stripe product/price id for
	// the Stripe pass, or an NMI plan_id for the NMI pass. For missing_in_*, it
	// carries the id we failed to find, so it is set for all kinds in practice.
	ExternalResourceID string `json:"external_resource_id,omitempty"`

	// Field is the diverged field name for field_drift; empty for orphan/missing.
	Field string `json:"field,omitempty"`
	// OpenRailsValue / ExternalValue carry the stringified diverging values for
	// field_drift; empty otherwise.
	OpenRailsValue string `json:"openrails_value,omitempty"`
	ExternalValue  string `json:"external_value,omitempty"`

	DetectedAt time.Time  `json:"detected_at"`
	ResolvedAt *time.Time `json:"resolved_at,omitempty"`
}
