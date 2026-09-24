package permissions_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/permissions"
	"github.com/open-rails/openrails/pkg/billingauth"
)

func TestForRolesPresetTiers(t *testing.T) {
	owner := []string{permissions.MerchantAll, permissions.CustomerAll}
	for _, tc := range []struct {
		roles []string
		want  []string
	}{
		{[]string{"owner"}, owner},
		{[]string{"admin"}, owner},
		{[]string{" OWNER "}, owner},
		{[]string{"member"}, permissions.PresetMemberPermissions},
		{[]string{"read-only"}, permissions.PresetReadOnlyPermissions},
		{[]string{"readonly"}, permissions.PresetReadOnlyPermissions},
		{[]string{"Viewer"}, permissions.PresetReadOnlyPermissions},
		{[]string{"viewer", "member"}, permissions.PresetMemberPermissions},
		{[]string{"member", "owner"}, append(append([]string{}, owner...), permissions.PresetMemberPermissions...)},
		{[]string{"superuser", "billing-admin"}, nil},
		{nil, nil},
	} {
		require.Equal(t, tc.want, permissions.ForRoles(tc.roles...), "%v", tc.roles)
	}
	require.ElementsMatch(t, []string{
		permissions.CustomerBalanceRead, permissions.CustomerBillingUpdate,
		permissions.CustomerSpendDelegationsRead, permissions.CustomerSpendDelegationsUpdate,
		permissions.CustomerCheckoutCreate, permissions.CustomerPaymentMethodsUpdate,
	}, permissions.PresetMemberPermissions)
	require.Equal(t, []string{permissions.CustomerBalanceRead, permissions.CustomerSpendDelegationsRead}, permissions.PresetReadOnlyPermissions)
	require.Subset(t, permissions.PresetMemberPermissions, permissions.PresetReadOnlyPermissions)
}

// The preset must compose with the gate's wildcard matcher.
func TestForRolesComposesWithHasPermission(t *testing.T) {
	for _, tc := range []struct {
		role  string
		perm  string
		allow bool
	}{
		{"owner", permissions.MerchantSettingsUpdate, true},
		{"owner", permissions.MerchantPaymentsRefund, true},
		{"owner", permissions.CustomerBalanceRead, true},
		{"owner", permissions.CustomerCheckoutCreate, true},
		{"owner", permissions.RootMerchantsRead, false},
		{"member", permissions.CustomerBalanceRead, true},
		{"member", permissions.CustomerCheckoutCreate, true},
		{"member", permissions.MerchantSettingsUpdate, false},
		{"member", permissions.MerchantCheckoutCreate, false},
		{"read-only", permissions.CustomerBalanceRead, true},
		{"read-only", permissions.CustomerBillingUpdate, false},
		{"read-only", permissions.CustomerCheckoutCreate, false},
	} {
		require.Equal(t, tc.allow, billingauth.HasPermission(permissions.ForRoles(tc.role), tc.perm), "%s -> %s", tc.role, tc.perm)
	}
}
