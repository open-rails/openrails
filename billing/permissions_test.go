package billing_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/billingauth"
)

func TestPermissionsForRolesPresetTiers(t *testing.T) {
	owner := []string{billing.MerchantAll, billing.CustomerAll}
	for _, tc := range []struct {
		roles []string
		want  []string
	}{
		{[]string{"owner"}, owner},
		{[]string{"admin"}, owner},
		{[]string{" OWNER "}, owner},
		{[]string{"member"}, billing.PresetMemberPermissions},
		{[]string{"read-only"}, billing.PresetReadOnlyPermissions},
		{[]string{"readonly"}, billing.PresetReadOnlyPermissions},
		{[]string{"Viewer"}, billing.PresetReadOnlyPermissions},
		{[]string{"viewer", "member"}, billing.PresetMemberPermissions},
		{[]string{"member", "owner"}, append(append([]string{}, owner...), billing.PresetMemberPermissions...)},
		{[]string{"superuser", "billing-admin"}, nil},
		{nil, nil},
	} {
		require.Equal(t, tc.want, billing.PermissionsForRoles(tc.roles...), "%v", tc.roles)
	}
	require.ElementsMatch(t, []string{
		billing.CustomerBalanceRead, billing.CustomerBillingUpdate,
		billing.CustomerSpendDelegationsRead, billing.CustomerSpendDelegationsUpdate,
		billing.CustomerCheckoutCreate, billing.CustomerPaymentMethodsUpdate,
	}, billing.PresetMemberPermissions)
	require.Equal(t, []string{billing.CustomerBalanceRead, billing.CustomerSpendDelegationsRead}, billing.PresetReadOnlyPermissions)
	require.Subset(t, billing.PresetMemberPermissions, billing.PresetReadOnlyPermissions)
}

// The preset must compose with the gate's wildcard matcher.
func TestForRolesComposesWithHasPermission(t *testing.T) {
	for _, tc := range []struct {
		role  string
		perm  string
		allow bool
	}{
		{"owner", billing.MerchantSettingsUpdate, true},
		{"owner", billing.MerchantPaymentsRefund, true},
		{"owner", billing.CustomerBalanceRead, true},
		{"owner", billing.CustomerCheckoutCreate, true},
		{"owner", billing.RootMerchantsRead, false},
		{"member", billing.CustomerBalanceRead, true},
		{"member", billing.CustomerCheckoutCreate, true},
		{"member", billing.MerchantSettingsUpdate, false},
		{"member", billing.MerchantCheckoutCreate, false},
		{"read-only", billing.CustomerBalanceRead, true},
		{"read-only", billing.CustomerBillingUpdate, false},
		{"read-only", billing.CustomerCheckoutCreate, false},
	} {
		require.Equal(t, tc.allow, billingauth.HasPermission(billing.PermissionsForRoles(tc.role), tc.perm), "%s -> %s", tc.role, tc.perm)
	}
}

// The step-up policy: merchant operations that move money, grant access or
// mint credentials; never reads or a buyer's own billing.
func TestRequiresRecentSignIn(t *testing.T) {
	for perm, want := range map[string]bool{
		billing.MerchantCustomerSettingsUpdate: true, billing.MerchantPaymentsRefund: true,
		billing.MerchantPaymentProvidersUpdate: true, billing.MerchantCatalogUpdate: true,
		billing.MerchantCreditsGrant: true, billing.MerchantBillingImport: true,
		billing.MerchantBillingExport: true, billing.MerchantCredentialsManage: true,
		billing.MerchantMembersManage: true, billing.MerchantAccessGrantPermanent: true,
		billing.MerchantPaymentsRead: false, billing.MerchantCatalogOwnRead: false,
		billing.MerchantDashboardUpdate: false, billing.MerchantHostEventsAcknowledge: false,
		billing.CustomerCheckoutCreate: false, billing.CustomerSpendDelegationsUpdate: false,
	} {
		require.Equal(t, want, billing.RequiresRecentSignIn(perm), perm)
	}
}
