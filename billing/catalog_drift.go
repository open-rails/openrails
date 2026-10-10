package billing

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
