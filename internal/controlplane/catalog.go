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

// OpenRails permissions (#554). Merchant-resource permissions live in the OpenRails
// `merchant:` namespace: they are granted/evaluated in the AuthKit org that OWNS
// the merchant (org<->merchant 1:1), and the org owner auto-holds `merchant:*` via
// authkit OwnerOwnsAppResources (authkit#100). Org-customer treasury permissions
// live in `org:spend-delegations:*` (the payer org sharing its OWN balance).
// Customer self-service (`/v1/me/*`) carries no OpenRails grant: the authenticated
// subject is the route target. The catalog is COARSE — one permission per real
// role boundary, not per route.
const (
	// PermAdmin is a deprecated source-compatibility alias for the merchant-owner
	// apex grant. It is NOT a magic permission and does not satisfy arbitrary checks.
	PermAdmin = authcore.OrgOwnerGrant

	// --- Canonical #554 merchant catalog ---
	PermMerchantSettingsRead           = "merchant:settings:read"
	PermMerchantSettingsUpdate         = "merchant:settings:update"
	PermMerchantPaymentProvidersRead   = "merchant:payment-providers:read"
	PermMerchantPaymentProvidersUpdate = "merchant:payment-providers:update"
	PermMerchantCatalogRead            = "merchant:catalog:read"
	PermMerchantCatalogUpdate          = "merchant:catalog:update"
	PermMerchantCustomersRead          = "merchant:customers:read"
	PermMerchantCustomersUpdate        = "merchant:customers:update"
	PermMerchantPaymentsRead           = "merchant:payments:read"
	PermMerchantPaymentsRefund         = "merchant:payments:refund"
	PermMerchantSubscriptionsRead      = "merchant:subscriptions:read"
	PermMerchantSubscriptionsUpdate    = "merchant:subscriptions:update"
	PermMerchantAdmissionsCreate       = "merchant:admissions:create"
	PermMerchantUsageRead              = "merchant:usage:read"
	PermMerchantRepairAlertsRead       = "merchant:repair-alerts:read"

	// --- Org-customer treasury (NOT merchant-owner): scoped to the org named in
	// /v1/orgs/:org_id/spend-delegations. A personal customer balance is never
	// delegable. ---
	PermOrgSpendDelegationsRead   = "org:spend-delegations:read"
	PermOrgSpendDelegationsUpdate = "org:spend-delegations:update"

	// --- Deprecated source-compat aliases (#554): the old constant identifiers now
	// resolve to their collapsed `merchant:*` value, so existing routes/tests
	// compile unchanged while every gate STRING becomes merchant:*. New code uses
	// the canonical names above. The precise per-route fine-graining + alias
	// removal land with the /v1/service -> /v1/merchant move (#555). ---
	PermCreditsRead                = PermMerchantCustomersRead
	PermCreditsWrite               = PermMerchantCustomersUpdate
	PermCreditsSpend               = PermMerchantAdmissionsCreate
	PermEntitlementsRead           = PermMerchantCustomersRead
	PermCatalogWrite               = PermMerchantCatalogUpdate
	PermPaymentsRefund             = PermMerchantPaymentsRefund
	PermSubscriptionsCancel        = PermMerchantSubscriptionsUpdate
	PermMerchantBillingRead        = PermMerchantCustomersRead
	PermMerchantEntitlementsWrite  = PermMerchantCustomersUpdate
	PermMerchantProductAccessWrite = PermMerchantCustomersUpdate
	PermMerchantCreditsWrite       = PermMerchantCustomersUpdate
	PermMerchantPaymentsWrite      = PermMerchantCustomersUpdate
	PermMerchantSubscriptionsWrite = PermMerchantSubscriptionsUpdate
	PermMerchantConfigurationRead  = PermMerchantSettingsRead
	PermMerchantConfigurationWrite = PermMerchantSettingsUpdate
	PermMerchantSecretsList        = PermMerchantPaymentProvidersRead
	PermMerchantSecretsWrite       = PermMerchantPaymentProvidersUpdate
	PermMerchantSecretsDelete      = PermMerchantPaymentProvidersUpdate
	PermMerchantSecretsTest        = PermMerchantPaymentProvidersUpdate
	PermMerchantMetricsRead        = PermMerchantUsageRead
)

