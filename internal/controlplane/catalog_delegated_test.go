package controlplane

import (
	"testing"

	authhttp "github.com/open-rails/authkit/http"
	"github.com/stretchr/testify/require"
)

// These tests pin the LOAD-BEARING permission gate for federated tenant-signed
// browser tokens (issue #259): since the tenant signs, the verify-time catalog is
// the SOLE authority for what a browser token may carry. A regression that let a
// service/operator grant through would be a privilege-escalation hole.

func TestIsDelegatedPermission_AcceptsSelfAndTenant(t *testing.T) {
	for _, p := range SelfCatalogNames() {
		require.True(t, IsDelegatedPermission(p), "self perm %q must be accepted", p)
	}
	for _, p := range TenantCatalogNames() {
		require.True(t, IsDelegatedPermission(p), "tenant perm %q must be accepted", p)
		require.True(t, IsTenantPermission(p))
		require.False(t, IsSelfPermission(p), "tenant perm %q must not be a self perm", p)
	}
}

func TestIsDelegatedPermission_HardRejectsServiceAndOperatorGrants(t *testing.T) {
	// None of these may EVER ride a browser token. Includes the deprecated mint
	// permission, every coarse server-to-server capability, and both admin tiers.
	forbidden := []string{
		PermAdmin,
		PermCreditsRead,
		PermCreditsWrite,
		PermEntitlementsRead,
		PermCatalogWrite,
		PermPaymentsRefund,
		PermSubscriptionsCancel,
		PermPlatformSuperadmin,
		"openrails:tenant:*", // no wildcard is ever a real permission
		"openrails:tenant",   // coarse super-grant must not exist
		"openrails:self:*",   // wildcard, not a concrete self perm
		"totally-unknown-perm",
	}
	for _, p := range forbidden {
		require.False(t, IsDelegatedPermission(p), "permission %q must be hard-rejected on a browser token", p)
	}
}

// TestDelegatedVerify_AcceptsTenantAdminPermission proves a tenant-signed token
// carrying a concrete openrails:tenant:* grant verifies through the same catalog
// the real verifier uses.
func TestDelegatedVerify_AcceptsTenantAdminPermission(t *testing.T) {
	v, signer := newTestDelegatedVerifier(t)
	tok := mintDelegated(t, signer, authhttp.DelegatedAccessParams{
		Permissions: []string{PermTenantBillingRead, PermTenantEntitlementsWrite},
	})
	_, dp, err := v.VerifyDelegatedAccess(tok)
	require.NoError(t, err)
	require.Contains(t, dp.Permissions, PermTenantBillingRead)
}

// TestDelegatedVerify_RejectsServicePermissionEvenWhenTenantSigned proves the
// catalog gate rejects a service grant regardless of a valid signature — the
// whole point of moving signing to the tenant is that the catalog, not the
// signer, bounds authority.
func TestDelegatedVerify_RejectsServicePermissionEvenWhenTenantSigned(t *testing.T) {
	v, signer := newTestDelegatedVerifier(t)
	tok := mintDelegated(t, signer, authhttp.DelegatedAccessParams{
		Permissions: []string{PermCreditsWrite},
	})
	_, _, err := v.VerifyDelegatedAccess(tok)
	require.Error(t, err, "a credits:write grant must be rejected even on a validly-signed token")
}

// TestDelegatedVerify_RejectsMixedSelfAndService proves the gate is all-or-nothing
// per token: one bad permission in an otherwise-self token rejects the whole token.
func TestDelegatedVerify_RejectsMixedSelfAndService(t *testing.T) {
	v, signer := newTestDelegatedVerifier(t)
	tok := mintDelegated(t, signer, authhttp.DelegatedAccessParams{
		Permissions: []string{PermSelfBillingRead, PermCatalogWrite},
	})
	_, _, err := v.VerifyDelegatedAccess(tok)
	require.Error(t, err, "a single non-delegated permission must reject the whole token")
}
