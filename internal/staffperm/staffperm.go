// Package staffperm is the standalone server's staff permissions: its
// merchant persona's, which the control plane declares and the server's
// guards on the merchant API name.
package staffperm

const (
	// Read, Write and Admin guard the merchant API's reads, writes and
	// configuration (openrails.StaffReads, StaffWrites, MerchantConfig).
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
