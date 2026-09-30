package controlplane

import (
	"strings"
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

func TestMerchantRoleCatalog(t *testing.T) {
	require.Equal(t, []iam.Perm{MerchantType.OwnerGrant()}, MerchantRolePermissions(MerchantOwner))
	for _, name := range []string{" OWNER ", "viewer", "support", "creator"} {
		role, ok := MerchantRole(name)
		require.True(t, ok, name)
		require.Equal(t, MerchantType, role.Persona())
	}
	_, ok := MerchantRole("superadmin")
	require.False(t, ok)
	require.ElementsMatch(t, []string{permissions.MerchantCatalogOwnRead, permissions.MerchantCatalogOwnUpdate}, texts(MerchantRolePermissions(MerchantCreator)))
	require.Equal(t, []iam.Role{MerchantViewer, MerchantSupport, MerchantOwner}, MerchantAPIKeyRoles(), "machine keys cannot resolve a creator subject")
	require.Equal(t, []string{"creator", "viewer", "support", "owner"}, RoleNames(MerchantRoles()), "the wire shows bare role names")
	for _, p := range texts(MerchantRolePermissions(MerchantViewer)) {
		require.True(t, strings.HasSuffix(p, ":read"), "viewer is read-only: %s", p)
	}

	ownerOnly := []string{
		permissions.MerchantSettingsUpdate, permissions.MerchantPaymentProvidersUpdate, permissions.MerchantCatalogUpdate,
		permissions.MerchantCreditsGrant, permissions.MerchantCreditsRevoke, permissions.MerchantCredentialsManage,
		permissions.MerchantMembersRead, permissions.MerchantMembersManage, permissions.MerchantBillingImport,
		permissions.MerchantBillingExport, permissions.MerchantAdmissionsCreate, permissions.MerchantCheckoutCreate,
	}
	for _, role := range []iam.Role{MerchantCreator, MerchantSupport, MerchantViewer} {
		for _, p := range ownerOnly {
			require.False(t, (&ResolvedServiceCredential{Permissions: texts(MerchantRolePermissions(role))}).HasPermission(p), "%s must not hold %s", role, p)
		}
	}
	require.Equal(t, CustomerType, CustomerMember.Persona())
}

// #757: a non-user principal may only mint keys for authority it already holds.
func TestMerchantRoleCoveredByPreventsEscalation(t *testing.T) {
	support, viewer := texts(MerchantRolePermissions(MerchantSupport)), texts(MerchantRolePermissions(MerchantViewer))
	for _, tc := range []struct {
		role   iam.Role
		grants []string
		want   bool
	}{
		{MerchantOwner, []string{"merchant:*"}, true},
		{MerchantSupport, []string{"merchant:*"}, true},
		{MerchantViewer, []string{"merchant:*"}, true},
		{MerchantViewer, []string{"merchant:*:read"}, true},
		{MerchantSupport, []string{"merchant:*:read"}, false},
		{MerchantOwner, support, false},
		{MerchantViewer, support, false},
		{MerchantSupport, viewer, false},
		{MerchantViewer, viewer, true},
		{MerchantOwner, []string{"customer:*", "root:*"}, false},
		{CustomerMember, []string{"customer:*"}, false},
		{MerchantViewer, nil, false},
	} {
		require.Equal(t, tc.want, MerchantRoleCoveredBy(tc.role, tc.grants), "%s by %v", tc.role, tc.grants)
	}
}

func texts(perms []iam.Perm) []string {
	out := make([]string, len(perms))
	for i, p := range perms {
		out[i] = p.String()
	}
	return out
}
