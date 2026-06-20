package policy

import "context"

// PermAdmin is a deprecated source-compatibility alias for the merchant-owner
// apex grant. It is not a magic permission.
const PermAdmin = "org:*"

// PermCatalogWrite is the narrow merchant catalog mutation capability. It
// mirrors controlplane.PermCatalogWrite without making gin-free route
// registration import the control-plane package.
const PermCatalogWrite = "org:catalog:update"

// AdminPermissionChecker is the live AuthKit effective-permission check the
// control plane provides for merchant-local `org:` permissions.
type AdminPermissionChecker interface {
	HasAdminPermission(ctx context.Context, tenantSlug, userID, perm string) (bool, error)
}

// PlatformSuperadminChecker is the live AuthKit platform-RBAC check.
type PlatformSuperadminChecker interface {
	HasPlatformSuperadmin(ctx context.Context, userID string) (bool, error)
}
