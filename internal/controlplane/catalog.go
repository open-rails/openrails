// Package controlplane implements OpenRails' OpenRails-owned AuthKit control
// plane (issue #224).
//
// OpenRails owns an AuthKit control plane rather than acting only as an external
// JWT verifier. In self-hosted / locked-down mode OpenRails:
//
//   - mounts only the AuthKit route groups it intentionally exposes (NOT the
//     full DefaultAPI surface),
//   - runs with public user registration disabled,
//   - bootstraps the merchant permission-group, the OpenRails permission catalog
//     (`merchant:*` / `customer:*`), and an initial deployment admin API key
//     through in-process AuthKit CORE calls (CreatePermissionGroup /
//     Genesis().AssignGroupRole / MintAPIKey) — never raw SQL or a private HTTP route.
//
// HARDCUT (#567): merchant-local authority is evaluated in the caller's merchant
// permission-group using OpenRails' app permissions (`merchant:*` seller,
// `customer:*` buyer/treasury). Cross-merchant directory authority belongs to
// root/platform control, not merchant groups.
package controlplane

import (
	"github.com/open-rails/authkit"
	authcore "github.com/open-rails/authkit/embedded"

	"github.com/open-rails/openrails/permissions"
)

// Permission-group personas (#567). OpenRails declares exactly TWO flat
// top-level group types under the intrinsic `root` type:
//
//   - MerchantType — a merchant IS a top-level permission-group (child of root).
//     Staff roles owner/support/viewer; holds `merchant:*`.
//   - CustomerType — explicit hosted portal membership (child of root).
//     Roles owner/member; holds customer:* for that group. Billing customer
//     records and spend policies do not require an AuthKit customer group.
//
// A merchant group is addressed by its immutable id; OpenRails owns the merchant
// name (#1106). A customer group is addressed by the customer uuid string.
const (
	MerchantType authkit.Persona = "merchant"
	CustomerType authkit.Persona = "customer"

	// Merchant staff roles (#567). owner is auto-seeded by authkit (= `merchant:*`).
	MerchantRoleOwner   = "owner"
	MerchantRoleSupport = "support"
	MerchantRoleViewer  = "viewer"
	MerchantRoleCreator = "creator"

	// Customer roles (#567). owner is auto-seeded by authkit (= `customer:*`).
	CustomerRoleOwner  = "owner"
	CustomerRoleMember = "member"

	// Root (platform-operator) roles (#721): bounded merchant-directory bundles
	// declared on authkit's intrinsic root persona. The root `owner` (root:*) is
	// auto-seeded by authkit and covers both. No broad "superadmin" role is
	// declared beyond that owner.
	RootRoleMerchantDirectoryViewer = "merchant-directory-viewer"
	RootRoleMerchantDirectoryAdmin  = "merchant-directory-admin"
)

// CustomerGroup addresses the customer permission-group for customerID (the
// payer's own user id, #567).
func CustomerGroup(customerID string) authkit.GroupRef {
	return authkit.GroupRef{Persona: CustomerType, Instance: customerID}
}

