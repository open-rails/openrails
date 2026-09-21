//go:build integration

package authkit_test

import (
	"context"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	authcore "github.com/open-rails/authkit/embedded"
	billingauthkit "github.com/open-rails/openrails/embed/authkit"
	"github.com/stretchr/testify/require"
)

// Exercise the public adapter against a real AuthKit client and a separately
// initialized identity schema, without creating any billing tables or grants.
func TestDirectoryPublicClient(t *testing.T) {
	dsn := os.Getenv("OPENRAILS_TEST_DB_DSN")
	if dsn == "" {
		t.Skip("OPENRAILS_TEST_DB_DSN is required")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	defer pool.Close()
	schema := "identity_" + uuid.New().String()[:8]
	require.NoError(t, authcore.ApplyMigrations(ctx, pool, schema))
	defer func() {
		_, err := pool.Exec(ctx, "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
		require.NoError(t, err)
	}()
	core, err := authcore.New(authcore.Config{
		Schema:    schema,
		Keys:      authcore.KeysConfig{VerifyOnly: true},
		Token:     authcore.TokenConfig{Issuer: "https://identity.test", IssuedAudiences: []string{"test"}},
		Ephemeral: authcore.EphemeralConfig{AllowMemory: true},
	}, authcore.Deps{Postgres: pool})
	require.NoError(t, err)
	defer core.Close()
	directory := billingauthkit.NewDirectory(core)
	user, err := core.CreateUser(ctx, "billing@example.test", "billing-user")
	require.NoError(t, err)
	username, email, ok, err := directory.EmailIdentity(ctx, user.ID)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "billing-user", username)
	require.Equal(t, "billing@example.test", email)
	exists, err := directory.Exists(ctx, user.ID)
	require.NoError(t, err)
	require.True(t, exists)
	resolved, err := directory.GetUserIDByUsername(ctx, "billing-user")
	require.NoError(t, err)
	require.Equal(t, user.ID, resolved)
	for _, id := range []string{"invalid", uuid.NewString()} {
		_, _, ok, err := directory.EmailIdentity(ctx, id)
		require.NoError(t, err)
		require.False(t, ok)
	}
	require.NoError(t, core.SoftDeleteUser(ctx, user.ID))
	_, _, ok, err = directory.EmailIdentity(ctx, user.ID)
	require.NoError(t, err)
	require.False(t, ok)
	_, err = directory.GetUserIDByUsername(ctx, "billing-user")
	require.Error(t, err)
}
