// Package controlplane implements OpenRails' OpenRails-owned AuthKit control
// plane (issue #224).
//
// OpenRails owns an AuthKit control plane rather than acting only as an external
// JWT verifier. In self-hosted / locked-down mode OpenRails:
//
//   - mounts only the AuthKit route groups it intentionally exposes (NOT the
//     full DefaultAPI surface),
//   - runs with public user registration and public org management disabled,
//   - bootstraps the default tenant's operator org, OpenRails roles, the
//     `openrails:*` permission catalog, and an initial operator OAT through
//     in-process AuthKit CORE calls (CreateOrg / DefineRole / AssignRole /
//     MintOrgAccessToken) — never raw SQL or a private HTTP route.
//
// This package establishes admin authority via the operator org + permission
// catalog (the #221/#222 direction). It coordinates with #223's tenant
// primitive: the default tenant (tenant.DefaultSlug) maps to one OpenRails-owned
// AuthKit operator org.
package controlplane

import (
	"strings"

	authcore "github.com/open-rails/authkit/core"
)

// Permission is an OpenRails permission string in the OpenRails permission
// catalog. AuthKit treats permission strings as opaque; OpenRails owns their
// meaning. The `openrails:` prefix is kept in every deployment mode so leak
// scanners and audits can identify OpenRails-issued grants.
//
// CANONICAL FORM (reconciled for issue #222): OpenRails permissions use the
// COLON-form `openrails:<resource>:<action>` vocabulary (e.g.
// `openrails:credits:hold`). Issue #224 originally seeded a dot-form catalog
// (e.g. openrails.credits.read); #222's STEP 3 requires picking ONE canonical
// form and applying it everywhere. We adopt the colon-form that #222's permission
// catalog task spells out, and this #224 control-plane catalog is updated to
// match. There is exactly one permission vocabulary in OpenRails.
type Permission = authcore.PermissionDef

