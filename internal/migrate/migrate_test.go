package migrate

import (
	"testing"

	"github.com/open-rails/migratekit"
	"github.com/open-rails/openrails/config"
	postgresmigrations "github.com/open-rails/openrails/internal/migrate/postgres"
	"github.com/stretchr/testify/require"
)

func TestSchemaRelocation(t *testing.T) {
	authored, err := migratekit.LoadFromFS(postgresmigrations.FS)
	require.NoError(t, err)

	// The default schema runs the authored files byte for byte.
	for _, schema := range []string{"", config.DefaultSchema} {
		got, err := loadMigrations(schema)
		require.NoError(t, err)
		require.Equal(t, authored, got, schema)
	}

	shop, err := loadMigrations("shop")
	require.NoError(t, err)
	require.Contains(t, shop[0].Content, "CREATE SCHEMA IF NOT EXISTS shop;")
	require.Contains(t, shop[0].Content, "CREATE TABLE shop.merchants")
	require.NotContains(t, shop[0].Content, "CREATE TABLE billing.")
	require.Contains(t, shop[0].Content, "source IN ('openrails', 'provider_schedule', 'external')", "domain values are not schema references")
	require.Contains(t, authored[0].Content, "CREATE TABLE billing.merchants", "input must not be mutated")

	_, err = rewriteMigrationsSchema([]migratekit.Migration{{Name: "0009_bad.up.sql", Content: "SELECT 'unfinished"}}, "shop")
	require.ErrorContains(t, err, "0009_bad.up.sql")
}
