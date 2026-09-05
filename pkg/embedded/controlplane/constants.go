package controlplane

import (
	"github.com/open-rails/authkit"

	"github.com/open-rails/openrails/internal/controlplane"
)

// MerchantType / CustomerType re-export the OpenRails permission-group
// personas (#567) so an embedding host doesn't re-type the literals
// "merchant"/"customer" (#750) when inspecting ListSubjectGroups results or
// building an authkit.GroupRef. They are aliases for internal/controlplane's
// own constants — the two can never drift.
const (
	MerchantType = controlplane.MerchantType
	CustomerType = controlplane.CustomerType
)

// MerchantGroup / CustomerGroup address a merchant (by slug) or customer (by
// owning user id) permission-group for typed AuthKit calls.
func MerchantGroup(slug string) authkit.GroupRef { return controlplane.MerchantGroup(slug) }

func CustomerGroup(customerID string) authkit.GroupRef {
	return controlplane.CustomerGroup(customerID)
}

// CustomerGroupSlug returns the AuthKit permission-group instance_slug for a
// user's own customer group. By OpenRails convention (#567) that slug IS the
// user's own id — EnsureCustomerPermissionGroup and the lazy customer-group
// route wrapper both key off exactly this equality internally. Hosts walking a subject's ListSubjectGroups memberships use
// this helper (instead of re-deriving the convention as
// `g.InstanceSlug == userID`) to recognize a `CustomerType` membership as the
// caller's own self-owned group, e.g.:
//
//	if g.Persona == embcp.CustomerType && g.InstanceSlug == embcp.CustomerGroupSlug(userID) { ... }
func CustomerGroupSlug(userID string) string {
	return userID
}
