//go:build e2e && integration

package ci_test

import (
	"strings"
	"testing"

	"github.com/open-rails/authkit/iam"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/controlplane"
	"github.com/open-rails/openrails/internal/engine"
	"github.com/open-rails/openrails/internal/operator"
)

// The merchant roles' authority is AuthKit's running catalog, not a copy:
// support, viewer and creator hold no owner authority, and a non-user
// credential hands out only a role its grants cover.
func TestMerchantRolePermissionsInTheRunningCatalog(t *testing.T) {
	f := newFixture(t)
	cp := operator.Get(engine.Graph(f.attachControlPlane(t, nil)))
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
	require.Equal(t, []string{billing.MerchantAll}, held(controlplane.MerchantOwner))
	require.ElementsMatch(t, []string{billing.MerchantCatalogOwnRead, billing.MerchantCatalogOwnUpdate}, held(controlplane.MerchantCreator))
	for _, p := range held(controlplane.MerchantViewer) {
		require.True(t, strings.HasSuffix(p, ":read"), "viewer is read-only: %s", p)
	}
	ownerOnly := []string{
		billing.MerchantSettingsUpdate, billing.MerchantPaymentProvidersUpdate, billing.MerchantCatalogUpdate,
		billing.MerchantCreditsGrant, billing.MerchantCreditsRevoke, billing.MerchantCredentialsManage,
		billing.MerchantMembersRead, billing.MerchantMembersManage, billing.MerchantBillingImport,
		billing.MerchantBillingExport, billing.MerchantAdmissionsCreate, billing.MerchantCheckoutCreate,
	}
	for _, role := range []iam.Role{controlplane.MerchantCreator, controlplane.MerchantSupport, controlplane.MerchantViewer} {
		grants := held(role)
		for _, p := range ownerOnly {
			require.False(t, (&controlplane.ResolvedServiceCredential{Permissions: grants}).HasPermission(p), "%s must not hold %s", role, p)
		}
	}

	support, viewer := held(controlplane.MerchantSupport), held(controlplane.MerchantViewer)
	for _, tc := range []struct {
		role   iam.Role
		grants []string
		want   bool
	}{
		{controlplane.MerchantOwner, []string{"merchant:*"}, true},
		{controlplane.MerchantSupport, []string{"merchant:*"}, true},
		{controlplane.MerchantViewer, []string{"merchant:*:read"}, true},
		{controlplane.MerchantSupport, []string{"merchant:*:read"}, false},
		{controlplane.MerchantOwner, support, false},
		{controlplane.MerchantViewer, support, false},
		{controlplane.MerchantSupport, viewer, false},
		{controlplane.MerchantViewer, viewer, true},
		{controlplane.MerchantOwner, []string{"root:*"}, false},
		{controlplane.MerchantViewer, nil, false},
	} {
		covered, err := cp.RoleCoveredBy(tc.role, tc.grants)
		require.NoError(t, err)
		require.Equal(t, tc.want, covered, "%s by %v", tc.role, tc.grants)
	}
}
