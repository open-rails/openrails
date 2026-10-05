package billing

import (
	"time"

	"github.com/google/uuid"
)

// CatalogDrift is an open finding that a PSP's copy of a catalog resource
// differs from OpenRails: a field that disagrees, or a resource missing on
// one side. Findings are alerts; reconciling the price or product resolves
// them.
type CatalogDrift struct {
	ID                 uuid.UUID  `json:"id"`
	PSPID              PSPID      `json:"psp_id"`
	Rail               string     `json:"rail"`
	Kind               string     `json:"kind"`
	ResourceType       string     `json:"resource_type"`
	ResourceID         string     `json:"resource_id"`
	ExternalResourceID string     `json:"external_resource_id"`
	Field              string     `json:"field"`
	OpenRailsValue     string     `json:"openrails_value"`
	ExternalValue      string     `json:"external_value"`
	DetectedAt         time.Time  `json:"detected_at"`
	ResolvedAt         *time.Time `json:"resolved_at"`
}

// CatalogDriftListParams filters ListCatalogDrift.
type CatalogDriftListParams struct {
	PageRequest
	Rail         string
	Kind         string
	ResourceType string
}

// CatalogDriftRefresh is the result of reading every linked PSP's catalog
// now: what it scanned, and how many findings it opened and resolved.
type CatalogDriftRefresh struct {
	ScannedProducts    int `json:"scanned_products"`
	ScannedPrices      int `json:"scanned_prices"`
	ScannedNMIPlans    int `json:"scanned_nmi_plans"`
	ScannedSolanaPlans int `json:"scanned_solana_plans"`
	OpenedFindings     int `json:"opened_findings"`
	ResolvedFindings   int `json:"resolved_findings"`
	OpenFindings       int `json:"open_findings"`
}
