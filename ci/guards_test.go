//go:build e2e && integration

package ci_test

import "github.com/open-rails/openrails"

// perm is a test host's own permission.
type perm string

func (p perm) String() string { return string(p) }

// staffPermissions give every bundle its own permission.
var staffPermissions = openrails.Permissions{AdminRead: perm("host:billing:read"), AdminWrite: perm("host:billing:write"), CatalogWrite: perm("host:catalog:write"), MerchantConfig: perm("host:billing:admin")}
