package billing

// AccessLevel is what a caller may do in a staff area.
type AccessLevel string

const (
	AccessNone   AccessLevel = "none"
	AccessRead   AccessLevel = "read"
	AccessUpdate AccessLevel = "update"
)

// AdminAccess is what the caller may use of each staff area at the merchant,
// counting an area only where its group is on and the caller holds its
// permission, at the merchant or in the root scope: what the admin console
// shows them. Admin is customer support, read-only without
// MerchantCustomerManage or RootCustomerManage. ScopeID is the merchant's own
// scope's id. Assistants reports the enabled assistants in areas the caller
// can use.
type AdminAccess struct {
	Admin          AccessLevel     `json:"admin"`
	Catalog        bool            `json:"catalog"`
	MerchantConfig bool            `json:"merchant_config"`
	Metrics        bool            `json:"metrics"`
	ScopeID        string          `json:"scope_id"`
	Assistants     AdminAssistants `json:"assistants"`
}

// AdminAssistants reports assistant availability for the caller's staff areas.
type AdminAssistants struct {
	CatalogCopilot  bool `json:"catalog_copilot"`
	CatalogDrafting bool `json:"catalog_drafting"`
}
