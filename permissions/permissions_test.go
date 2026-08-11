package permissions_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/permissions"
	"github.com/open-rails/openrails/pkg/billingauth"
)

func TestForRolesPreset(t *testing.T) {
	t.Parallel()

	// owner/admin → the full wildcard tier, both namespaces.
	require.Equal(t, []string{permissions.MerchantAll, permissions.CustomerAll}, permissions.ForRoles("owner"))
	require.Equal(t, permissions.ForRoles("owner"), permissions.ForRoles("admin"))

	// member → the customer self-service set, exactly.
	require.ElementsMatch(t, []string{
		permissions.CustomerBalanceRead,
		permissions.CustomerBillingUpdate,
		permissions.CustomerSpendDelegationsRead,
		permissions.CustomerSpendDelegationsUpdate,
		permissions.CustomerCheckoutCreate,
		permissions.CustomerPaymentMethodsUpdate,
	}, permissions.ForRoles("member"))

	// read-only shapes → the :read subset.
	readOnly := []string{permissions.CustomerBalanceRead, permissions.CustomerSpendDelegationsRead}
	require.Equal(t, readOnly, permissions.ForRoles("read-only"))
	require.Equal(t, readOnly, permissions.ForRoles("readonly"))
	require.Equal(t, readOnly, permissions.ForRoles("viewer"))

	// Trimmed + case-insensitive; multiple roles union without duplicates.
	require.Equal(t, permissions.ForRoles("owner"), permissions.ForRoles(" OWNER "))
	union := permissions.ForRoles("member", "viewer")
	require.ElementsMatch(t, permissions.ForRoles("member"), union, "read-only is a subset of member — union adds nothing")

	// Unknown roles fail closed.
	require.Nil(t, permissions.ForRoles("superuser", "billing-admin"))
	require.Nil(t, permissions.ForRoles())
}

// The preset's wildcard tier composes with the delegated gate's matcher:
// merchant:*/customer:* cover every concrete catalog permission.
func TestForRolesComposesWithHasPermission(t *testing.T) {
	t.Parallel()

	ownerGrants := permissions.ForRoles("owner")
	for _, perm := range []string{
		permissions.MerchantSettingsUpdate,
		permissions.MerchantPaymentsRefund,
		permissions.CustomerBalanceRead,
		permissions.CustomerCheckoutCreate,
	} {
		require.True(t, billingauth.HasPermission(ownerGrants, perm), perm)
	}

	memberGrants := permissions.ForRoles("member")
	require.True(t, billingauth.HasPermission(memberGrants, permissions.CustomerBalanceRead))
	require.False(t, billingauth.HasPermission(memberGrants, permissions.MerchantSettingsUpdate))

	readOnlyGrants := permissions.ForRoles("read-only")
	require.True(t, billingauth.HasPermission(readOnlyGrants, permissions.CustomerBalanceRead))
	require.False(t, billingauth.HasPermission(readOnlyGrants, permissions.CustomerBillingUpdate))
	require.False(t, billingauth.HasPermission(readOnlyGrants, permissions.CustomerCheckoutCreate))
}
