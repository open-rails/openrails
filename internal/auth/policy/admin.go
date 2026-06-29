package policy

import "context"

// PermMerchantCatalogUpdate is the narrow merchant catalog mutation capability. It
// mirrors controlplane.PermMerchantCatalogUpdate (== merchant:catalog:update, #554)
// without making gin-free route registration import the control-plane package.
const PermMerchantCatalogUpdate = "merchant:catalog:update"

// AdminPermissionChecker is the live AuthKit effective-permission check the
// control plane provides for merchant-local `merchant:` permissions.
type AdminPermissionChecker interface {
	HasAdminPermission(ctx context.Context, merchantRef, userID, perm string) (bool, error)
}
