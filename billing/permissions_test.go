package billing

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// The permission names are a contract with every host's roles: each is the
// full persona:resource:action, and each merchant: name has its root: twin.
func TestPermissionsAreNamedInBothPersonas(t *testing.T) {
	require.Equal(t, []Permission{
		"merchant:billing:read", "root:billing:read", "merchant:billing:manage", "root:billing:manage",
		"merchant:catalog:manage", "root:catalog:manage", "merchant:config:manage", "root:config:manage",
		"merchant:metrics:read", "root:metrics:read",
		"merchant:entitlements:read", "root:entitlements:read", "merchant:catalog:read", "root:catalog:read",
		"merchant:usage:manage", "root:usage:manage", "merchant:costs:manage", "root:costs:manage",
		"merchant:events:read", "root:events:read", "merchant:orders:manage", "root:orders:manage",
		"merchant:customers:read", "root:customers:read",
	}, Permissions())
	all := Permissions()
	for i := 0; i < len(all); i += 2 {
		merchant, root := all[i], all[i+1]
		require.Equal(t, MerchantGroupPersona, merchant.Persona(), merchant)
		require.Equal(t, "root", root.Persona(), root)
		require.Equal(t, merchant.Resource(), root.Resource(), merchant)
		require.Equal(t, merchant.Action(), root.Action(), merchant)
		require.Equal(t, Permission(merchant.Persona()+":"+merchant.Resource()+":"+merchant.Action()), merchant)
	}
	for _, malformed := range []Permission{"", "billing", "root:billing", "root:billing:read:all"} {
		require.Empty(t, malformed.Persona()+malformed.Resource()+malformed.Action(), "%q is not persona:resource:action", malformed)
	}
	require.NotContains(t, all, RootMerchantsRead, "no route checks an operator permission")
}
