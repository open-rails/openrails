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
	"strings"

	authcore "github.com/open-rails/authkit/core"
)

// Permission is an OpenRails permission definition. AuthKit owns `root:`;
// OpenRails defines its app resources in `merchant:*` and `customer:*`.
type Permission = authcore.PermissionDef

// Permission-group personas (#567). OpenRails declares exactly TWO flat
// top-level group types under the intrinsic `root` type:
//
//   - MerchantType — a merchant IS a top-level permission-group (child of root,
//     no parent org). Staff roles owner/support/viewer; holds `merchant:*`.
//   - CustomerType — every payer is a `customer` group (child of root). Roles
//     owner/member; holds `customer:*`. Universal — there is NO org-vs-individual
//     split; every customer can delegate the spending of its balance.
//
// Both are addressed by (type, resourceRef): the merchant slug for MerchantType,
// the customer uuid string for CustomerType. There is NO `org` persona.
const (
	MerchantType = "merchant"
	CustomerType = "customer"

	// Merchant staff roles (#567). owner is auto-seeded by authkit (= `merchant:*`).
	MerchantRoleOwner   = "owner"
	MerchantRoleSupport = "support"
	MerchantRoleViewer  = "viewer"

	// Customer roles (#567). owner is auto-seeded by authkit (= `customer:*`).
	CustomerRoleOwner  = "owner"
	CustomerRoleMember = "member"
)

// Groups returns the OpenRails permission-group type catalog (#567): the two
// flat top-level personas (`merchant`, `customer`) declared under `root`. Fixed
// catalogs, AllowCustomRoles=false (custom roles + deep hierarchy are tensorhub's
// domain). authkit injects the intrinsic `root` type and auto-seeds each type's
// `owner` role (= `<type>:*`). Suitable for core.Config.RBAC.Groups.
func Groups() []authcore.GroupTypeDef {
	return []authcore.GroupTypeDef{
		{
			Name:             MerchantType,
			AllowedParents:   []string{authcore.RootType},
			AllowCustomRoles: false,
			// authkit auto-generates the staff/credential MANAGEMENT routes from
			// this profile (members, api-keys, remote-applications). OpenRails
			// mounts these and builds none of them (#567).
			Routes: authcore.ManagementProfile{
				MemberAssignment:      true,
				APIKeyMinting:         true,
				RemoteAppRegistration: true,
			},
			Roles: []authcore.RoleDef{
				// owner (= merchant:*) is auto-seeded; declared elsewhere implicitly.
				{
					Name: MerchantRoleSupport,
					Permissions: []string{
						PermMerchantCustomerSettingsRead, PermMerchantCustomerSettingsUpdate,
						PermMerchantPaymentsRead, PermMerchantPaymentsRefund,
						PermMerchantSubscriptionsRead, PermMerchantSubscriptionsUpdate,
						PermMerchantUsageRead, PermMerchantRepairAlertsRead,
					},
				},
				{
					Name: MerchantRoleViewer,
					// Read-only: every merchant:*:read. Finance / audit / analyst.
					Permissions: []string{
						PermMerchantSettingsRead, PermMerchantPaymentProvidersRead,
						PermMerchantCatalogRead, PermMerchantCustomerSettingsRead,
						PermMerchantPaymentsRead, PermMerchantSubscriptionsRead,
						PermMerchantUsageRead, PermMerchantRepairAlertsRead,
					},
				},
			},
		},
		{
			Name:             CustomerType,
			AllowedParents:   []string{authcore.RootType},
			AllowCustomRoles: false,
			Routes: authcore.ManagementProfile{
				MemberAssignment: true,
				APIKeyMinting:    true,
			},
			Roles: []authcore.RoleDef{
				// owner (= customer:*) is auto-seeded.
				{
					Name: CustomerRoleMember,
					// A delegated spender: read-only on the balance surface; spend is
					// bounded by the spend-delegation budget window assigned to them.
					Permissions: []string{
						PermCustomerBalanceRead,
						PermCustomerSpendDelegationsRead,
					},
				},
			},
		},
	}
}

// OpenRails permissions (#554). `merchant:*` is the seller hat (top-level
// `merchant` permission-group), `customer:*` the buyer hat (top-level `customer`
// permission-group sharing its OWN balance); the two are DISJOINT top-level
// personas (#567 — no org↔merchant coupling), each owner auto-holds its own
// `<persona>:*`. `/v1/me/*` self-service needs no grant. Coarse: one permission
// per role boundary, not per route.
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

	// --- Customer treasury: a customer (any payer) acting on its OWN balance (NOT
	// merchant-owner), scoped to /v1/customers/:customer_id/* (#567). Every
	// customer can delegate spend of its balance — there is no org-vs-individual
	// distinction. `customer:*` is a top-level persona, disjoint from `merchant:*`.
	// The payer surface is split (#566) so a finance reader, a billing-mode setter,
	// a payment-methods manager, and a credit-loader are distinct grants — coarse:
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
	{Name: PermCustomerBalanceRead, Description: "Customer: read the customer's balance, transactions, usage, payments, and invoices."},
	{Name: PermCustomerBillingUpdate, Description: "Customer: set the customer's billing mode (prepaid/arrears) and self-imposed spend caps."},
	{Name: PermCustomerPaymentMethodsUpdate, Description: "Customer: manage the customer's saved payment methods and Stripe billing portal."},
	{Name: PermCustomerCheckoutCreate, Description: "Customer: pre-pay / load credits onto the customer balance via checkout."},
	{Name: PermCustomerSpendDelegationsRead, Description: "Customer: read the customer's balance-sharing (spend-delegation) policy."},
	{Name: PermCustomerSpendDelegationsUpdate, Description: "Customer: replace the customer's balance-sharing (spend-delegation) policy."},
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
// HARD CUT (#567): under the permission-group model OpenRails declares the
// `merchant`/`customer` type catalogs (see Groups); authkit auto-seeds each
// type's `owner` role (= `<type>:*`). A merchant admin is the merchant group
// `owner`; server-to-server automation mints an API key under the merchant group
// against the `owner` role (which resolves to `merchant:*` at verify time).
const (
	// OwnerRole is the auto-seeded owner role authkit ships for every group type
	// (= `<type>:*`). The merchant `owner` holds `merchant:*`; the customer
	// `owner` holds `customer:*`. OpenRails does NOT define this role; it only
	// ASSIGNS it (the bootstrap admin) or MINTS against it.
	OwnerRole = authcore.OwnerRoleName
)

// MerchantOwnerRolePermissions returns the full `merchant:*` catalog permission
// set — what the merchant `owner` role resolves to. Used by test fixtures and to
// describe the deployment admin key's authority. See CatalogNames.
func MerchantOwnerRolePermissions() []string {
	out := make([]string, 0, len(catalogEntries))
	for _, e := range catalogEntries {
		if strings.HasPrefix(e.Name, MerchantType+":") {
			out = append(out, e.Name)
		}
	}
	return out
}
