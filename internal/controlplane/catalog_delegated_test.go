package controlplane

import (
	"testing"

	authhttp "github.com/open-rails/authkit/http"
	"github.com/stretchr/testify/require"
)

// These tests pin the LOAD-BEARING permission gate for federated merchant-signed
// browser tokens (issue #259): since the merchant signs, the verify-time catalog is
// the SOLE authority for what a browser token may carry. A regression that let a
// service/operator grant through would be a privilege-escalation hole.

func TestIsDelegatedPermission_AcceptsMerchantAdminOnly(t *testing.T) {
	for _, p := range MerchantCatalogNames() {
		require.True(t, IsDelegatedPermission(p), "merchant perm %q must be accepted", p)
		require.True(t, IsMerchantPermission(p))
	}
}

func TestIsDelegatedPermission_HardRejectsPlatformAndUnknownGrants(t *testing.T) {
	// None of these may EVER ride a browser token.
	forbidden := []string{
		PermAdmin,
		PermPlatformSuperadmin,
		"platform:*",
		"platform:metrics:read",
		"org:totally_unknown:read",
		"org:*",
		"self:*",
		"totally-unknown-perm",
	}
	for _, p := range forbidden {
		require.False(t, IsDelegatedPermission(p), "permission %q must be hard-rejected on a browser token", p)
	}
}

// TestDelegatedVerify_AcceptsMerchantAdminPermission proves a merchant-signed token
// carrying browser-safe org grants verifies through the same allowlist the real
// verifier uses.
func TestDelegatedVerify_AcceptsMerchantAdminPermission(t *testing.T) {
	v, signer := newTestDelegatedVerifier(t)
	tok := mintDelegated(t, signer, authhttp.DelegatedAccessParams{
		Permissions: []string{PermMerchantBillingRead, PermMerchantEntitlementsWrite},
	})
	_, dp, err := v.VerifyDelegatedAccess(tok)
	require.NoError(t, err)
	require.Contains(t, dp.Permissions, PermMerchantBillingRead)
}

// TestDelegatedVerify_RejectsServicePermissionEvenWhenMerchantSigned proves the
// catalog gate rejects a service grant regardless of a valid signature — the
// whole point of moving signing to the merchant is that the catalog, not the
// signer, bounds authority.
func TestDelegatedVerify_RejectsPlatformPermissionEvenWhenMerchantSigned(t *testing.T) {
	v, signer := newTestDelegatedVerifier(t)
	tok := mintDelegated(t, signer, authhttp.DelegatedAccessParams{
		Permissions: []string{PermPlatformSuperadmin},
	})
	_, _, err := v.VerifyDelegatedAccess(tok)
	require.Error(t, err, "a platform grant must be rejected even on a validly-signed token")
}

func TestDelegatedVerify_RejectsUnknownPermission(t *testing.T) {
	v, signer := newTestDelegatedVerifier(t)
	tok := mintDelegated(t, signer, authhttp.DelegatedAccessParams{
		Permissions: []string{"org:not_in_browser_catalog:read"},
	})
	_, _, err := v.VerifyDelegatedAccess(tok)
	require.Error(t, err, "a single non-delegated permission must reject the whole token")
}

func TestDelegatedVerify_AcceptsNoPermissions(t *testing.T) {
	v, signer := newTestDelegatedVerifier(t)
	tok := mintDelegated(t, signer, authhttp.DelegatedAccessParams{})
	_, dp, err := v.VerifyDelegatedAccess(tok)
	require.NoError(t, err)
	require.Empty(t, dp.Permissions)
}
