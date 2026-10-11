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
// permission: what the admin console shows them. Admin is customer support,
// read-only without AdminUpdate. ScopeID is the scope those permissions are
// held in (Routes.Scope's ID): on the standalone server the merchant's
// AuthKit group, whose roles the console's Roles page manages.
type AdminAccess struct {
	Admin          AccessLevel `json:"admin"`
	Catalog        bool        `json:"catalog"`
	MerchantConfig bool        `json:"merchant_config"`
	Metrics        bool        `json:"metrics"`
	ScopeID        string      `json:"scope_id"`
}
