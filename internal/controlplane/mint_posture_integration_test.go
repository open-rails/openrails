//go:build integration

package controlplane

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/dbtest"
)

// TestNew_VerifyOnlyMustBeDeclared proves the #748 posture: verify-only is a
// DECLARED config choice (auth.mint_disabled=true), never a silent downgrade
// from a signing-key discovery error. Outside development, a discovery
// failure without the flag is now a construction (boot) failure — previously
// it just warned and ran verify-only regardless of environment, making an
// outage indistinguishable from an intentional posture.
//
// Env "test" is used for the non-dev cases (matching the sibling
// bootstrap-package control-plane tests): it is NOT dev-like per OpenRails'
// own config.IsDev (so the #748 gate under test treats it as non-development),
// so the non-dev cases wire Redis (the engine refuses the in-memory ephemeral
// store outside development, ak#314) to keep that unrelated gate out of the way.
func TestNew_VerifyOnlyMustBeDeclared(t *testing.T) {
	pool := newBootstrapTestPool(t)
	rdb, _ := dbtest.SharedRedisClient(t)
	ctx := context.Background()

	t.Run("non-dev + no key + mint_disabled unset refuses to boot", func(t *testing.T) {
		cfg := &config.Config{
			Env:  "test",
			Auth: &config.AuthConfig{Issuer: "https://openrails.test"},
		}
		_, err := New(ctx, cfg, pool)
		require.Error(t, err, "an undeclared verify-only posture outside development must refuse to boot")
		require.Contains(t, err.Error(), "mint_disabled")
	})

	t.Run("non-dev + no key + mint_disabled=true boots verify-only", func(t *testing.T) {
		cfg := &config.Config{
			Env:  "test",
			Auth: &config.AuthConfig{Issuer: "https://openrails.test", MintDisabled: true, DirectPeerIP: true},
		}
		cp, err := New(ctx, cfg, pool, WithRedis(rdb))
		require.NoError(t, err, "a DECLARED verify-only posture must boot even outside development")
		require.NotNil(t, cp)
	})

	t.Run("dev + no key + mint_disabled unset still boots on the ephemeral dev key path", func(t *testing.T) {
		cfg := &config.Config{
			Env:  "dev",
			Auth: &config.AuthConfig{Issuer: "https://openrails.test"},
		}
		cp, err := New(ctx, cfg, pool)
		require.NoError(t, err, "development must keep booting without a declared posture (#748 scopes the hard failure to non-development)")
		require.NotNil(t, cp)
	})
}
