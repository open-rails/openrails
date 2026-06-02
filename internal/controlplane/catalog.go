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
//     `openrails.*` permission catalog, and an initial operator OAT through
//     in-process AuthKit CORE calls (CreateOrg / DefineRole / AssignRole /
//     MintOrgAccessToken) — never raw SQL or a private HTTP route.
//
// This package establishes admin authority via the operator org + permission
// catalog (the #221/#222 direction). It coordinates with #223's tenant
// primitive: the default tenant (tenant.DefaultSlug) maps to one OpenRails-owned
// AuthKit operator org.
package controlplane

import authcore "github.com/open-rails/authkit/core"

// Permission is an OpenRails permission string in the OpenRails permission
// catalog. AuthKit treats permission strings as opaque; OpenRails owns their
// meaning. The `openrails.` prefix is kept in every deployment mode so leak
// scanners and audits can identify OpenRails-issued grants.
//
// NOTE on form: issue #224 specifies the dot-form names below
// (e.g. openrails.credits.read). Issue #222 later proposes a colon-form
// resource:action vocabulary (e.g. openrails:credits:hold). The dot-form here is
// the #224 control-plane catalog; reconciling the two forms is tracked as an
// open question for #222 and intentionally NOT decided here.
type Permission = authcore.PermissionDef

// The OpenRails permission catalog (issue #224). These are the permission
// strings seeded into AuthKit and granted to the operator role.
const (
	// PermAdmin is the broad OpenRails operator/admin capability.
	PermAdmin = "openrails.admin"

	// Credit operations (server-to-server / operator).
	PermCreditsRead  = "openrails.credits.read"
	PermCreditsWrite = "openrails.credits.write"

	// Entitlement reads (enrichment, server-to-server).
	PermEntitlementsRead = "openrails.entitlements.read"

	// Catalog (products/prices) writes.
	PermCatalogWrite = "openrails.catalog.write"

	// Destructive billing operations.
	PermPaymentsRefund      = "openrails.payments.refund"
	PermSubscriptionsCancel = "openrails.subscriptions.cancel"
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
