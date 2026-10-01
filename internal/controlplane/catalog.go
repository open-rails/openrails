// Package controlplane implements OpenRails' AuthKit control plane (issue
// #224).
//
// OpenRails owns an AuthKit control plane rather than acting only as an external
// JWT verifier. In self-hosted / locked-down mode OpenRails mounts only the
// AuthKit route groups it intentionally exposes, runs with public user
// registration disabled, and creates merchant groups, roles and credentials
// through in-process AuthKit Client calls — never raw SQL or a private HTTP
// route.
//
// HARDCUT (#567): merchant-local authority is evaluated in the caller's merchant
// permission group using OpenRails' app permissions (`merchant:*` seller,
// `customer:*` buyer/treasury). Cross-merchant directory authority belongs to
// root/platform control, not merchant groups.
package controlplane

import (
	"strings"

	"github.com/open-rails/authkit"
	"github.com/open-rails/authkit/iam"

	"github.com/open-rails/openrails/permissions"
)

// Roles is OpenRails' permission model (#567): two flat personas beside root.
// A merchant IS a merchant group; a customer group is explicit hosted
// portal membership, keyed by the customer's user id. Every permission
// OpenRails checks is registered here, because AuthKit refuses a check on an
// unregistered one. The strings are package permissions' public vocabulary.
var (
	Roles           = authkit.NewRoles()
	merchantPersona = Roles.Persona("merchant", authkit.APIKeys, authkit.RemoteApplications)
	customerPersona = Roles.Persona("customer")

	MerchantType = merchantPersona.Persona
	CustomerType = customerPersona.Persona

	// Owners hold their whole namespace (`merchant:*`, `customer:*`).
	MerchantOwner = merchantPersona.Owner
	CustomerOwner = customerPersona.Owner

	MerchantCreator = merchantRole("creator",
		permissions.MerchantCatalogOwnRead, permissions.MerchantCatalogOwnUpdate)
	MerchantSupport = merchantRole("support",
		permissions.MerchantCustomerSettingsRead, permissions.MerchantCustomerSettingsUpdate,
		permissions.MerchantPaymentsRead, permissions.MerchantPaymentsRefund,
		permissions.MerchantInvoicesRead, permissions.MerchantInvoicesCollect,
		permissions.MerchantSubscriptionsRead, permissions.MerchantSubscriptionsUpdate,
		permissions.MerchantUsageRead, permissions.MerchantHostEventsRead, permissions.MerchantRepairAlertsRead,
		permissions.MerchantMetricsRead, permissions.MerchantDashboardUpdate)
	// MerchantViewer is read-only: finance, audit, analysts and LLM agents.
	MerchantViewer = merchantRole("viewer",
		permissions.MerchantSettingsRead, permissions.MerchantPaymentProvidersRead,
		permissions.MerchantCatalogRead, permissions.MerchantCustomerSettingsRead,
		permissions.MerchantPaymentsRead, permissions.MerchantInvoicesRead, permissions.MerchantSubscriptionsRead,
		permissions.MerchantUsageRead, permissions.MerchantHostEventsRead, permissions.MerchantRepairAlertsRead,
		permissions.MerchantMetricsRead)

	// CustomerMember is a delegated spender: read-only on the balance surface.
	CustomerMember = customerPersona.Role("member",
		declared(permissions.CustomerBalanceRead), declared(permissions.CustomerSpendDelegationsRead))

	// Bounded platform-operator roles (#721) for the cross-merchant directory.
	// The root owner holds root:* and covers both.
	RootMerchantDirectoryViewer = Roles.Root.Role("merchant-directory-viewer", declared(permissions.RootMerchantsRead))
	RootMerchantDirectoryAdmin  = Roles.Root.Role("merchant-directory-admin",
		declared(permissions.RootMerchantsRead), declared(permissions.RootMerchantsDelete), declared(permissions.RootMerchantsRestore))
)

