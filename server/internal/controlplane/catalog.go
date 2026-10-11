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
// Merchant-local authority is evaluated in the caller's merchant permission
// group using OpenRails' `merchant:` permissions (billing.Permissions), which
// guard the admin API's routes; their `root:` twins, held in the root group,
// reach every merchant. Cross-merchant directory authority belongs to
// root/platform control, not merchant groups.
package controlplane

import (
	"strings"

	"github.com/open-rails/authkit"
	"github.com/open-rails/authkit/iam"

	"github.com/open-rails/openrails/billing"
)

// Roles is the standalone server's permission model: two flat personas beside
// root. A merchant IS a merchant group; a customer group is explicit hosted
// portal membership, keyed by the customer's user id. Every permission the
// server checks is registered here, because AuthKit refuses a check on an
// unregistered one. A merchant defines its own roles from its persona's
// permissions (CustomRoles): least privilege for a program's API key.
var (
	Roles           = authkit.NewRoles()
	merchantPersona = Roles.Persona(billing.MerchantGroupPersona, authkit.APIKeys, authkit.RemoteApplications, authkit.CustomRoles, authkit.OAuthClients)
	customerPersona = Roles.Persona(billing.CustomerGroupPersona)

	MerchantType = merchantPersona.Persona
	CustomerType = customerPersona.Persona

	// Owners hold their whole namespace.
	MerchantOwner = merchantPersona.Owner
	CustomerOwner = customerPersona.Owner

	// MerchantSupport reads and acts on customers' billing; the merchant's
	// configuration is the owner's.
	MerchantSupport = merchantRole("support", supportGrants...)
	// MerchantViewer is read-only: finance, audit, analysts and LLM agents.
	MerchantViewer = merchantRole("viewer", viewerGrants...)

	// Bounded platform-operator roles (#721) for the cross-merchant directory.
	// The root owner holds root:* and covers both.
	RootMerchantDirectoryViewer = Roles.Root.Role("merchant-directory-viewer", declared(billing.RootMerchantsRead))
	RootMerchantDirectoryAdmin  = Roles.Root.Role("merchant-directory-admin",
		declared(billing.RootMerchantsRead), declared(billing.RootMerchantsDelete), declared(billing.RootMerchantsRestore))
)

// catalogPerms registers every OpenRails permission, each on its persona:
// the routes' (billing.Permissions) and the platform operator's. members:*,
// credentials:*, directory:* and roles:* are AuthKit's built-ins and are not
// declared again.
var catalogPerms = func() map[billing.Permission]iam.Perm {
	out := map[billing.Permission]iam.Perm{}
	for _, perm := range append(billing.Permissions(),
		billing.RootMerchantsRead, billing.RootMerchantsDelete, billing.RootMerchantsRestore,
		billing.RootWorkerHealthRead, billing.RootAdminRateLimitsUnlock) {
		persona := Roles.Root.PersonaDef
		if perm.Persona() == billing.MerchantGroupPersona {
			persona = merchantPersona
		}
		out[perm] = persona.Permission(perm.Resource(), perm.Action())
	}
	return out
}()

func declared(perm billing.Permission) iam.Perm { return catalogPerms[perm] }

// APIKeyPrefix is the fixed OpenRails API-key marker.
const APIKeyPrefix = "openrails"

var (
	supportGrants = []billing.Permission{billing.MerchantBillingRead, billing.MerchantBillingManage}
	viewerGrants  = []billing.Permission{billing.MerchantBillingRead}
)

func merchantRole(name string, perms ...billing.Permission) iam.Role {
	grants := make([]iam.Grant, 0, len(perms))
	for _, perm := range perms {
		grants = append(grants, declared(perm))
	}
	return merchantPersona.Role(name, grants...)
}

// MerchantRoles are the roles a teammate or merchant API key may hold, least
// privilege first.
func MerchantRoles() []iam.Role {
	return []iam.Role{MerchantViewer, MerchantSupport, MerchantOwner}
}

// MerchantRole resolves a bare merchant role name from the wire: a declared
// role ("viewer") or a merchant's custom role ("custom-storefront"), which
// AuthKit finds in the merchant's group where it is used.
func MerchantRole(name string) (iam.Role, bool) {
	name = strings.ToLower(strings.TrimSpace(name))
	for _, role := range MerchantRoles() {
		if role.Name() == name {
			return role, true
		}
	}
	if !isCustomRole(name) {
		return iam.Role{}, false
	}
	var role iam.Role
	if err := role.UnmarshalText([]byte(MerchantType.String() + ":" + name)); err != nil {
		return iam.Role{}, false
	}
	return role, true
}

// isCustomRole reports whether a bare role name is a custom role's: a
// merchant's "storefront" is merchant:custom-storefront.
func isCustomRole(name string) bool {
	rest, ok := strings.CutPrefix(name, "custom-")
	return ok && rest != ""
}

// RoleNames is roles' bare names, as the wire shows them.
func RoleNames(roles []iam.Role) []string {
	out := make([]string, len(roles))
	for i, role := range roles {
		out[i] = role.Name()
	}
	return out
}