// The OpenRails permission catalog. These are the permission strings seeded into
// AuthKit and granted to the operator role. Colon-form `openrails:resource:action`
// is the single canonical vocabulary (issue #222).
const (
	// PermAdmin is the broad OpenRails operator/admin capability.
	PermAdmin = "openrails:admin"

	// Credit operations (server-to-server / operator). reserve/hold, capture,
	// release, withdraw, deposit, and balance/transaction reads are gated by
	// these two coarse capabilities (write covers all mutating credit ops).
	PermCreditsRead  = "openrails:credits:read"
	PermCreditsWrite = "openrails:credits:write"

	// Entitlement reads (enrichment, server-to-server).
	PermEntitlementsRead = "openrails:entitlements:read"

	// Catalog (products/prices) writes.
	PermCatalogWrite = "openrails:catalog:write"

	// Destructive billing operations.
	PermPaymentsRefund      = "openrails:payments:refund"
	PermSubscriptionsCancel = "openrails:subscriptions:cancel"

	// Self-service (browser-direct) permissions. These gate the tenant-scoped
	// self-service surface (`/v1/self/*`) reached with a DELEGATED ACCESS TOKEN
	// minted by a tenant's host frontend (issue #222 foundation for the browser
	// tier: doujins #253 / hentai0 #142 / cozy-art #46). Unlike the coarse
	// server-to-server OAT permissions above, `openrails:self:*` authorizes a
	// human end-user (the token's `delegated_sub`) to manage ONLY their own
	// billing — never another user's and never operator/admin surfaces.
	PermSelfBillingRead        = "openrails:self:billing:read"
	PermSelfCheckoutCreate     = "openrails:self:checkout:create"
	PermSelfSubscriptionCancel = "openrails:self:subscriptions:cancel"
	PermSelfPaymentMethods     = "openrails:self:payment-methods:manage"

	// DEPRECATED (issue #259): PermSelfMint backs the central mint round-trip,
	// superseded by federated tenant self-signing (POST /v1/service/tenant/issuers
	// + local minting). Retained for the dual-trust migration window; removed once
	// all tenants self-sign.
	//
	// PermSelfMint authorizes a server-to-server OAT caller (a tenant's host
	// backend, e.g. doujins/hentai0) to MINT short-lived, user-scoped delegated
	// access tokens for its OWN tenant, to hand to a browser for the
	// `openrails:self:*` surface (issue #222 browser tier). It is an
	// operator/server-to-server permission carried by OATs — NOT a browser
	// self-permission — so it is deliberately NOT part of selfCatalog: a minted
	// browser token can never itself carry the mint capability. The minting tenant
	// is always the CALLER's OAT tenant, so this permission can never mint
	// cross-tenant.
	PermSelfMint = "openrails:self:mint"

	// Tenant-admin (browser-direct) permissions (issue #259). These gate the
	// `/v1/tenant-admin/users/:user_id/*` surface reached with a FEDERATED,
	// TENANT-SIGNED delegated access token whose `delegated_sub` is the ACTING
	// ADMIN. Unlike `openrails:self:*` (which act on the holder's OWN billing),
	// a `openrails:tenant:*` token acts on ANY user WITHIN the token's tenant via
	// the `:user_id` path param (scoped to that tenant; cross-tenant targets are
	// rejected). The minting TENANT decides who is an admin (it signs the token);
	// OpenRails trusts the signature + this catalog.
	//
	// DECISION (owner): permissions are EXPLICIT, exact-match, NO wildcard. A full
	// admin token lists ALL FIVE; a read-only/support admin lists only
	// PermTenantBillingRead. There is deliberately NO `openrails:tenant:*` wildcard
	// and NO coarse `openrails:tenant:admin` super-grant — so any future-added
	// tenant perm does NOT auto-grant to existing admin tokens (the minting tenant
	// must opt each admin in explicitly), matching how `openrails:self:*` works.
	PermTenantBillingRead        = "openrails:tenant:billing:read"
	PermTenantEntitlementsWrite  = "openrails:tenant:entitlements:write"
	PermTenantCreditsWrite       = "openrails:tenant:credits:write"
	PermTenantPaymentsWrite      = "openrails:tenant:payments:write"
	PermTenantSubscriptionsWrite = "openrails:tenant:subscriptions:write"

	// PermPlatformSuperadmin is the PLATFORM-level cross-tenant administrative
	// capability (issue #226), DISTINCT from PermAdmin. PermAdmin is per-tenant
	// operator authority: it is held by an operator org and only governs THAT
	// tenant. PermPlatformSuperadmin is held by a SEPARATE platform org/role
	// (cfg.Auth.ControlPlane.PlatformOrgSlug) and authorizes managed-hosting
	// platform operators to administer ANY tenant via the control plane:
	// the cross-tenant `/v1/platform/*` surface, cross-tenant search, platform
	// metrics, and break-glass elevation. A tenant operator admin does NOT hold
	// this permission (it is never seeded into operator orgs), so it cannot pass
	// the platform-superadmin gate. It is deliberately NOT part of selfCatalog:
	// a browser delegated token can never carry it.
	PermPlatformSuperadmin = "openrails:platform:superadmin"
)

// catalogEntries is the canonical ordered list of OpenRails permissions with
// human-readable descriptions. Keep ordering stable so seeding is deterministic.
var catalogEntries = []Permission{
	{Name: PermAdmin, Description: "Full OpenRails operator/admin authority for the deployment."},
	{Name: PermCreditsRead, Description: "Read credit balances and transactions."},
	{Name: PermCreditsWrite, Description: "Deposit, withdraw, hold, capture, and release credits."},
	{Name: PermEntitlementsRead, Description: "Read/enrich entitlements."},
	{Name: PermCatalogWrite, Description: "Create and update products and prices."},
	{Name: PermPaymentsRefund, Description: "Refund payments."},
	{Name: PermSubscriptionsCancel, Description: "Cancel subscriptions on behalf of the operator."},
	{Name: PermSelfBillingRead, Description: "Self-service: read your own balance, credits, transactions, subscriptions, and payment history."},
	{Name: PermSelfCheckoutCreate, Description: "Self-service: create your own checkout sessions."},
	{Name: PermSelfSubscriptionCancel, Description: "Self-service: cancel your own subscriptions."},
	{Name: PermSelfPaymentMethods, Description: "Self-service: manage your own payment methods."},
	{Name: PermSelfMint, Description: "Mint short-lived, user-scoped delegated access tokens for your own tenant (server-to-server, browser tier)."},
	{Name: PermPlatformSuperadmin, Description: "Platform superadmin: administer ANY tenant across the managed-hosting platform (cross-tenant control plane). Seeded ONLY to the platform org, never to per-tenant operator orgs (issue #226)."},
}

// selfCatalog is the set of self-service permissions accepted on delegated
// access tokens for the browser-direct `/v1/self/*` surface. A delegated token
// presenting any permission OUTSIDE this set is rejected: browser tokens must
// not carry operator/server-to-server grants.
var selfCatalog = map[string]struct{}{
	PermSelfBillingRead:        {},
	PermSelfCheckoutCreate:     {},
	PermSelfSubscriptionCancel: {},
	PermSelfPaymentMethods:     {},
}

