package controlplane

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/credential"
)

// #565: every credential type matches grants with the same namespace-anchored glob.
func TestCredentialPermissionGlob(t *testing.T) {
	for _, tc := range []struct {
		grants []string
		perm   string
		want   bool
	}{
		{[]string{"merchant:*"}, billing.MerchantCatalogUpdate, true},
		{[]string{billing.MerchantCatalogUpdate}, billing.MerchantCatalogUpdate, true},
		{[]string{billing.MerchantCatalogUpdate}, billing.MerchantCatalogRead, false},
		{[]string{"merchant:*:read"}, billing.MerchantCatalogRead, true},
		{[]string{"merchant:*:read"}, billing.MerchantCatalogUpdate, false},
		{[]string{"customer:*"}, billing.MerchantCatalogUpdate, false},
		{[]string{"root:*"}, billing.MerchantAdmissionsCreate, false},
		{[]string{"*"}, billing.MerchantCatalogUpdate, false},
		{[]string{billing.MerchantCustomerSettingsUpdate}, billing.MerchantAdmissionsCreate, false},
		{nil, billing.MerchantCatalogUpdate, false},
	} {
		require.Equal(t, tc.want, (&credential.ResolvedDelegated{Permissions: tc.grants}).HasPermission(tc.perm), "delegated %v %s", tc.grants, tc.perm)
		require.Equal(t, tc.want, (&credential.ResolvedResourceAccess{Permissions: tc.grants}).HasPermission(tc.perm), "resource %v %s", tc.grants, tc.perm)
		require.Equal(t, tc.want, (&ResolvedServiceCredential{Permissions: tc.grants}).HasPermission(tc.perm), "service %v %s", tc.grants, tc.perm)
		require.Equal(t, tc.want, len(credential.IntersectPermissions([]string{tc.perm}, tc.grants)) > 0, "intersect %v %s", tc.grants, tc.perm)
	}

	// #569: merchant credentials are merchant-wide but never act without a merchant or subject.
	wide := &ResolvedServiceCredential{MerchantID: billing.MerchantID(uuid.New())}
	require.True(t, wide.AllowsCustomer(uuid.New()))
	require.False(t, wide.AllowsCustomer(uuid.Nil))
	require.False(t, (&ResolvedServiceCredential{}).AllowsCustomer(uuid.New()))
	require.False(t, (*ResolvedServiceCredential)(nil).AllowsCustomer(uuid.New()))
}

// The roles' permissions are read from the running AuthKit catalog; the
// e2e TestMerchantRolePermissionsInTheRunningCatalog covers them.
func TestMerchantRoleCatalog(t *testing.T) {
	for _, name := range []string{" OWNER ", "viewer", "support"} {
		role, ok := MerchantRole(name)
		require.True(t, ok, name)
		require.Equal(t, MerchantType, role.Persona())
	}
	_, ok := MerchantRole("superadmin")
	require.False(t, ok)
	require.Equal(t, []string{"viewer", "support", "owner"}, RoleNames(MerchantRoles()), "the wire shows bare role names")
}
