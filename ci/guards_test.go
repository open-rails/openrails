//go:build e2e && integration

package ci_test

import "github.com/open-rails/openrails"

// perm is a test host's own permission.
type perm string

func (p perm) String() string { return string(p) }

// staffGroups turns every staff route group on, and staffPermissions gives
// each its own permission.
var (
	staffGroups      = openrails.RouteGroups{Admin: true, Catalog: true, MerchantConfig: true, Metrics: true}
	staffPermissions = openrails.Permissions{AdminRead: perm("host:billing:read"), AdminUpdate: perm("host:billing:update"), Catalog: perm("host:catalog"), MerchantConfig: perm("host:billing:admin"), Metrics: perm("host:metrics")}
)
