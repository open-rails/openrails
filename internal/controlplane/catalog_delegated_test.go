package controlplane

import (
	"testing"

	"github.com/open-rails/authkit"
	"github.com/stretchr/testify/require"
)

// #564: federated remote-app-signed browser tokens carry NO OpenRails allowlist.
// AuthKit bounds requested permissions to the signer's stored authority and
// rejects over-claims before ResolveDelegated sees the principal.

func TestDelegatedVerify_AcceptsMerchantAdminClaim(t *testing.T) {
	v, signer := newTestDelegatedVerifier(t)
	tok := mintDelegated(t, signer, authkit.DelegatedAccessParams{
		Permissions: []string{PermMerchantCustomerSettingsRead, PermMerchantCustomerSettingsUpdate},
	})
	_, dp, err := v.VerifyDelegatedAccess(tok)
	require.NoError(t, err)
	require.Contains(t, dp.Permissions, PermMerchantCustomerSettingsRead)
}

func TestDelegatedVerify_AcceptsNoPermissions(t *testing.T) {
	v, signer := newTestDelegatedVerifier(t)
	tok := mintDelegated(t, signer, authkit.DelegatedAccessParams{})
	_, dp, err := v.VerifyDelegatedAccess(tok)
	require.NoError(t, err)
	require.Empty(t, dp.Permissions)
}
