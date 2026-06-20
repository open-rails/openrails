// Package controlplane implements OpenRails' OpenRails-owned AuthKit control
// plane (issue #224).
//
// OpenRails owns an AuthKit control plane rather than acting only as an external
// JWT verifier. In self-hosted / locked-down mode OpenRails:
//
//   - mounts only the AuthKit route groups it intentionally exposes (NOT the
//     full DefaultAPI surface),
//   - runs with public user registration and public org management disabled,
//   - bootstraps the default merchant's own AuthKit org, OpenRails roles, the
//     OpenRails `org:` permission set, and an initial deployment admin API key
//     through in-process AuthKit CORE calls (CreateOrg / DefineRole /
//     AssignRole / MintAPIKey) — never raw SQL or a private HTTP route.
//
// HARDCUT (#537): merchant-local authority lives in the caller's merchant
// AuthKit org (`org:` permissions). Cross-merchant directory authority lives in
// AuthKit platform RBAC (`platform:` permissions).
package controlplane

import (
	"strings"

	authcore "github.com/open-rails/authkit/core"
)

// Permission is an OpenRails permission definition. AuthKit owns the RBAC
// layers; OpenRails defines host resources inside `org:`.
type Permission = authcore.PermissionDef

// OpenRails permissions. `org:` permissions are merchant-local AuthKit org
// permissions. `platform:` permissions come from AuthKit platform RBAC. Customer
// self-service routes do not carry OpenRails `self:` grants: the authenticated
// subject is the route target.
const (
	// PermAdmin is kept as a deprecated source-compatibility alias for the
	// merchant-owner apex grant. It is NOT a magic permission and does not satisfy
	// arbitrary checks by itself.
	PermAdmin = authcore.OrgOwnerGrant

	PermCreditsRead  = "org:credits:read"
	PermCreditsWrite = "org:credits:update"

	// PermCreditsSpend remains a domain action: spending payer balance is not the
	// same operation as updating credit records.
	PermCreditsSpend = "org:credits:spend"

	PermEntitlementsRead = "org:entitlements:read"

	PermCatalogWrite = "org:catalog:update"

	PermPaymentsRefund      = "org:payments:refund"
	PermSubscriptionsCancel = "org:subscriptions:update"

	PermMerchantBillingRead        = "org:billing:read"
	PermMerchantEntitlementsWrite  = "org:entitlements:update"
	PermMerchantProductAccessWrite = "org:product_access:update"
	PermMerchantCreditsWrite       = PermCreditsWrite
	PermMerchantPaymentsWrite      = "org:payments:update"
	PermMerchantSubscriptionsWrite = PermSubscriptionsCancel
	PermMerchantConfigurationRead  = "org:configuration:read"
	PermMerchantConfigurationWrite = "org:configuration:update"
	PermMerchantSecretsList        = "org:secrets:read"
	PermMerchantSecretsWrite       = "org:secrets:update"
	PermMerchantSecretsDelete      = "org:secrets:delete"
	PermMerchantSecretsTest        = "org:secrets:test"
	PermMerchantMetricsRead        = "org:metrics:read"

	// Existing route groups still use this name for the merchant directory gate.
	// The value is a concrete AuthKit platform permission; super-admin roles hold
	// authcore.PlatformSuperAdminGrant (`platform:*`), which expands to this.
	PermPlatformSuperadmin = authcore.PermPlatformOrgsUpdate
)

// catalogEntries is the canonical ordered list of OpenRails permissions with
// human-readable descriptions. Keep ordering stable so seeding is deterministic.
var catalogEntries = []Permission{
	{Name: PermCreditsRead, Description: "Read credit balances and transactions."},
	{Name: PermCreditsWrite, Description: "Deposit, withdraw, hold, capture, and release credits."},
	{Name: PermCreditsSpend, Description: "Bill (draw down) a customer's balance on the hot path: authorize, hold, and capture against the payer's account (issue #246)."},
	{Name: PermEntitlementsRead, Description: "Read/enrich entitlements."},
	{Name: PermCatalogWrite, Description: "Create and update products and prices."},
	{Name: PermPaymentsRefund, Description: "Refund payments."},
	{Name: PermSubscriptionsCancel, Description: "Cancel subscriptions on behalf of the operator."},
	{Name: PermMerchantBillingRead, Description: "Merchant admin: read customer billing detail."},
	{Name: PermMerchantEntitlementsWrite, Description: "Merchant admin: update customer entitlements."},
	{Name: PermMerchantProductAccessWrite, Description: "Merchant admin: update customer product access."},
	{Name: PermMerchantPaymentsWrite, Description: "Merchant admin: update payment state."},
	{Name: PermMerchantConfigurationRead, Description: "Merchant admin: read merchant profile and operational configuration."},
	{Name: PermMerchantConfigurationWrite, Description: "Merchant admin: update merchant profile and operational configuration."},
	{Name: PermMerchantSecretsList, Description: "Merchant admin: list configured merchant-secret status without plaintext."},
	{Name: PermMerchantSecretsWrite, Description: "Merchant admin: create or rotate write-only merchant secrets."},
	{Name: PermMerchantSecretsDelete, Description: "Merchant admin: delete merchant secrets."},
	{Name: PermMerchantSecretsTest, Description: "Merchant admin: validate merchant secrets without reading plaintext."},
	{Name: PermMerchantMetricsRead, Description: "Merchant admin: read merchant analytics metrics."},
}

