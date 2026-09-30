package controlplane

import (
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/authkit/iam"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/permissions"
	"github.com/open-rails/openrails/pkg/merchant"
)

// BND-C1: a service JWT may only down-scope its issuer's stored authority.
func TestServiceJWTPermissionsOnlyDownScope(t *testing.T) {
	for _, tc := range []struct {
		name            string
		claimed, stored []string
		want            []string
	}{
		{"empty stored denies all", []string{"root:*", permissions.MerchantCustomerSettingsRead}, nil, nil},
		{"empty claimed yields nothing", nil, []string{permissions.MerchantCustomerSettingsRead}, nil},
		{"only granted survive", []string{permissions.MerchantCustomerSettingsRead, permissions.MerchantCustomerSettingsUpdate, "root:*"}, []string{permissions.MerchantCustomerSettingsRead}, []string{permissions.MerchantCustomerSettingsRead}},
		{"self-asserted root stripped", []string{"root:*"}, []string{permissions.MerchantCustomerSettingsRead}, []string{}},
		{"explicitly stored root survives", []string{"root:*"}, []string{"root:*", permissions.MerchantCustomerSettingsRead}, []string{"root:*"}},
		{"owner glob covers concrete claims", []string{permissions.MerchantCustomerSettingsUpdate, permissions.MerchantPaymentsRefund}, []string{"merchant:*"}, []string{permissions.MerchantCustomerSettingsUpdate, permissions.MerchantPaymentsRefund}},
		{"narrow grant cannot cover a glob claim", []string{"merchant:*"}, []string{permissions.MerchantCatalogRead}, []string{}},
		{"segment glob", []string{permissions.MerchantPaymentsRead, permissions.MerchantPaymentsRefund}, []string{"merchant:*:read"}, []string{permissions.MerchantPaymentsRead}},
		{"namespace never crosses", []string{permissions.MerchantCatalogRead}, []string{"customer:*", "*"}, []string{}},
		{"order follows claimed", []string{"c:x", "a:x"}, []string{"b:x", "a:x", "c:x"}, []string{"c:x", "a:x"}},
		{"stored duplicates add nothing", []string{"a:x"}, []string{"a:x", "a:x"}, []string{"a:x"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := intersectPermissions(tc.claimed, tc.stored)
			require.Equal(t, len(tc.want), len(got), "%v", got)
			if len(tc.want) > 0 {
				require.Equal(t, tc.want, got)
			}
		})
	}
	require.Equal(t, []string{"a:x", "b:x"}, cleanPermissionList([]string{" a:x ", "", "b:x", "a:x", "  "}))
}

// #565: every credential type matches grants with the same namespace-anchored glob.
func TestCredentialPermissionGlob(t *testing.T) {
	for _, tc := range []struct {
		grants []string
		perm   string
		want   bool
	}{
		{[]string{"merchant:*"}, permissions.MerchantCatalogUpdate, true},
		{[]string{permissions.MerchantCatalogUpdate}, permissions.MerchantCatalogUpdate, true},
		{[]string{permissions.MerchantCatalogUpdate}, permissions.MerchantCatalogRead, false},
		{[]string{"merchant:*:read"}, permissions.MerchantCatalogRead, true},
		{[]string{"merchant:*:read"}, permissions.MerchantCatalogUpdate, false},
		{[]string{"customer:*"}, permissions.MerchantCatalogUpdate, false},
		{[]string{"root:*"}, permissions.MerchantAdmissionsCreate, false},
		{[]string{"*"}, permissions.MerchantCatalogUpdate, false},
		{[]string{permissions.MerchantCustomerSettingsUpdate}, permissions.MerchantAdmissionsCreate, false},
		{nil, permissions.MerchantCatalogUpdate, false},
	} {
		require.Equal(t, tc.want, (&ResolvedDelegated{Permissions: tc.grants}).HasPermission(tc.perm), "delegated %v %s", tc.grants, tc.perm)
		require.Equal(t, tc.want, (&ResolvedServiceCredential{Permissions: tc.grants}).HasPermission(tc.perm), "service %v %s", tc.grants, tc.perm)
		require.Equal(t, tc.want, len(intersectPermissions([]string{tc.perm}, tc.grants)) == 1, "intersect %v %s", tc.grants, tc.perm)
	}

	// #569: merchant credentials are merchant-wide but never act without a merchant or subject.
	wide := &ResolvedServiceCredential{MerchantID: merchant.ID(uuid.New())}
	require.True(t, wide.AllowsCustomer(uuid.New()))
	require.False(t, wide.AllowsCustomer(uuid.Nil))
	require.False(t, (&ResolvedServiceCredential{}).AllowsCustomer(uuid.New()))
	require.False(t, (*ResolvedServiceCredential)(nil).AllowsCustomer(uuid.New()))
}

// The roles' permissions are read from the running AuthKit catalog; the
// greenfield TestMerchantRolePermissionsInTheRunningCatalog covers them.
func TestMerchantRoleCatalog(t *testing.T) {
	for _, name := range []string{" OWNER ", "viewer", "support", "creator"} {
		role, ok := MerchantRole(name)
		require.True(t, ok, name)
		require.Equal(t, MerchantType, role.Persona())
	}
	_, ok := MerchantRole("superadmin")
	require.False(t, ok)
	require.Equal(t, []iam.Role{MerchantViewer, MerchantSupport, MerchantOwner}, MerchantAPIKeyRoles(), "machine keys cannot resolve a creator subject")
	require.Equal(t, []string{"creator", "viewer", "support", "owner"}, RoleNames(MerchantRoles()), "the wire shows bare role names")
	require.Equal(t, CustomerType, CustomerMember.Persona())
}
