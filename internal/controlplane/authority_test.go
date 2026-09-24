package controlplane

import (
	"strings"
	"testing"

	"github.com/google/uuid"
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
		{"empty stored denies all", []string{"root:*", PermMerchantCustomerSettingsRead}, nil, nil},
		{"empty claimed yields nothing", nil, []string{PermMerchantCustomerSettingsRead}, nil},
		{"only granted survive", []string{PermMerchantCustomerSettingsRead, PermMerchantCustomerSettingsUpdate, "root:*"}, []string{PermMerchantCustomerSettingsRead}, []string{PermMerchantCustomerSettingsRead}},
		{"self-asserted root stripped", []string{"root:*"}, []string{PermMerchantCustomerSettingsRead}, []string{}},
		{"explicitly stored root survives", []string{"root:*"}, []string{"root:*", PermMerchantCustomerSettingsRead}, []string{"root:*"}},
		{"owner glob covers concrete claims", []string{PermMerchantCustomerSettingsUpdate, PermMerchantPaymentsRefund}, []string{"merchant:*"}, []string{PermMerchantCustomerSettingsUpdate, PermMerchantPaymentsRefund}},
		{"narrow grant cannot cover a glob claim", []string{"merchant:*"}, []string{PermMerchantCatalogRead}, []string{}},
		{"segment glob", []string{PermMerchantPaymentsRead, PermMerchantPaymentsRefund}, []string{"merchant:*:read"}, []string{PermMerchantPaymentsRead}},
		{"namespace never crosses", []string{PermMerchantCatalogRead}, []string{"customer:*", "*"}, []string{}},
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
		{[]string{"merchant:*"}, PermMerchantCatalogUpdate, true},
		{[]string{PermMerchantCatalogUpdate}, PermMerchantCatalogUpdate, true},
		{[]string{PermMerchantCatalogUpdate}, PermMerchantCatalogRead, false},
		{[]string{"merchant:*:read"}, PermMerchantCatalogRead, true},
		{[]string{"merchant:*:read"}, PermMerchantCatalogUpdate, false},
		{[]string{"customer:*"}, PermMerchantCatalogUpdate, false},
		{[]string{"root:*"}, PermMerchantAdmissionsCreate, false},
		{[]string{"*"}, PermMerchantCatalogUpdate, false},
		{[]string{PermMerchantCustomerSettingsUpdate}, PermMerchantAdmissionsCreate, false},
		{nil, PermMerchantCatalogUpdate, false},
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
	owner, ok := MerchantRolePermissions(" OWNER ")
	require.True(t, ok)
	require.Equal(t, []string{"merchant:*"}, owner)
	_, ok = MerchantRolePermissions("superadmin")
	require.False(t, ok)
	creator, _ := MerchantRolePermissions(MerchantRoleCreator)
	require.ElementsMatch(t, []string{permissions.MerchantCatalogOwnRead, permissions.MerchantCatalogOwnUpdate}, creator)
	require.Contains(t, MerchantRoles(), MerchantRoleCreator)
	require.Equal(t, []string{MerchantRoleViewer, MerchantRoleSupport, MerchantRoleOwner}, MerchantAPIKeyRoles(), "machine keys cannot resolve a creator subject")
	viewer, _ := MerchantRolePermissions(MerchantRoleViewer)
	for _, p := range viewer {
		require.True(t, strings.HasSuffix(p, ":read"), "viewer is read-only: %s", p)
	}

	ownerOnly := []string{
		PermMerchantSettingsUpdate, PermMerchantPaymentProvidersUpdate, PermMerchantCatalogUpdate,
		PermMerchantCreditsGrant, PermMerchantCreditsRevoke, PermMerchantCredentialsManage,
		PermMerchantMembersRead, PermMerchantMembersManage, PermMerchantBillingImport,
		PermMerchantBillingExport, PermMerchantAdmissionsCreate, permissions.MerchantCheckoutCreate,
	}
	for _, role := range []string{MerchantRoleCreator, MerchantRoleSupport, MerchantRoleViewer} {
		grants, _ := MerchantRolePermissions(role)
		for _, p := range ownerOnly {
			require.False(t, (&ResolvedServiceCredential{Permissions: grants}).HasPermission(p), "%s must not hold %s", role, p)
		}
	}
	for _, def := range Groups() {
		for _, role := range def.Roles {
			for _, p := range role.Permissions {
				if def.Name == CustomerType {
					require.True(t, strings.HasPrefix(p, "customer:") && strings.HasSuffix(p, ":read"), "customer %s: %s", role.Name, p)
				}
			}
		}
	}
}

// #757: a non-user principal may only mint keys for authority it already holds.
func TestMerchantRoleCoveredByPreventsEscalation(t *testing.T) {
	support, _ := MerchantRolePermissions(MerchantRoleSupport)
	viewer, _ := MerchantRolePermissions(MerchantRoleViewer)
	for _, tc := range []struct {
		role   string
		grants []string
		want   bool
	}{
		{MerchantRoleOwner, []string{"merchant:*"}, true},
		{MerchantRoleSupport, []string{"merchant:*"}, true},
		{MerchantRoleViewer, []string{"merchant:*"}, true},
		{MerchantRoleViewer, []string{"merchant:*:read"}, true},
		{MerchantRoleSupport, []string{"merchant:*:read"}, false},
		{MerchantRoleOwner, support, false},
		{MerchantRoleViewer, support, false},
		{MerchantRoleSupport, viewer, false},
		{MerchantRoleViewer, viewer, true},
		{MerchantRoleOwner, []string{"customer:*", "root:*"}, false},
		{"superadmin", []string{"merchant:*"}, false},
		{MerchantRoleViewer, nil, false},
	} {
		require.Equal(t, tc.want, MerchantRoleCoveredBy(tc.role, tc.grants), "%s by %v", tc.role, tc.grants)
	}
}

func TestMerchantCreationOnlyTouchesMerchantPersona(t *testing.T) {
	defs := withMerchantCreation(Groups(), MerchantCreationConfig{ReservedSlugs: []string{"acme"}, ReservedEscalationRole: "operator", SlugPattern: "[a-z]+"})
	seen := false
	for _, def := range defs {
		if def.Name != MerchantType {
			require.False(t, def.Creation.Enabled, "%s", def.Name)
			continue
		}
		seen = true
		require.True(t, def.Creation.Enabled)
		require.Equal(t, "[a-z]+", def.Creation.SlugPattern)
		require.Equal(t, "operator", string(def.Creation.ReservedEscalationRole))
		require.Subset(t, def.Creation.ReservedSlugs, merchant.ReservedHostedSlugs, "hosted defaults are always reserved")
		require.Contains(t, def.Creation.ReservedSlugs, "acme")
	}
	require.True(t, seen)
	for _, def := range Groups() {
		require.False(t, def.Creation.Enabled, "Groups() returns a fresh catalog")
	}
}