// merchantCatalog is the set of merchant-admin permissions accepted on a FEDERATED
// merchant-signed delegated access token for the `/v1/admin/*` surface
// (issue #259). Exact-match, no wildcard. A token presenting any permission
// outside merchantCatalog is rejected: browser tokens must never carry
// operator/server-to-server grants.
var merchantCatalog = map[string]struct{}{
	PermMerchantBillingRead:        {},
	PermMerchantEntitlementsWrite:  {},
	PermMerchantProductAccessWrite: {},
	PermCatalogWrite:               {},
	PermMerchantCreditsWrite:       {},
	PermMerchantPaymentsWrite:      {},
	PermMerchantSubscriptionsWrite: {},
	PermMerchantConfigurationRead:  {},
	PermMerchantConfigurationWrite: {},
	PermMerchantSecretsList:        {},
	PermMerchantSecretsWrite:       {},
	PermMerchantSecretsDelete:      {},
	PermMerchantSecretsTest:        {},
	PermMerchantMetricsRead:        {},
}

// MerchantCatalogNames returns the merchant-admin permission names in catalog order.
func MerchantCatalogNames() []string {
	return []string{
		PermMerchantBillingRead,
		PermMerchantEntitlementsWrite,
		PermMerchantProductAccessWrite,
		PermCatalogWrite,
		PermMerchantCreditsWrite,
		PermMerchantPaymentsWrite,
		PermMerchantSubscriptionsWrite,
		PermMerchantConfigurationRead,
		PermMerchantConfigurationWrite,
		PermMerchantSecretsList,
		PermMerchantSecretsWrite,
		PermMerchantSecretsDelete,
		PermMerchantSecretsTest,
		PermMerchantMetricsRead,
	}
}

// IsMerchantPermission reports whether perm is a known merchant-admin permission.
func IsMerchantPermission(perm string) bool {
	_, ok := merchantCatalog[strings.TrimSpace(perm)]
	return ok
}

// IsDelegatedPermission reports whether perm is acceptable on a browser-direct
// delegated access token. Self-service tokens may omit permissions entirely;
// any supplied permission must be a browser-safe merchant-admin permission.
func IsDelegatedPermission(perm string) bool {
	return IsMerchantPermission(perm)
}

// Catalog returns the OpenRails permission catalog as AuthKit PermissionDefs,
// suitable for core.Config.Permissions. The returned slice is a copy so
// callers cannot mutate the package-level catalog.
func Catalog() []Permission {
	out := make([]Permission, len(catalogEntries))
	copy(out, catalogEntries)
	return out
}

// CatalogNames returns just the permission names in catalog order.
func CatalogNames() []string {
	out := make([]string, len(catalogEntries))
	for i, e := range catalogEntries {
		out[i] = e.Name
	}
	return out
}

// Admin-role identity. The operator role is a merchant-local org role. The
// role identifier is kept stable ("openrails-operator") to avoid re-seeding
// existing deployments.
const (
	// OperatorRole is the OpenRails admin role seeded under a merchant's own AuthKit
	// org. It holds the merchant-local OpenRails `org:` permissions.
	OperatorRole = "openrails-operator"

	// PlatformRole is the AuthKit platform role name used by hosted deployments.
	PlatformRole = "openrails-platform-superadmin"
)

// OperatorRolePermissions returns the permission names granted to a merchant's
// operator role: the OpenRails host-defined `org:` permission set.
func OperatorRolePermissions() []string {
	return CatalogNames()
}

// PlatformRolePermissions returns the AuthKit platform super-admin grant. AuthKit
// expands this to concrete `platform:` permissions; it never reaches `org:`.
func PlatformRolePermissions() []string {
	return []string{authcore.PlatformSuperAdminGrant}
}
