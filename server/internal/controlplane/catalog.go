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
// group using the server's own `merchant:` permissions (staffperm), which
// guard the admin API's routes. Cross-merchant directory authority belongs
// to root/platform control, not merchant groups.
package controlplane

import (
	"strings"

	"github.com/open-rails/authkit"
	"github.com/open-rails/authkit/iam"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/credential"
	"github.com/open-rails/openrails/internal/staffperm"
)

// Roles is the standalone server's permission model: two flat personas beside
// root. A merchant IS a merchant group; a customer group is explicit hosted
// portal membership, keyed by the customer's user id. Every permission the
// server checks is registered here, because AuthKit refuses a check on an
// unregistered one.
var (
	Roles           = authkit.NewRoles()
	merchantPersona = Roles.Persona(billing.MerchantGroupPersona, authkit.APIKeys, authkit.RemoteApplications)
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
	declare(merchantPersona, staffperm.Declared...)
	declare(Roles.Root.PersonaDef,
		billing.RootMerchantsRead, billing.RootMerchantsDelete, billing.RootMerchantsRestore,
		billing.RootWorkerHealthRead, billing.RootAdminRateLimitsUnlock)
	return out
}()

func declared(perm string) iam.Perm { return catalogPerms[perm] }

var (
	supportGrants = []string{staffperm.BillingRead, staffperm.BillingManage}
	viewerGrants  = []string{staffperm.BillingRead}
)

// merchantRoleGrants are a merchant role's permissions as wire strings, for
// mapping an issuer's roles onto OpenRails merchant roles.
func merchantRoleGrants(name string) ([]string, bool) {
	switch name {
	case MerchantOwner.Name():
		return []string{staffperm.All}, true
	case MerchantSupport.Name():
		return supportGrants, true
	case MerchantViewer.Name():
		return viewerGrants, true
	}
	return nil, false
}

// merchantRoleFor names the merchant role whose grants are exactly perms;
// any other set is "custom".
func merchantRoleFor(perms []string) string {
	for _, role := range []iam.Role{MerchantOwner, MerchantSupport, MerchantViewer} {
		grants, _ := merchantRoleGrants(role.Name())
		if credential.EquivalentPermissions(grants, perms) {
			return role.Name()
		}
	}
	return "custom"
}

func merchantRole(name string, perms ...string) iam.Role {
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
