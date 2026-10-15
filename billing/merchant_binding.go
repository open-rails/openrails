package billing

import "context"

// MerchantBinding explicitly associates an existing billing identity with a
// business tenant. Its permission group's ID is always the merchant's ID.
// Neither a merchant name nor an authentication request can create this binding.
type MerchantBinding struct {
	ID       MerchantID
	TenantID string
}

// MerchantGroup is the live group metadata needed to verify a declared binding.
// It carries no authentication runtime or role grants.
type MerchantGroup struct {
	ID       string
	TenantID string
	Persona  string
	Deleted  bool
}

// MerchantGroupReader verifies the metadata of an explicitly configured group.
// The caller owns this dependency. Implementations must read the live group in
// binding's tenant, rather than derive ownership from its ID or a request header.
type MerchantGroupReader interface {
	LookupMerchantGroup(context.Context, MerchantBinding) (MerchantGroup, error)
}
