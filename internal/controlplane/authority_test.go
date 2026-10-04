package controlplane

import (
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/authkit/iam"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
)

// BND-C1: a service JWT may only down-scope its issuer's stored authority.
func TestServiceJWTPermissionsOnlyDownScope(t *testing.T) {
	for _, tc := range []struct {
		name            string
		claimed, stored []string
		want            []string
	}{
		{"empty stored denies all", []string{"root:*", billing.MerchantCustomerSettingsRead}, nil, nil},
		{"empty claimed yields nothing", nil, []string{billing.MerchantCustomerSettingsRead}, nil},
		{"only granted survive", []string{billing.MerchantCustomerSettingsRead, billing.MerchantCustomerSettingsUpdate, "root:*"}, []string{billing.MerchantCustomerSettingsRead}, []string{billing.MerchantCustomerSettingsRead}},
		{"self-asserted root stripped", []string{"root:*"}, []string{billing.MerchantCustomerSettingsRead}, []string{}},
		{"explicitly stored root survives", []string{"root:*"}, []string{"root:*", billing.MerchantCustomerSettingsRead}, []string{"root:*"}},
		{"owner glob covers concrete claims", []string{billing.MerchantCustomerSettingsUpdate, billing.MerchantPaymentsRefund}, []string{"merchant:*"}, []string{billing.MerchantCustomerSettingsUpdate, billing.MerchantPaymentsRefund}},
		{"narrow grant cannot cover a glob claim", []string{"merchant:*"}, []string{billing.MerchantCatalogRead}, []string{}},
		{"segment glob", []string{billing.MerchantPaymentsRead, billing.MerchantPaymentsRefund}, []string{"merchant:*:read"}, []string{billing.MerchantPaymentsRead}},
		{"namespace never crosses", []string{billing.MerchantCatalogRead}, []string{"customer:*", "*"}, []string{}},
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
		require.Equal(t, tc.want, (&ResolvedDelegated{Permissions: tc.grants}).HasPermission(tc.perm), "delegated %v %s", tc.grants, tc.perm)
		require.Equal(t, tc.want, (&ResolvedServiceCredential{Permissions: tc.grants}).HasPermission(tc.perm), "service %v %s", tc.grants, tc.perm)
		require.Equal(t, tc.want, len(intersectPermissions([]string{tc.perm}, tc.grants)) == 1, "intersect %v %s", tc.grants, tc.perm)
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
	for _, name := range []string{" OWNER ", "viewer", "support", "creator"} {
		role, ok := MerchantRole(name)
		require.True(t, ok, name)
		require.Equal(t, MerchantType, role.Persona())
	}
	_, ok := MerchantRole("superadmin")
	require.False(t, ok)
	require.Equal(t, []iam.Role{MerchantViewer, MerchantSupport, MerchantOwner}, MerchantAPIKeyRoles(), "machine keys cannot resolve a creator subject")
	require.Equal(t, []string{"creator", "viewer", "support", "owner"}, RoleNames(MerchantRoles()), "the wire shows bare role names")
}
