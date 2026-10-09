// Package staffperm is the standalone server's staff permissions: its
// merchant persona's, which the control plane declares and the server's
// admin API checks.
package staffperm

const (
	// Read, Write and Admin are the server's Routes.Permissions: AdminRead,
	// AdminUpdate, and Admin for both Catalog and MerchantConfig.
	Read  = "merchant:billing:read"
	Write = "merchant:billing:write"
	Admin = "merchant:billing:admin"
	// Metrics is the server's Routes.Permissions.Metrics: revenue and
	// sales. Support does not hold it.
	Metrics = "merchant:billing:metrics"

	// AuthKit's built-ins on the merchant persona: the team, and the
	// merchant's API keys.
	MembersRead       = "merchant:members:read"
	MembersManage     = "merchant:members:manage"
	CredentialsManage = "merchant:credentials:manage"

	// All is the merchant owner's grant.
	All = "merchant:*"
)
