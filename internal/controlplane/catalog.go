// Package controlplane implements OpenRails' OpenRails-owned AuthKit control
// plane (issue #224).
//
// OpenRails owns an AuthKit control plane rather than acting only as an external
// JWT verifier. In self-hosted / locked-down mode OpenRails:
//
//   - mounts only the AuthKit route groups it intentionally exposes (NOT the
//     full DefaultAPI surface),
//   - runs with public user registration and public org management disabled,
//   - bootstraps the default merchant's own AuthKit org, the OpenRails permission
//     catalog (`merchant:*` / `customer:*`), and an initial deployment admin API
//     key through in-process AuthKit CORE calls (CreateOrg / AssignRole / MintAPIKey)
//     — never raw SQL or a private HTTP route. OpenRails defines NO org role of its
//     own (#543): the admin is made the org `owner`; the API key is permission-scoped.
//
// HARDCUT (#537): merchant-local authority is evaluated in the caller's merchant
// AuthKit org using OpenRails' app permissions (`merchant:*` seller, `customer:*`
// buyer/treasury); `org:`/`platform:` stay AuthKit-native. Cross-merchant directory
// authority is AuthKit platform RBAC (`platform:`).
package controlplane

import (
	"context"
	"errors"
	"fmt"

	authcore "github.com/open-rails/authkit/core"
)

// Permission is an OpenRails permission definition. AuthKit owns `org:`/`platform:`;
// OpenRails defines its app resources in `merchant:*` and `customer:*`.
type Permission = authcore.PermissionDef

// OpenRails permissions (#554), evaluated in the AuthKit org that owns the merchant
// (org<->merchant 1:1). `merchant:*` is the seller hat, `customer:*` the buyer hat
// (the org sharing its OWN balance); the owner auto-holds both via authkit
// OwnerOwnsAppResources (#100). `/v1/me/*` self-service needs no grant. Coarse: one
// permission per role boundary, not per route.
const (
	PermMerchantSettingsRead           = "merchant:settings:read"
	PermMerchantSettingsUpdate         = "merchant:settings:update"
	PermMerchantPaymentProvidersRead   = "merchant:payment-providers:read"
	PermMerchantPaymentProvidersUpdate = "merchant:payment-providers:update"
	PermMerchantCatalogRead            = "merchant:catalog:read"
	PermMerchantCatalogUpdate          = "merchant:catalog:update"
	PermMerchantCustomerSettingsRead   = "merchant:customer-settings:read"
	PermMerchantCustomerSettingsUpdate = "merchant:customer-settings:update"
	PermMerchantPaymentsRead           = "merchant:payments:read"
	PermMerchantPaymentsRefund         = "merchant:payments:refund"
	PermMerchantSubscriptionsRead      = "merchant:subscriptions:read"
	PermMerchantSubscriptionsUpdate    = "merchant:subscriptions:update"
	PermMerchantAdmissionsCreate       = "merchant:admissions:create"
	PermMerchantUsageRead              = "merchant:usage:read"
	PermMerchantRepairAlertsRead       = "merchant:repair-alerts:read"

	// --- Customer treasury: org acting AS a customer/payer (NOT merchant-owner),
	// scoped to /v1/orgs/:org_id/*. Personal balances are never delegable.
	// `customer:` is disjoint from `merchant:*` and AuthKit `org:*`. The payer
	// surface is split (#566) so a finance reader, a billing-mode setter, a
	// payment-methods manager, and a credit-loader are distinct grants — coarse:
	// one permission per role boundary, not per route. ---
	PermCustomerBalanceRead          = "customer:balance:read"
	PermCustomerBillingUpdate        = "customer:billing:update"
	PermCustomerPaymentMethodsUpdate = "customer:payment-methods:update"
	PermCustomerCheckoutCreate       = "customer:checkout:create"

	PermCustomerSpendDelegationsRead   = "customer:spend-delegations:read"
	PermCustomerSpendDelegationsUpdate = "customer:spend-delegations:update"
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
	{Name: PermMerchantCustomerSettingsRead, Description: "Merchant: read customer support profile, balance, transactions, entitlements, product access, and saved payment-method metadata."},
	{Name: PermMerchantCustomerSettingsUpdate, Description: "Merchant support writes: customer profile/settings, entitlement/product-access grants, balance adjustments, and credit limits."},
	{Name: PermMerchantPaymentsRead, Description: "Merchant: search and read merchant payments."},
	{Name: PermMerchantPaymentsRefund, Description: "Merchant: refund a payment."},
	{Name: PermMerchantSubscriptionsRead, Description: "Merchant: search and read subscriptions."},
	{Name: PermMerchantSubscriptionsUpdate, Description: "Merchant: cancel or update a subscription."},
	{Name: PermMerchantAdmissionsCreate, Description: "Merchant: admission lifecycle — admit, capture, release, and report wasted spend (machine hot path)."},
	{Name: PermMerchantUsageRead, Description: "Merchant: read usage/revenue rollups and analytics metrics."},
	{Name: PermMerchantRepairAlertsRead, Description: "Merchant: read ledger/provider repair alerts."},
	{Name: PermCustomerBalanceRead, Description: "Customer: read the org's balance, transactions, usage, payments, and invoices."},
	{Name: PermCustomerBillingUpdate, Description: "Customer: set the org's billing mode (prepaid/arrears) and self-imposed spend caps."},
	{Name: PermCustomerPaymentMethodsUpdate, Description: "Customer: manage the org's saved payment methods and Stripe billing portal."},
	{Name: PermCustomerCheckoutCreate, Description: "Customer: pre-pay / load credits onto the org balance via checkout."},
	{Name: PermCustomerSpendDelegationsRead, Description: "Customer: read the org's balance-sharing (spend-delegation) policy."},
	{Name: PermCustomerSpendDelegationsUpdate, Description: "Customer: replace the org's balance-sharing (spend-delegation) policy."},
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
	// "owner"). With OwnerOwnsAppResources enabled, AuthKit expands the owner's
	// `org:*` grant over OpenRails app namespaces (`merchant:*`, `customer:*`), so
	// the org owner can perform every merchant/customer-treasury operation.
	// OpenRails does NOT define this role; it only ASSIGNS it to the bootstrap admin.
	OwnerRole = "owner"

	// OperatorRole is a legacy role name retained for TEST FIXTURES only. OpenRails
	// no longer seeds or declares it in any org (#543).
	OperatorRole = "openrails-operator"
)

// OperatorRolePermissions returns the full OpenRails app catalog permission
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
// OpenRails catalog tokens (no wildcards / reserved-write tokens), so
// OperatorRolePermissions() and its subsets are always valid.
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
// (OperatorRole) exists in the org carrying the full OpenRails app catalog
// (OperatorRolePermissions). It is the role an admin/deployment API key is minted
// against under the v0.43.0 role-unified mint model. Returns the role slug.
func EnsureOperatorRole(ctx context.Context, core *authcore.Service, orgSlug string) (string, error) {
	return EnsureRole(ctx, core, orgSlug, OperatorRole, OperatorRolePermissions())
}
