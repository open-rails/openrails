package billing

// MerchantBillingImportResult is the committed receipt for a complete billing
// archive. Repeating an import returns its receipt without replaying records.
type MerchantBillingImportResult struct {
	MerchantID      MerchantID `json:"merchant_id"`
	Digest          string     `json:"digest"`
	Rows            int64      `json:"rows,string"`
	AlreadyImported bool       `json:"already_imported"`
}
