//go:build e2e && integration

package ci_test

import (
	"testing"

	"github.com/open-rails/authkit/iam"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/server/internal/controlplane"
	"github.com/open-rails/openrails/server/internal/operator"
)

// The merchant roles' authority is AuthKit's running catalog, not a copy:
// support and viewer hold no owner authority.
func TestMerchantRolePermissionsInTheRunningCatalog(t *testing.T) {
	f := newFixture(t)
	_, cp := operator.Of(f.newServer(t, nil))
	require.NotNil(t, cp)

	held := func(role iam.Role) []string {
		t.Helper()
		perms, err := cp.Core().RolePermissions(role)
		require.NoError(t, err)
		out := make([]string, len(perms))
		for i, p := range perms {
			out[i] = p.String()
		}
		return out
	}
	require.Equal(t, []string{"merchant:*"}, held(controlplane.MerchantOwner))
	require.ElementsMatch(t, []string{string(billing.MerchantCustomerRead), string(billing.MerchantCustomerManage)}, held(controlplane.MerchantSupport), "support reads and acts on customers' billing")
	reads := []billing.Permission{billing.MerchantCustomerRead, billing.MerchantCatalogRead, billing.MerchantConfigRead, billing.MerchantMetricsRead, billing.MerchantUsageRead}
	var names []string
	for _, p := range reads {
		names = append(names, string(p))
	}
	require.ElementsMatch(t, names, held(controlplane.MerchantViewer), "viewer can inspect each area without mutations")
	for _, role := range []iam.Role{controlplane.MerchantSupport, controlplane.MerchantViewer} {
		grants := held(role)
		for _, p := range grants {
			require.Contains(t, append(reads, billing.MerchantCustomerManage), billing.Permission(p), "%s must not hold %s", role, p)
		}
		for _, g := range grants {
			require.NotContains(t, g, "*", "%s holds no pattern", role)
		}
	}
}
