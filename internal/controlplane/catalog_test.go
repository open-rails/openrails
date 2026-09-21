package controlplane

import (
	"github.com/open-rails/openrails/permissions"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestCreatorRoleHasOnlySubjectBoundCatalogAuthority(t *testing.T) {
	grants, ok := MerchantRolePermissions(MerchantRoleCreator)
	require.True(t, ok)
	require.ElementsMatch(t, []string{permissions.MerchantCatalogOwnRead, permissions.MerchantCatalogOwnUpdate}, grants)
	require.Contains(t, MerchantRoles(), MerchantRoleCreator)
	require.NotContains(t, MerchantAPIKeyRoles(), MerchantRoleCreator, "machine keys do not resolve an owner subject")
	creator := &ResolvedServiceCredential{Permissions: grants}
	for _, permission := range []string{permissions.MerchantCatalogUpdate, permissions.MerchantPaymentProvidersUpdate, permissions.MerchantPaymentsRefund, permissions.MerchantCustomerSettingsUpdate} {
		require.False(t, creator.HasPermission(permission))
	}
}

// TestAdmissionCreatePermission_GateSemantics proves the admission hot-path gate:
// an API key WITHOUT merchant:admissions:create fails the admission gate while
// still passing customer writes. (The merchant owner's default grant is proven
// end-to-end in the bootstrap integration test.)
func TestAdmissionCreatePermission_GateSemantics(t *testing.T) {
	// A customer-write key without admission authority fails the spend gate.
	writeOnly := &ResolvedServiceCredential{Permissions: []string{PermMerchantCustomerSettingsUpdate}}
	if writeOnly.HasPermission(PermMerchantAdmissionsCreate) {
		t.Fatalf("merchant:customer-settings:update must NOT imply merchant:admissions:create")
	}
	if !writeOnly.HasPermission(PermMerchantCustomerSettingsUpdate) {
		t.Fatalf("write-only API key must still pass the write gate")
	}

	// An API key holding merchant:admissions:create passes the spend gate.
	spender := &ResolvedServiceCredential{Permissions: []string{PermMerchantCustomerSettingsUpdate, PermMerchantAdmissionsCreate}}
	if !spender.HasPermission(PermMerchantAdmissionsCreate) {
		t.Fatalf("API key holding %q must pass the spend gate", PermMerchantAdmissionsCreate)
	}

	admin := &ResolvedServiceCredential{Permissions: []string{"root:*"}}
	if admin.HasPermission(PermMerchantAdmissionsCreate) {
		t.Fatalf("foreign-persona apex grant %q must not bypass merchant spend gate", "root:*")
	}
}
