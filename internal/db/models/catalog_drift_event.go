package models

import (
	"strings"
	"time"

	"github.com/google/uuid"
)

// CatalogDriftProvider names the rail a drift event concerns. CCBill is
// absent: it has no catalog-list API, so its catalog stays manual-only.
type CatalogDriftProvider string

const (
	CatalogDriftProviderStripe CatalogDriftProvider = "stripe"
	CatalogDriftProviderNMI    CatalogDriftProvider = "nmi"
	CatalogDriftProviderSolana CatalogDriftProvider = "solana"
)

// CatalogDriftKind is the <kind> of a catalog.<kind> reconciliation finding.
//
//   - orphan_in_*: an upstream product/price/plan has no matching OpenRails
//     row. Alert-only; operators decide whether to import or delete it.
//   - missing_in_*: an OpenRails row stores an upstream id absent from the
//     pulled list. Resolve via the per-price reconcile action.
//   - field_drift: a row and its upstream mirror disagree on a mutable field;
//     Provider says which rail. Resolve via per-price reconcile.
type CatalogDriftKind string

const (
	CatalogDriftOrphanInStripe  CatalogDriftKind = "orphan_in_stripe"
	CatalogDriftMissingInStripe CatalogDriftKind = "missing_in_stripe"
	CatalogDriftOrphanInNMI     CatalogDriftKind = "orphan_in_nmi"
	CatalogDriftMissingInNMI    CatalogDriftKind = "missing_in_nmi"
	// CatalogDriftMissingInSolana: a price's on-chain plan PDA has no Plan
	// account. Solana has no orphan kind: the program cannot list plans, so
	// drift is checked per stored price.
	CatalogDriftMissingInSolana CatalogDriftKind = "missing_in_solana"
	CatalogDriftFieldDrift      CatalogDriftKind = "field_drift"
)

// CatalogDriftFindingPrefix prefixes the finding type of every catalog drift event.
const CatalogDriftFindingPrefix = "catalog."

// CatalogDriftKindOf is the drift kind of a catalog.* finding type.
func CatalogDriftKindOf(findingType string) CatalogDriftKind {
	return CatalogDriftKind(strings.TrimPrefix(findingType, CatalogDriftFindingPrefix))
}

// CatalogDriftResourceType identifies which catalog object a drift event concerns.
type CatalogDriftResourceType string

const (
	CatalogDriftResourceProduct CatalogDriftResourceType = "product"
	CatalogDriftResourcePrice   CatalogDriftResourceType = "price"
)

// CatalogDriftEvent is an alert-only catalog reconciliation finding: the loop
// never mutates providers or catalog rows. It is open while ResolvedAt is nil
// and dedupes on (PSP, kind, resource type, resource id, external id, field).
type CatalogDriftEvent struct {
	ID uuid.UUID `json:"id"`
	// PSPID is the immutable provider account whose catalog was compared.
	PSPID uuid.UUID `json:"psp_id"`
	// Provider is the PSP's rail; it disambiguates the shared field_drift kind.
	Provider CatalogDriftProvider `json:"provider"`
	Kind     CatalogDriftKind     `json:"kind"`

	// OpenRailsResourceType is "product" or "price". Always set.
	OpenRailsResourceType CatalogDriftResourceType `json:"openrails_resource_type"`
	// OpenRailsResourceID is the UUID of the OpenRails row (empty for pure
	// orphans that have no OpenRails counterpart).
	OpenRailsResourceID string `json:"openrails_resource_id,omitempty"`
	// ExternalResourceID is the upstream id (Stripe product/price id, NMI
	// plan_id); for missing_in_* it is the id not found.
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
