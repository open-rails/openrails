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
	"context"

	authcore "github.com/open-rails/authkit/embedded"

	"github.com/open-rails/openrails/permissions"
	"github.com/open-rails/openrails/pkg/merchant"
)

// Permission-group personas (#567). OpenRails declares exactly TWO flat
// top-level group types under the intrinsic `root` type:
//
//   - MerchantType — a merchant IS a top-level permission-group (child of root).
//     Staff roles owner/support/viewer; holds `merchant:*`.
//   - CustomerType — every payer is a `customer` group (child of root). Roles
//     owner/member; holds `customer:*`. Universal: every customer can delegate
//     the spending of its balance.
//
// Both are addressed by (type, resourceRef): the merchant slug for MerchantType,
// the customer uuid string for CustomerType.
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

	// Root (platform-operator) roles (#721): bounded merchant-directory bundles
	// declared on authkit's intrinsic root persona. The root `owner` (root:*) is
	// auto-seeded by authkit and covers both. No broad "superadmin" role is
	// declared beyond that owner.
	RootRoleMerchantDirectoryViewer = "merchant-directory-viewer"
	RootRoleMerchantDirectoryAdmin  = "merchant-directory-admin"
)

// Groups returns the OpenRails permission-group type catalog (#567): the two
// flat top-level personas (`merchant`, `customer`) declared under `root`. Fixed
// catalogs, CustomRoles=false (custom roles + deep hierarchy are tensorhub's
// domain). authkit injects the intrinsic `root` type and auto-seeds each type's
// `owner` role (= `<type>:*`). Suitable for core.Config.RBAC.
func Groups() []authcore.PersonaDef {
	return []authcore.PersonaDef{
		// Root persona EXTENSION (#721): extra bounded operator roles merged onto
		// authkit's intrinsic root persona (BuildSchema merges Name==root
		// declarations; owner = root:* stays auto-seeded). These gate the
		// cross-merchant /v1/platform/merchants directory.
		{
			Name: authcore.RootPersona,
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
			Parent: authcore.RootPersona,
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
					Name: MerchantRoleSupport,
					Permissions: []string{
						PermMerchantCustomerSettingsRead, PermMerchantCustomerSettingsUpdate,
						PermMerchantPaymentsRead, PermMerchantPaymentsRefund,
						PermMerchantSubscriptionsRead, PermMerchantSubscriptionsUpdate,
						PermMerchantUsageRead, PermMerchantRepairAlertsRead, PermMerchantMetricsRead,
						PermMerchantDashboardUpdate,
					},
				},
				{
					Name: MerchantRoleViewer,
					// Read-only: every merchant:*:read. Finance / audit / analyst.
					Permissions: []string{
						PermMerchantSettingsRead, PermMerchantPaymentProvidersRead,
						PermMerchantCatalogRead, PermMerchantCustomerSettingsRead,
						PermMerchantPaymentsRead, PermMerchantSubscriptionsRead,
						PermMerchantUsageRead, PermMerchantRepairAlertsRead, PermMerchantMetricsRead,
					},
				},
			},
		},
		{
			Name:   CustomerType,
			Parent: authcore.RootPersona,
			Capabilities: authcore.PersonaCapabilities{
				APIKeys:            true,
				RemoteApplications: true,
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

// MerchantCreationConfig opts the merchant persona into authkit's generated
// instance-creation path (ak#263, or#914): user-claimed merchant slugs go
// through CreateInstanceForSubject, so the slug pattern, reserved-slug
// escalation, and the host admission (cost) gate all apply to hosted
// "registration is provisioning" flows. Standalone/locked posture never
// enables this; hosted products opt in through
// pkg/embedded/controlplane.AttachOptions.MerchantCreation.
type MerchantCreationConfig struct {
	// ReservedSlugs are reserved IN ADDITION to merchant.ReservedHostedSlugs
	// (the advisory hosted default, always included). Exact lowercase slugs.
	ReservedSlugs []string
	// ReservedEscalationRole names the root-group role allowed to claim
	// reserved slugs through the creation path. Empty = reserved slugs are not
	// claimable through it at all (operator paths — Bootstrap, manifests —
	// are unaffected either way).
	ReservedEscalationRole string
	// SlugPattern further restricts creatable slugs beyond authkit's built-in
	// instance-slug rule: an unanchored regexp source, anchored at schema
	// build. Empty = built-in rule only.
	SlugPattern string
	// Admission is the host COST gate (authkit's WithInstanceAdmission seam,
	// ak#263): consulted with the normalized candidate slug and the creating
	// user id before any merchant group is created through the creation path.
	// Return a non-nil error to refuse. nil = allow. Velocity limits (per-IP +
	// per-user) are authkit's own and apply on its generated HTTP route;
	// in-process creation (embcp.ProvisionMerchant) is gated by everything
	// except those HTTP-layer limits.
	Admission func(ctx context.Context, instanceSlug, ownerUserID string) error
}

// withMerchantCreation returns the Groups() catalog with the merchant persona's
// generated-creation route enabled per cfg (or#914).
func withMerchantCreation(defs []authcore.PersonaDef, cfg MerchantCreationConfig) []authcore.PersonaDef {
	reserved := append(append([]string{}, merchant.ReservedHostedSlugs...), cfg.ReservedSlugs...)
	for i := range defs {
		if defs[i].Name != MerchantType {
			continue
		}
		defs[i].Creation = authcore.InstanceCreationDef{
			Enabled:                true,
			SlugPattern:            cfg.SlugPattern,
			ReservedSlugs:          reserved,
			ReservedEscalationRole: cfg.ReservedEscalationRole,
		}
	}
	return defs
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
	PermMerchantPaymentsRead           = permissions.MerchantPaymentsRead
	PermMerchantPaymentsRefund         = permissions.MerchantPaymentsRefund
	PermMerchantSubscriptionsRead      = permissions.MerchantSubscriptionsRead
	PermMerchantSubscriptionsUpdate    = permissions.MerchantSubscriptionsUpdate
	PermMerchantAdmissionsCreate       = permissions.MerchantAdmissionsCreate
	PermMerchantUsageRead              = permissions.MerchantUsageRead
	PermMerchantRepairAlertsRead       = permissions.MerchantRepairAlertsRead
	PermMerchantMetricsRead            = permissions.MerchantMetricsRead
	PermMerchantDashboardUpdate        = permissions.MerchantDashboardUpdate
	PermMerchantFindingsResolve        = permissions.MerchantFindingsResolve
	PermMerchantBillingImport          = permissions.MerchantBillingImport
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
	OwnerRole = authcore.OwnerRoleName
)