// SelfCatalogNames returns the self-service permission names in catalog order.
func SelfCatalogNames() []string {
	return []string{
		PermSelfBillingRead,
		PermSelfCheckoutCreate,
		PermSelfSubscriptionCancel,
		PermSelfPaymentMethods,
	}
}

// IsSelfPermission reports whether perm is a known self-service permission.
func IsSelfPermission(perm string) bool {
	_, ok := selfCatalog[strings.TrimSpace(perm)]
	return ok
}

// tenantCatalog is the set of tenant-admin permissions accepted on a FEDERATED
// tenant-signed delegated access token for the `/v1/tenant-admin/*` surface
// (issue #259). Exact-match, no wildcard. A token presenting any permission
// outside selfCatalog ∪ tenantCatalog is rejected: browser tokens must never
// carry operator/server-to-server grants.
var tenantCatalog = map[string]struct{}{
	PermTenantBillingRead:        {},
	PermTenantEntitlementsWrite:  {},
	PermTenantCreditsWrite:       {},
	PermTenantPaymentsWrite:      {},
	PermTenantSubscriptionsWrite: {},
}

// TenantCatalogNames returns the tenant-admin permission names in catalog order.
func TenantCatalogNames() []string {
	return []string{
		PermTenantBillingRead,
		PermTenantEntitlementsWrite,
		PermTenantCreditsWrite,
		PermTenantPaymentsWrite,
		PermTenantSubscriptionsWrite,
	}
}

// IsTenantPermission reports whether perm is a known tenant-admin permission.
func IsTenantPermission(perm string) bool {
	_, ok := tenantCatalog[strings.TrimSpace(perm)]
	return ok
}

// IsDelegatedPermission reports whether perm is acceptable on ANY browser-direct
// delegated access token — i.e. a self-service OR tenant-admin permission. This
// is the SOLE allowlist the federated verifier enforces (issue #259): every
// `permissions` entry must pass this, so service/operator grants
// (`openrails:credits:*`, `openrails:catalog:write`, `openrails:admin`,
// `openrails:platform:superadmin`, ...) are hard-rejected by exclusion.
func IsDelegatedPermission(perm string) bool {
	return IsSelfPermission(perm) || IsTenantPermission(perm)
}

// Catalog returns the OpenRails permission catalog as AuthKit PermissionDefs,
// suitable for core.Config.PermissionCatalog. The returned slice is a copy so
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

// Operator-role identity. The operator role is granted every OpenRails
// permission EXCEPT the platform-superadmin capability, and is the admin
// authority for ONE tenant's operator org.
const (
	// OperatorRole is the OpenRails role seeded in a tenant's operator org. It
	// holds the full OpenRails per-tenant catalog but NOT PermPlatformSuperadmin
	// (which is cross-tenant platform authority, seeded only to PlatformRole).
	OperatorRole = "openrails-operator"

	// PlatformRole is the OpenRails role seeded in the SEPARATE platform org
	// (cfg.Auth.ControlPlane.PlatformOrgSlug, issue #226). It holds
	// PermPlatformSuperadmin — the cross-tenant managed-hosting authority — and
	// is the only role granted that permission. It is distinct from OperatorRole
	// so that a tenant operator admin can never reach the platform surface.
	PlatformRole = "openrails-platform-superadmin"
)

// OperatorRolePermissions returns the permission names granted to a tenant's
// operator role: the full OpenRails catalog EXCEPT PermPlatformSuperadmin. The
// platform-superadmin capability is cross-tenant and must never be carried by a
// per-tenant operator org (issue #226).
func OperatorRolePermissions() []string {
	all := CatalogNames()
	out := make([]string, 0, len(all))
	for _, p := range all {
		if p == PermPlatformSuperadmin {
			continue
		}
		out = append(out, p)
	}
	return out
}

// PlatformRolePermissions returns the permission names granted to the platform
// superadmin role: ONLY PermPlatformSuperadmin (issue #226). The platform role
// is deliberately narrow — it is the cross-tenant admin gate, not a grab-bag of
// per-tenant capabilities. Per-tenant mutations a superadmin performs go through
// the control plane and are audited in billing.platform_audit.
func PlatformRolePermissions() []string {
	return []string{PermPlatformSuperadmin}
}
