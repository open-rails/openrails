package migrate

import (
	"strings"
	"testing"

	"github.com/open-rails/migratekit"
	"github.com/open-rails/openrails/config"
	"github.com/stretchr/testify/require"
)

func TestSchemaRelocation(t *testing.T) {
	in := []migratekit.Migration{{Name: "0001_x.up.sql", Content: "CREATE SCHEMA IF NOT EXISTS openrails;\n" +
		"CREATE TABLE openrails.merchants (id uuid REFERENCES openrails.merchants);\n" +
		"GRANT USAGE ON SCHEMA openrails TO host_runtime;\n" +
		"ALTER DEFAULT PRIVILEGES IN SCHEMA openrails GRANT SELECT ON TABLES TO host_runtime;\n" +
		"-- OpenRails billing schema prose\nCREATE ROLE host_runtime NOLOGIN;"}}
	out, err := rewriteMigrationsSchema(in, config.CanonicalSchema)
	require.NoError(t, err)
	require.Equal(t, in[0].Content, out[0].Content)
	for schema, want := range map[string]string{"": "billing", config.DefaultSchema: "billing", "shop": "shop"} {
		out, err := rewriteMigrationsSchema(in, schema)
		require.NoError(t, err)
		require.Equal(t, strings.ReplaceAll(in[0].Content, "openrails", want), out[0].Content, schema)
		require.Contains(t, in[0].Content, "CREATE TABLE openrails.", "input must not be mutated")
	}
	_, err = rewriteMigrationsSchema([]migratekit.Migration{{Name: "0009_bad.up.sql", Content: "SELECT 'unfinished"}}, "shop")
	require.ErrorContains(t, err, "0009_bad.up.sql")

	got, err := rewriteSchema(`-- openrails stays documentation
/* outer openrails /* nested openrails */ comment */
CREATE TABLE "openrails".payment_methods (rebill_driver text CHECK (rebill_driver IN ('openrails','provider')), note text DEFAULT 'openrails.payment_methods');
CREATE FUNCTION openrails.sample() RETURNS text SET search_path TO 'pg_catalog', 'openrails' LANGUAGE plpgsql AS $body$
BEGIN
 PERFORM 'openrails.payment_methods'::regclass;
 PERFORM 1 FROM openrails.payment_methods;
 RETURN 'openrails';
END
$body$;`, "shop")
	require.NoError(t, err)
	for _, kept := range []string{"-- openrails stays documentation", "/* outer openrails /* nested openrails */ comment */", "IN ('openrails','provider')", "DEFAULT 'openrails.payment_methods'", "RETURN 'openrails'"} {
		require.Contains(t, got, kept, "domain values and comments are data")
	}
	for _, moved := range []string{"CREATE TABLE shop.payment_methods", "CREATE FUNCTION shop.sample", "'pg_catalog', 'shop'", "'shop.payment_methods'::regclass", "FROM shop.payment_methods"} {
		require.Contains(t, got, moved)
	}
	for _, quoted := range []string{"SELECT $$FROM openrails.payment_methods$$, $tag$openrails$tag$;", `SELECT 'it''s openrails', E'it\'s openrails', "not openrails";`} {
		got, err := rewriteSchema(quoted, "shop")
		require.NoError(t, err)
		require.Equal(t, quoted, got)
	}
	for _, bad := range []string{"SELECT 'unfinished", "SELECT \"unfinished", "/* outer /* inner */", "DO $body$BEGIN"} {
		_, err := rewriteSchema(bad, "shop")
		require.Error(t, err, bad)
	}

	// Every default deployment runs the embedded chain through this rewrite.
	relocated, err := loadMigrations(config.DefaultSchema)
	require.NoError(t, err)
	require.NotEmpty(t, relocated)
	for _, m := range relocated {
		require.NotContains(t, m.Content, "CREATE TABLE openrails.", m.Name)
	}
	require.Contains(t, relocated[0].Content, "source IN ('openrails', 'provider_schedule', 'external')", "domain values are not schema references")
}