// catalogPerms registers every OpenRails permission. members:* and
// credentials:* are AuthKit's built-ins and are not declared again.
var catalogPerms = func() map[string]iam.Perm {
	out := map[string]iam.Perm{}
	declare := func(p *authkit.PersonaDef, perms ...string) {
		for _, perm := range perms {
			_, rest, _ := strings.Cut(perm, ":")
			resource, action, _ := strings.Cut(rest, ":")
			out[perm] = p.Permission(resource, action)
		}
	}
	declare(merchantPersona,
		permissions.MerchantHostEventsRead, permissions.MerchantHostEventsAcknowledge,
		permissions.MerchantSettingsRead, permissions.MerchantSettingsUpdate,
		permissions.MerchantPaymentProvidersRead, permissions.MerchantPaymentProvidersUpdate,
		permissions.MerchantCatalogRead, permissions.MerchantCatalogUpdate,
		permissions.MerchantCatalogOwnRead, permissions.MerchantCatalogOwnUpdate,
		permissions.MerchantCustomerSettingsRead, permissions.MerchantCustomerSettingsUpdate,
		permissions.MerchantInvoicesRead, permissions.MerchantInvoicesUpdate, permissions.MerchantInvoicesCollect,
		permissions.MerchantCheckoutCreate,
		permissions.MerchantPaymentsRead, permissions.MerchantPaymentsRefund,
		permissions.MerchantSubscriptionsRead, permissions.MerchantSubscriptionsUpdate,
		permissions.MerchantAdmissionsCreate, permissions.MerchantUsageRead, permissions.MerchantRepairAlertsRead,
		permissions.MerchantMetricsRead, permissions.MerchantDashboardUpdate, permissions.MerchantFindingsResolve,
		permissions.MerchantBillingImport, permissions.MerchantBillingExport,
		permissions.MerchantCreditsGrant, permissions.MerchantCreditsRevoke, permissions.MerchantAccessGrantPermanent)
	declare(customerPersona,
		permissions.CustomerBalanceRead, permissions.CustomerBillingUpdate, permissions.CustomerPaymentMethodsUpdate,
		permissions.CustomerCheckoutCreate, permissions.CustomerSpendDelegationsRead, permissions.CustomerSpendDelegationsUpdate)
	declare(Roles.Root.PersonaDef,
		permissions.RootMerchantsRead, permissions.RootMerchantsDelete, permissions.RootMerchantsRestore,
		permissions.RootWorkerHealthRead, permissions.RootAdminRateLimitsUnlock)
	return out
}()

func declared(perm string) iam.Perm { return catalogPerms[perm] }

func merchantRole(name string, perms ...string) iam.Role {
	grants := make([]iam.Grant, 0, len(perms))
	for _, perm := range perms {
		grants = append(grants, declared(perm))
	}
	return merchantPersona.Role(name, grants...)
}

// MerchantRoles are the merchant team roles, least privilege first. Creator
// is subject-bound: a merchant-scoped machine key cannot supply that identity.
func MerchantRoles() []iam.Role {
	return []iam.Role{MerchantCreator, MerchantViewer, MerchantSupport, MerchantOwner}
}

// MerchantAPIKeyRoles are the roles a merchant API key may hold, least
// privilege first.
func MerchantAPIKeyRoles() []iam.Role {
	return []iam.Role{MerchantViewer, MerchantSupport, MerchantOwner}
}

// MerchantRole resolves a bare merchant role name ("viewer") from the wire.
func MerchantRole(name string) (iam.Role, bool) {
	name = strings.ToLower(strings.TrimSpace(name))
	for _, role := range MerchantRoles() {
		if role.Name() == name {
			return role, true
		}
	}
	return iam.Role{}, false
}

// RoleNames is roles' bare names, as the wire shows them.
func RoleNames(roles []iam.Role) []string {
	out := make([]string, len(roles))
	for i, role := range roles {
		out[i] = role.Name()
	}
	return out
}

// RoleCoveredBy reports whether grants cover every permission role confers in
// the running catalog: a non-user principal may hand out only authority it
// holds.
func (c *ControlPlane) RoleCoveredBy(role iam.Role, grants []string) (bool, error) {
	perms, err := c.client.RolePermissions(role)
	if err != nil {
		return false, err
	}
	return coveredAll(perms, grants), nil
}

// coveredAll reports whether grants cover every one of perms, and perms is
// not empty.
func coveredAll(perms []iam.Perm, grants []string) bool {
	if len(perms) == 0 {
		return false
	}
	for _, perm := range perms {
		if !covered(perm, grants) {
			return false
		}
	}
	return true
}

// covered reports whether some grant authorizes perm (iam.Perm.Matches).
func covered(perm iam.Perm, grants []string) bool {
	for _, text := range grants {
		var grant iam.Perm
		if grant.UnmarshalText([]byte(strings.TrimSpace(text))) == nil && perm.Matches(grant) {
			return true
		}
	}
	return false
}