// catalogEntries is the canonical ordered list of OpenRails permissions with
// human-readable descriptions. Keep ordering stable so seeding is deterministic.
var catalogEntries = []Permission{
	{Name: PermMerchantSettingsRead, Description: "Merchant: read merchant-owned settings (display, checkout, admission policy)."},
	{Name: PermMerchantSettingsUpdate, Description: "Merchant: update merchant-owned settings."},
	{Name: PermMerchantPaymentProvidersRead, Description: "Merchant: read configured payment-provider status (never plaintext)."},
	{Name: PermMerchantPaymentProvidersUpdate, Description: "Merchant: configure/disable payment providers; credentials validated before storage."},
	{Name: PermMerchantCatalogRead, Description: "Merchant: read catalog products, prices, and drift."},
	{Name: PermMerchantCatalogUpdate, Description: "Merchant: create/update products and prices, publish catalog, refresh drift."},
	{Name: PermMerchantCustomersRead, Description: "Merchant: read customer profile, balance, transactions, entitlements, product access, payments, and subscriptions."},
	{Name: PermMerchantCustomersUpdate, Description: "Merchant support writes: entitlement/product-access grants, balance adjustments, credit limits, off-channel payments, payment-method removal."},
	{Name: PermMerchantPaymentsRead, Description: "Merchant: search and read merchant payments."},
	{Name: PermMerchantPaymentsRefund, Description: "Merchant: refund a payment."},
	{Name: PermMerchantSubscriptionsRead, Description: "Merchant: search and read subscriptions."},
	{Name: PermMerchantSubscriptionsUpdate, Description: "Merchant: cancel or update a subscription."},
	{Name: PermMerchantAdmissionsCreate, Description: "Merchant: admission lifecycle — admit, capture, release, and report wasted spend (machine hot path)."},
	{Name: PermMerchantUsageRead, Description: "Merchant: read usage/revenue rollups and analytics metrics."},
	{Name: PermMerchantRepairAlertsRead, Description: "Merchant: read ledger/provider repair alerts."},
	{Name: PermOrgSpendDelegationsRead, Description: "Org customer: read the org's balance-sharing (spend-delegation) policy."},
	{Name: PermOrgSpendDelegationsUpdate, Description: "Org customer: replace the org's balance-sharing (spend-delegation) policy."},
}

// merchantCatalog is the set of merchant-admin permissions accepted on a FEDERATED
// merchant-signed delegated access token for the `/v1/admin/*` surface
// (issue #259). Exact-match, no wildcard. A token presenting any permission
// outside merchantCatalog is rejected: browser tokens must never carry
// operator/server-to-server grants.
var merchantCatalog = map[string]struct{}{
	PermMerchantSettingsRead:           {},
	PermMerchantSettingsUpdate:         {},
	PermMerchantPaymentProvidersRead:   {},
	PermMerchantPaymentProvidersUpdate: {},
	PermMerchantCatalogRead:            {},
	PermMerchantCatalogUpdate:          {},
	PermMerchantCustomersRead:          {},
	PermMerchantCustomersUpdate:        {},
	PermMerchantPaymentsRead:           {},
	PermMerchantPaymentsRefund:         {},
	PermMerchantSubscriptionsRead:      {},
	PermMerchantSubscriptionsUpdate:    {},
	PermMerchantUsageRead:              {},
	PermMerchantRepairAlertsRead:       {},
	PermOrgSpendDelegationsRead:        {},
	PermOrgSpendDelegationsUpdate:      {},
}

// MerchantCatalogNames returns the browser-safe merchant/treasury permission names
// in catalog order. It EXCLUDES merchant:admissions:create — the machine hot-path
// admit grant is never carried on a browser delegated token.
func MerchantCatalogNames() []string {
	return []string{
		PermMerchantSettingsRead,
		PermMerchantSettingsUpdate,
		PermMerchantPaymentProvidersRead,
		PermMerchantPaymentProvidersUpdate,
		PermMerchantCatalogRead,
		PermMerchantCatalogUpdate,
		PermMerchantCustomersRead,
		PermMerchantCustomersUpdate,
		PermMerchantPaymentsRead,
		PermMerchantPaymentsRefund,
		PermMerchantSubscriptionsRead,
		PermMerchantSubscriptionsUpdate,
		PermMerchantUsageRead,
		PermMerchantRepairAlertsRead,
		PermOrgSpendDelegationsRead,
		PermOrgSpendDelegationsUpdate,
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