// Groups returns the OpenRails permission-group type catalog (#567): the two
// flat top-level personas (`merchant`, `customer`) declared under `root`. Fixed
// catalogs, CustomRoles=false. authkit injects the intrinsic `root` type and
// auto-seeds each type's
// `owner` role (= `<type>:*`). Suitable for core.Config.RBAC.
func Groups() []authcore.PersonaDef {
	return []authcore.PersonaDef{
		// Root persona EXTENSION (#721): extra bounded operator roles merged onto
		// authkit's intrinsic root persona (BuildSchema merges Name==root
		// declarations; owner = root:* stays auto-seeded). These gate the
		// cross-merchant /v1/platform/merchants directory.
		{
			Name: authkit.RootPersona,
			Roles: []authcore.RoleDef{
				{
					Name:        RootRoleMerchantDirectoryViewer,
					Permissions: []string{PermRootMerchantsRead},
				},
				{
					Name: RootRoleMerchantDirectoryAdmin,
					Permissions: []string{
						PermRootMerchantsRead, PermRootMerchantsDelete, PermRootMerchantsRestore,
					},
				},
			},
		},
		{
			Name:   MerchantType,
			Parent: authkit.RootPersona,
			// authkit auto-generates the staff/credential MANAGEMENT routes from
			// these capabilities (members, api-keys, remote-applications). OpenRails
			// mounts these and builds none of them (#567).
			Capabilities: authcore.PersonaCapabilities{
				APIKeys:            true,
				RemoteApplications: true,
			},
			Roles: []authcore.RoleDef{
				// owner (= merchant:*) is auto-seeded; declared elsewhere implicitly.
				{
					Name:        MerchantRoleCreator,
					Permissions: []string{permissions.MerchantCatalogOwnRead, permissions.MerchantCatalogOwnUpdate},
				},
				{
					Name: MerchantRoleSupport,
					Permissions: []string{
						PermMerchantCustomerSettingsRead, PermMerchantCustomerSettingsUpdate,
						PermMerchantPaymentsRead, PermMerchantPaymentsRefund,
						PermMerchantInvoicesRead, PermMerchantInvoicesCollect,
						PermMerchantSubscriptionsRead, PermMerchantSubscriptionsUpdate,
						PermMerchantUsageRead, PermMerchantHostEventsRead, PermMerchantRepairAlertsRead, PermMerchantMetricsRead,
						PermMerchantDashboardUpdate,
					},
				},
				{
					Name: MerchantRoleViewer,
					// Read-only: every merchant:*:read. Finance / audit / analyst.
					Permissions: []string{
						PermMerchantSettingsRead, PermMerchantPaymentProvidersRead,
						PermMerchantCatalogRead, PermMerchantCustomerSettingsRead,
						PermMerchantPaymentsRead, PermMerchantInvoicesRead, PermMerchantSubscriptionsRead,
						PermMerchantUsageRead, PermMerchantHostEventsRead, PermMerchantRepairAlertsRead, PermMerchantMetricsRead,
					},
				},
			},
		},
		{
			// Explicit hosted customer-portal membership only. Billing
			// credentials are merchant-scoped; customer-owned machine keys
			// and signing applications have no supported receiver path.
			Name:   CustomerType,
			Parent: authkit.RootPersona,
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
// personas (#567), each owner auto-holds its own
// `<persona>:*`. `/v1/me/*` self-service needs no grant. Coarse: one permission
// per role boundary, not per route.
const (
	PermMerchantSettingsRead           = permissions.MerchantSettingsRead
	PermMerchantSettingsUpdate         = permissions.MerchantSettingsUpdate
	PermMerchantPaymentProvidersRead   = permissions.MerchantPaymentProvidersRead
	PermMerchantPaymentProvidersUpdate = permissions.MerchantPaymentProvidersUpdate
	PermMerchantCatalogRead            = permissions.MerchantCatalogRead
	PermMerchantCatalogUpdate          = permissions.MerchantCatalogUpdate
	PermMerchantCustomerSettingsRead   = permissions.MerchantCustomerSettingsRead
	PermMerchantCustomerSettingsUpdate = permissions.MerchantCustomerSettingsUpdate
	PermMerchantInvoicesRead           = permissions.MerchantInvoicesRead
	PermMerchantInvoicesUpdate         = permissions.MerchantInvoicesUpdate
	PermMerchantInvoicesCollect        = permissions.MerchantInvoicesCollect
	PermMerchantPaymentsRead           = permissions.MerchantPaymentsRead
	PermMerchantPaymentsRefund         = permissions.MerchantPaymentsRefund
	PermMerchantSubscriptionsRead      = permissions.MerchantSubscriptionsRead
	PermMerchantSubscriptionsUpdate    = permissions.MerchantSubscriptionsUpdate
	PermMerchantAdmissionsCreate       = permissions.MerchantAdmissionsCreate
	PermMerchantUsageRead              = permissions.MerchantUsageRead
	PermMerchantHostEventsRead         = permissions.MerchantHostEventsRead
	PermMerchantHostEventsAcknowledge  = permissions.MerchantHostEventsAcknowledge
	PermMerchantRepairAlertsRead       = permissions.MerchantRepairAlertsRead
	PermMerchantMetricsRead            = permissions.MerchantMetricsRead
	PermMerchantDashboardUpdate        = permissions.MerchantDashboardUpdate
	PermMerchantFindingsResolve        = permissions.MerchantFindingsResolve
	PermMerchantBillingImport          = permissions.MerchantBillingImport
	PermMerchantBillingExport          = permissions.MerchantBillingExport
	PermMerchantCreditsGrant           = permissions.MerchantCreditsGrant
	PermMerchantCreditsRevoke          = permissions.MerchantCreditsRevoke
	PermMerchantMembersRead            = permissions.MerchantMembersRead
	PermMerchantMembersManage          = permissions.MerchantMembersManage
	PermMerchantCredentialsManage      = permissions.MerchantCredentialsManage

	// --- Platform operator (root persona, #721): cross-merchant directory and
	// operational override authority checked against the singleton root group,
	// never a merchant group. `root:` is authkit's platform-operator namespace. ---
	PermRootMerchantsRead         = permissions.RootMerchantsRead
	PermRootMerchantsDelete       = permissions.RootMerchantsDelete
	PermRootMerchantsRestore      = permissions.RootMerchantsRestore
	PermRootWorkerHealthRead      = permissions.RootWorkerHealthRead
	PermRootAdminRateLimitsUnlock = permissions.RootAdminRateLimitsUnlock

	// --- Customer treasury: a customer (any payer) acting on its OWN balance (NOT
	// merchant-owner), scoped to /v1/customers/:customer_id/* (#567). Coarse: one
	// permission per role boundary, not per route. ---
	PermCustomerBalanceRead          = permissions.CustomerBalanceRead
	PermCustomerBillingUpdate        = permissions.CustomerBillingUpdate
	PermCustomerPaymentMethodsUpdate = permissions.CustomerPaymentMethodsUpdate
	PermCustomerCheckoutCreate       = permissions.CustomerCheckoutCreate

	PermCustomerSpendDelegationsRead   = permissions.CustomerSpendDelegationsRead
	PermCustomerSpendDelegationsUpdate = permissions.CustomerSpendDelegationsUpdate
)

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
	OwnerRole = authkit.OwnerRole
)
