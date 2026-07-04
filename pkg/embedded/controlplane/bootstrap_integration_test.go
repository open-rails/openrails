//go:build integration

package controlplane_test

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/dbtest"
	embcp "github.com/open-rails/openrails/pkg/embedded/controlplane"
)

// TestRunBootstrap_ExternalShapeAndExplicitMintOnly is #747's regression
// guard, exercised entirely through the exported pkg/embedded/controlplane
// surface (this file cannot import internal/controlplane, proving the shape
// really is external-host-constructible):
//
//   - embcp.BootstrapOptions{...} compiles and round-trips through
//     embcp.RunBootstrap (the alias pattern actually works, not just typechecks).
//   - RunBootstrap forwards MintInitialAPIKey VERBATIM: a false request never
//     mints, even on a merchant's very first Bootstrap call (pre-#747 this was
//     force-overridden to true regardless of what the caller passed).
//   - An explicit true mints once, normally.
//   - After an operator revokes that key, a later explicit true request does
//     NOT auto-heal it with a fresh mint.
func TestRunBootstrap_ExternalShapeAndExplicitMintOnly(t *testing.T) {
	ctx := context.Background()
	dsn := dbtest.SharedPostgresDSN(t)
	cfg := hostedTestConfig(dsn, "https://controlplane.openrails.test")
	e := newHostApp(t, cfg)
	require.NoError(t, embcp.Attach(ctx, e.App(), cfg, nil))

	// Provision the merchant through the #738 public seam (no raw SQL): this is
	// also the realistic API-mode shape, where a merchant row/group can exist
	// before Bootstrap ever runs for it.
	slug := "bootstrap-747-" + strings.ToLower(uuid.NewString()[:8])
	_, err := embcp.ProvisionMerchant(ctx, e.App(), embcp.ProvisionMerchantRequest{Slug: slug})
	require.NoError(t, err)

	cp := embcp.Get(e.App())
	require.NotNil(t, cp)

	// A false request must never mint — not even on the merchant's very first
	// RunBootstrap call. Pre-#747 this was force-overridden to true.
	res1, err := embcp.RunBootstrap(ctx, e.App(), embcp.BootstrapOptions{
		BootstrapMerchantSlug: slug,
		MintInitialAPIKey:     false,
	})
	require.NoError(t, err)
	require.False(t, res1.APIKeyMinted, "MintInitialAPIKey:false must be honored verbatim")
	keys, err := cp.Core().ListAPIKeys(ctx, "merchant", slug)
	require.NoError(t, err)
	require.Empty(t, keys)

	// An explicit true request on this genuinely-first-run merchant mints.
	res2, err := embcp.RunBootstrap(ctx, e.App(), embcp.BootstrapOptions{
		BootstrapMerchantSlug: slug,
		MintInitialAPIKey:     true,
	})
	require.NoError(t, err)
	require.True(t, res2.APIKeyMinted)
	require.NotEmpty(t, res2.APIKeySecret)

	// Operator revokes it (e.g. suspected compromise).
	keys, err = cp.Core().ListAPIKeys(ctx, "merchant", slug)
	require.NoError(t, err)
	require.Len(t, keys, 1)
	revoked, err := cp.Core().RevokeAPIKey(ctx, "merchant", slug, keys[0].ID)
	require.NoError(t, err)
	require.True(t, revoked)

	// A later explicit mint request must NOT auto-heal the revocation.
	res3, err := embcp.RunBootstrap(ctx, e.App(), embcp.BootstrapOptions{
		BootstrapMerchantSlug: slug,
		MintInitialAPIKey:     true,
	})
	require.NoError(t, err)
	require.False(t, res3.APIKeyMinted, "RunBootstrap must never re-mint after an operator revokes all keys")

	finalKeys, err := cp.Core().ListAPIKeys(ctx, "merchant", slug)
	require.NoError(t, err)
	require.Len(t, finalKeys, 1, "no replacement key should exist after revocation")
}
