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
// permission and is the admin authority for the operator org.
const (
	// OperatorRole is the OpenRails role seeded in the operator org that holds
	// the full OpenRails permission catalog.
	OperatorRole = "openrails-operator"
)

// OperatorRolePermissions returns the permission names granted to the operator
// role: the full OpenRails catalog.
func OperatorRolePermissions() []string {
	return CatalogNames()
}
