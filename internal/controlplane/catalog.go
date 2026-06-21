// Package controlplane implements OpenRails' OpenRails-owned AuthKit control
// plane (issue #224).
//
// OpenRails owns an AuthKit control plane rather than acting only as an external
// JWT verifier. In self-hosted / locked-down mode OpenRails:
//
//   - mounts only the AuthKit route groups it intentionally exposes (NOT the
//     full DefaultAPI surface),
//   - runs with public user registration and public org management disabled,
//   - bootstraps the default merchant's own AuthKit org, the OpenRails `org:`
//     permission catalog, and an initial deployment admin API key through
//     in-process AuthKit CORE calls (CreateOrg / AssignRole / MintAPIKey) — never
//     raw SQL or a private HTTP route. OpenRails defines NO org role of its own
//     (#543): the admin is made the org `owner`; the API key is permission-scoped.
//
// HARDCUT (#537): merchant-local authority lives in the caller's merchant
// AuthKit org (`org:` permissions). Cross-merchant directory authority lives in
// AuthKit platform RBAC (`platform:` permissions).
package controlplane

import (
	"context"
	"errors"
	"fmt"
	"strings"

	authcore "github.com/open-rails/authkit/core"
)

// Permission is an OpenRails permission definition. AuthKit owns the RBAC
// layers; OpenRails defines host resources inside `org:`.
type Permission = authcore.PermissionDef

// OpenRails permissions. `org:` permissions are merchant-local AuthKit org
// permissions. Customer self-service routes do not carry OpenRails `self:`
// grants: the authenticated subject is the route target.
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

// Admin-role identity.
//
// OpenRails defines/seeds NO org role of its own (#543): merchant-admin authority
// comes from AuthKit's built-in `owner` role for human admins (OwnerRole, holds
// `org:*`) and from direct permission-scoped API keys for server-to-server
// automation. `OperatorRole` survives only as a test-fixture name; production
// never defines it in an org.
const (
	// OwnerRole is AuthKit's built-in, reserved org-owner role (core's hardcoded
	// "owner"). Its `org:*` grant — seeded by AuthKit on CreateOrg — expands over
	// the OpenRails `org:` catalog, so the org owner can perform every
	// merchant-admin operation. OpenRails does NOT define this role; it only
	// ASSIGNS it to the bootstrap admin.
	OwnerRole = "owner"

	// OperatorRole is a legacy role name retained for TEST FIXTURES only. OpenRails
	// no longer seeds or declares it in any org (#543).
	OperatorRole = "openrails-operator"
)

// OperatorRolePermissions returns the full OpenRails `org:` catalog permission
// set. It is NOT a role: it is used to DIRECT-SCOPE the admin API key (a key
// carries permissions, not a role) and by test fixtures. See CatalogNames.
func OperatorRolePermissions() []string {
	return CatalogNames()
}

// EnsureRole idempotently ensures an org role exists with exactly the given
// permission set, returning the canonical role slug to mint against. AuthKit
// v0.43.0 mints API keys with a single org ROLE (not a permission bundle): the
// key's effective permissions are resolved from the role at verify time. So any
// caller that wants a bespoke permission set must first DEFINE a role carrying
// those perms, then mint with that role slug.
//
// Both steps are idempotent: DefineRole is INSERT ... ON CONFLICT DO NOTHING,
// SetRolePermissions replaces the role's permission set. perms must be concrete
// `org:` catalog tokens (no wildcards / reserved-write tokens) — the OpenRails
// catalog is, so OperatorRolePermissions() and its subsets are always valid.
func EnsureRole(ctx context.Context, core *authcore.Service, orgSlug, role string, perms []string) (string, error) {
	if core == nil {
		return "", errors.New("controlplane: nil core service")
	}
	if err := core.DefineRole(ctx, orgSlug, role); err != nil {
		return "", fmt.Errorf("controlplane: define role %q in org %q: %w", role, orgSlug, err)
	}
	if err := core.SetRolePermissions(ctx, orgSlug, role, perms); err != nil {
		return "", fmt.Errorf("controlplane: set permissions on role %q in org %q: %w", role, orgSlug, err)
	}
	return role, nil
}

// EnsureOperatorRole idempotently ensures the OpenRails operator role
// (OperatorRole) exists in the org carrying the full `org:` catalog
// (OperatorRolePermissions). It is the role an admin/deployment API key is minted
// against under the v0.43.0 role-unified mint model. Returns the role slug.
func EnsureOperatorRole(ctx context.Context, core *authcore.Service, orgSlug string) (string, error) {
	return EnsureRole(ctx, core, orgSlug, OperatorRole, OperatorRolePermissions())
}
