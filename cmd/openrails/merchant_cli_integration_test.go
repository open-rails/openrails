//go:build integration

package main

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/authkit"
	authcore "github.com/open-rails/authkit/embedded"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/pkg/merchant"
	"github.com/stretchr/testify/require"
)

func TestCLIMerchantNameAndExplicitIdentityAreDistinct(t *testing.T) {
	ctx := context.Background()
	_, dsn := dbtest.SharedRLSPostgres(t)
	admin := dbtest.SharedSuperuserPGXPool(t)
	poolConfig, err := pgxpool.ParseConfig(dsn)
	require.NoError(t, err)
	poolConfig.MaxConns = 1
	pool, err := pgxpool.NewWithConfig(ctx, poolConfig)
	require.NoError(t, err)
	defer pool.Close()
	database, err := db.NewWithPGXPool(pool, config.DefaultSchema)
	require.NoError(t, err)
	core, err := authcore.New(authcore.Config{Keys: authcore.KeysConfig{VerifyOnly: true}, Token: authcore.TokenConfig{Issuer: "https://cli-names.test", IssuedAudiences: []string{"test"}}, RBAC: []authcore.PersonaDef{{Name: "merchant", Parent: authkit.RootPersona}}, Ephemeral: authcore.EphemeralConfig{AllowMemory: true}}, authcore.Deps{Postgres: admin})
	require.NoError(t, err)
	require.NoError(t, core.SeedPermissionGroupContainment(ctx))
	_, err = core.EnsureRootGroup(ctx)
	require.NoError(t, err)
	suffix := uuid.NewString()[:8]
	directory, err := merchants.NewDirectoryService(database.DataPool())
	require.NoError(t, err)
	groupA, err := core.CreatePermissionGroup(ctx, authkit.CreatePermissionGroupRequest{Persona: "merchant", InstanceSlug: "cli-a-" + suffix})
	require.NoError(t, err)
	a, _, err := directory.Provision(ctx, merchants.ProvisionRequest{Slug: "cli-a-" + suffix, PermissionGroupID: groupA})
	require.NoError(t, err)
	// B's legal public name happens to equal A's internal UUID. A bare string
	// must follow the name namespace, even while A is alive.
	groupB, err := core.CreatePermissionGroup(ctx, authkit.CreatePermissionGroupRequest{Persona: "merchant", InstanceSlug: a.ID.String()})
	require.NoError(t, err)
	b, _, err := directory.Provision(ctx, merchants.ProvisionRequest{Slug: a.ID.String(), PermissionGroupID: groupB})
	require.NoError(t, err)
	require.NoError(t, database.RunInMerchantConn(merchant.WithID(ctx, b.ID), func(ctx context.Context) error {
		selected, err := resolveCLIMerchant(ctx, database, a.ID.String())
		require.NoError(t, err)
		require.Equal(t, b.ID, selected)
		return err
	}))
	selected, err := resolveCLIMerchant(ctx, database, "id:"+a.ID.String())
	require.NoError(t, err)
	require.Equal(t, a.ID, selected)
	_, err = resolveCLIMerchant(ctx, database, "id:"+uuid.NewString())
	require.Error(t, err, "an absent explicit UUID cannot produce empty successful work")
	_, err = admin.Exec(ctx, `UPDATE openrails.merchants SET deleted_at=now() WHERE id=$1`, a.ID.UUID())
	require.NoError(t, err)
	_, err = resolveCLIMerchant(ctx, database, "id:"+a.ID.String())
	require.Error(t, err, "a deleted explicit UUID is unavailable")
	selected, err = resolveCLIMerchant(ctx, database, a.ID.String())
	require.NoError(t, err)
	require.Equal(t, b.ID, selected, "deleting A does not change B's public name")
}
