//go:build integration

package dbtest

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	authkitembedded "github.com/open-rails/authkit/embedded"
	"github.com/stretchr/testify/require"
)

// ApplyAuthKitMigrations asks AuthKit to initialize its own schema. The test
// harness deliberately does not import AuthKit's private migration source or
// migratekit.
func ApplyAuthKitMigrations(t *testing.T, ctx context.Context, pool *pgxpool.Pool, schema string) {
	t.Helper()
	require.NoError(t, authkitembedded.ApplyMigrations(ctx, pool, schema))
}
