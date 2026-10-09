// Package staffperm is the standalone server's staff permissions: its
// merchant persona's, which the control plane declares and the server's
// admin API checks.
package staffperm

const (
	// Read, Write and Admin are the server's Routes.Permissions: AdminRead,
	// AdminWrite, and Admin for both CatalogWrite and MerchantConfig.
	Read  = "merchant:billing:read"
	Write = "merchant:billing:write"
	Admin = "merchant:billing:admin"

	// AuthKit's built-ins on the merchant persona: the team, and the
	// merchant's API keys.
	MembersRead       = "merchant:members:read"
	MembersManage     = "merchant:members:manage"
	CredentialsManage = "merchant:credentials:manage"

	// All is the merchant owner's grant.
	All = "merchant:*"
)
