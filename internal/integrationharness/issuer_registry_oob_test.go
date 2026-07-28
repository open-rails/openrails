//go:build integration

package integrationharness

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/authkit"
	authtesting "github.com/open-rails/authkit/authtest"
	authcore "github.com/open-rails/authkit/embedded"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/controlplane"
	"github.com/open-rails/openrails/internal/dbtest"
	embcp "github.com/open-rails/openrails/pkg/embedded/controlplane"
)

// #852: registering/updating a remote-application issuer OUT OF BAND (the store
// path the CLI push-merchant-config uses: UpsertRemoteApplication in a process
// that never calls ReloadRemoteApplications) must converge in the live server's
// delegated verifier without a restart:
//   - a BRAND-NEW issuer verifies immediately (the verifier lazy-loads on miss),
//   - a rotated static key on an ALREADY-LOADED issuer (a miss never fires)
//     verifies within the issuer-registry TTL via the activity-driven refresh.
func TestDelegatedIssuerOutOfBandRegistrationNoRestart(t *testing.T) {
	ctx := context.Background()
	h := New(t, ctx)
	surface := h.StartStandalone("usd")

	cp := embcp.Get(surface.App())
	require.NotNil(t, cp, "control plane attached")
	core := cp.Core()
	require.NotNil(t, core, "authkit core")
	groupID := h.ensureMerchantGroup(core, dbtest.TestMerchantSlug)

	// --- Case A: brand-new issuer through the CLI's store path, NO reload.
	issuerA := authtesting.NewTestIssuerWithAudience("openrails")
	h.cleanup(issuerA.Close)
	slugA := "oob-new-" + strings.ReplaceAll(uuid.NewString(), "-", "")[:10]
	raA, err := core.UpsertRemoteApplication(ctx, authkit.RemoteApplication{
		Slug:              slugA,
		PermissionGroupID: groupID,
		Issuer:            issuerA.URL(),
		Mode:              authkit.RemoteAppModeStatic,
		PublicKeys:        testIssuerRemoteAppKeys(t, issuerA),
		Enabled:           true,
	})
	require.NoError(t, err, "register issuer out of band")
	require.NoError(t, core.Genesis().AssignGroupRole(ctx, controlplane.MerchantType, dbtest.TestMerchantSlug, raA.ID, authcore.SubjectKindRemoteApp, controlplane.MerchantRoleOwner))
	// Deliberately NO cp.ReloadRemoteApplications: the CLI runs in another process.

	tokenA, err := mintDelegatedAccessToken(ctx, issuerA.Signer(), authkit.DelegatedAccessParams{
		Issuer:           issuerA.URL(),
		Audiences:        []string{"openrails"},
		DelegatedSubject: uuid.NewString(),
		TTL:              time.Hour,
	})
	require.NoError(t, err)
	res, err := cp.ResolveDelegated(ctx, tokenA, "")
	require.NoError(t, err, "brand-new out-of-band issuer must verify without restart (lazy-load-on-miss)")
	require.Equal(t, dbtest.TestMerchantSlug, res.MerchantSlug)

	// --- Case B: static-key rotation on an already-loaded issuer, NO reload.
	// A key rotation never produces an issuer miss, so lazy-load cannot heal it;
	// convergence relies on the #852 TTL refresh kicked by verification traffic.
	cp.SetIssuerRegistryTTL(50 * time.Millisecond)
	defer cp.SetIssuerRegistryTTL(0)

	oldKeys := authtesting.NewTestIssuerWithAudience("openrails")
	h.cleanup(oldKeys.Close)
	newKeys := authtesting.NewTestIssuerWithAudience("openrails")
	h.cleanup(newKeys.Close)

	slugB := "oob-rot-" + strings.ReplaceAll(uuid.NewString(), "-", "")[:10]
	raB, err := core.UpsertRemoteApplication(ctx, authkit.RemoteApplication{
		Slug:              slugB,
		PermissionGroupID: groupID,
		Issuer:            oldKeys.URL(),
		Mode:              authkit.RemoteAppModeStatic,
		PublicKeys:        testIssuerRemoteAppKeys(t, oldKeys),
		Enabled:           true,
	})
	require.NoError(t, err)
	require.NoError(t, core.Genesis().AssignGroupRole(ctx, controlplane.MerchantType, dbtest.TestMerchantSlug, raB.ID, authcore.SubjectKindRemoteApp, controlplane.MerchantRoleOwner))
	// Deterministically load the OLD key into the live verifier (the long-running
	// server already trusts it).
	require.NoError(t, cp.ReloadRemoteApplications(ctx))

	oldToken, err := mintDelegatedAccessToken(ctx, oldKeys.Signer(), authkit.DelegatedAccessParams{
		Issuer:           oldKeys.URL(),
		Audiences:        []string{"openrails"},
		DelegatedSubject: uuid.NewString(),
		TTL:              time.Hour,
	})
	require.NoError(t, err)
	_, err = cp.ResolveDelegated(ctx, oldToken, "")
	require.NoError(t, err, "pre-rotation baseline")

	// Rotate out of band: same slug + issuer identity, NEW static key, no reload.
	raB2, err := core.UpsertRemoteApplication(ctx, authkit.RemoteApplication{
		Slug:              slugB,
		PermissionGroupID: groupID,
		Issuer:            oldKeys.URL(),
		Mode:              authkit.RemoteAppModeStatic,
		PublicKeys:        testIssuerRemoteAppKeys(t, newKeys),
		Enabled:           true,
	})
	require.NoError(t, err)
	require.Equal(t, raB.ID, raB2.ID, "upsert keeps the remote_application identity")

	rotatedToken, err := mintDelegatedAccessToken(ctx, newKeys.Signer(), authkit.DelegatedAccessParams{
		Issuer:           oldKeys.URL(), // issuer identity unchanged; only the key rotated
		Audiences:        []string{"openrails"},
		DelegatedSubject: uuid.NewString(),
		TTL:              time.Hour,
	})
	require.NoError(t, err)

	// Each failed verification (stale key) kicks the async TTL refresh; the
	// rotated key must verify well within the window, no restart.
	require.Eventually(t, func() bool {
		_, err := cp.ResolveDelegated(ctx, rotatedToken, "")
		return err == nil
	}, 15*time.Second, 100*time.Millisecond, "rotated out-of-band key must verify within the issuer-registry TTL")
}
