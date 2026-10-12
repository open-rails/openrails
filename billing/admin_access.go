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
// shows them. Catalog and MerchantConfig distinguish read from manage, as
// Admin does for customer support. Admin is read-only without
// MerchantCustomerManage or RootCustomerManage. ScopeID is the merchant's own
// scope's id, the merchant's: its AuthKit group, whose roles the console's
// Roles page manages.
type AdminAccess struct {
	Admin          AccessLevel `json:"admin"`
	Catalog        AccessLevel `json:"catalog"`
	MerchantConfig AccessLevel `json:"merchant_config"`
	Metrics        bool        `json:"metrics"`
	ScopeID        string      `json:"scope_id"`
	// ApplicationPermissions lists effective permissions for mounted /v1/app routes.
	// It is empty for human callers, even when their role holds these permissions.
	// Root grants are expressed as their merchant-scoped equivalents.
	ApplicationPermissions []Permission `json:"application_permissions"`
}
