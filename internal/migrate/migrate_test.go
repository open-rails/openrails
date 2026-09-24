package migrate

import (
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/migratekit"
	"github.com/open-rails/openrails/config"
	postgresmigrations "github.com/open-rails/openrails/internal/migrate/postgres"
	"github.com/stretchr/testify/require"
)

func TestResetTargetIsOneExactIdentity(t *testing.T) {
	cfg, err := pgx.ParseConfig("postgresql://user@db.internal:5432/app")
	require.NoError(t, err)
	cfg.Host = " DB.Internal "
	require.Equal(t, "db.internal:5432/app", canonicalResetTarget(cfg))
	cfg.Host, cfg.Port, cfg.Database = "2001:db8::1", 5433, "app/test"
	require.Equal(t, "[2001:db8::1]:5433/app%2Ftest", canonicalResetTarget(cfg))

	cfg, err = pgx.ParseConfig("host=dev.internal,prod.internal port=5432 dbname=app sslmode=disable")
	require.NoError(t, err)
	require.ErrorContains(t, validateResetConnectionConfig(cfg), "multiple host targets")
	cfg, err = pgx.ParseConfig("host=dev.internal dbname=app sslmode=prefer")
	require.NoError(t, err)
	require.NoError(t, validateResetConnectionConfig(cfg), "TLS fallback on the same host is one target")

	for name, tc := range map[string]struct{ target, allowed, confirmation, err string }{
		"production-looking name allowed by exact identity": {"localhost:5432/productivity", "other:1/x, localhost:5432/productivity", "reset-openrails@localhost:5432/productivity", ""},
		"not allow-listed":                   {"db.internal:5432/app", "", "reset-openrails@db.internal:5432/app", "not allow-listed"},
		"same database on another host":      {"prod.internal:5432/app", "dev.internal:5432/app", "reset-openrails@prod.internal:5432/app", "not allow-listed"},
		"allow-list prefix is not a match":   {"localhost:5432/app", "localhost:5432/app2", "reset-openrails@localhost:5432/app", "not allow-listed"},
		"confirmation for another database":  {"localhost:5432/app", "localhost:5432/app", "reset-openrails@localhost:5432/other", "does not match"},
		"confirmation without target prefix": {"localhost:5432/app", "localhost:5432/app", "localhost:5432/app", "does not match"},
	} {
		err := validateResetAuthorization(tc.target, tc.allowed, tc.confirmation)
		if tc.err == "" {
			require.NoError(t, err, name)
		} else {
			require.ErrorContains(t, err, tc.err, name)
		}
	}
	report := EmbeddedResetPlan{Target: "localhost:5432/app", LedgerRows: []string{"1", "2"}}.Report()
	require.Contains(t, report, "DROP SCHEMA IF EXISTS billing CASCADE")
	require.Contains(t, report, "confirmation token: reset-openrails@localhost:5432/app")
	require.NotContains(t, report, "user@", "the plan never echoes DSN credentials")
}

func TestStatusRequiresAnExactVerifiableLedger(t *testing.T) {
	var migrations []migratekit.Migration
	for _, name := range []string{"0001_one", "0002_two", "0003_three", "0004_four", "0005_five"} {
		migrations = append(migrations, migratekit.Migration{Name: name + ".up.sql", Content: "SELECT '" + name + "';"})
	}
	exact := func(m migratekit.Migration) migratekit.AppliedRecord {
		return migratekit.AppliedRecord{Key: migratekit.Prefix(m.Name), Filename: m.Name, Digest: migratekit.ContentDigest(m.Content), SemanticDigest: migratekit.SemanticContentDigest(m.Content), Status: "applied"}
	}
	var all []migratekit.AppliedRecord
	for _, m := range migrations {
		all = append(all, exact(m))
	}
	report := buildPostgresStatus(migratekit.Status{App: "openrails", Schema: "billing", Applied: all}, migrations)
	require.True(t, report.Exact)
	require.Empty(t, report.Missing)
	require.Empty(t, report.Orphaned)
	require.Empty(t, report.Drift)

	renamed := exact(migrations[1])
	renamed.Filename, renamed.Digest, renamed.SemanticDigest = "0002_other.up.sql", "wrong", "wrong"
	failed := exact(migrations[3])
	failed.Status = "failed"
	report = buildPostgresStatus(migratekit.Status{App: "openrails", Schema: "billing", Applied: []migratekit.AppliedRecord{
		exact(migrations[0]), renamed, {Key: "3"}, failed,
		{Key: "6", Filename: "0006_orphan.up.sql", Digest: "orphan", Status: "applied"},
	}}, migrations)
	require.False(t, report.Exact)
	require.Equal(t, []EmbeddedMigration{report.Embedded[4]}, report.Missing)
	require.Len(t, report.Orphaned, 1)
	require.Equal(t, "6", report.Orphaned[0].Key)
	kinds := map[string]bool{}
	for _, d := range report.Drift {
		kinds[d.Kind] = true
	}
	for _, kind := range []string{"filename_mismatch", "content_hash_mismatch", "semantic_hash_mismatch", "unverifiable_legacy_row", "unfinished"} {
		require.True(t, kinds[kind], kind)
	}
	for _, section := range []string{"exact: false", "MISSING", "ORPHANED", "DRIFT"} {
		require.Contains(t, report.Report(), section)
	}

	// Only whitespace/comments changed: content drift, semantic identity kept.
	cosmetic := exact(migrations[0])
	cosmetic.Digest = migratekit.ContentDigest(migrations[0].Content + "\n-- note")
	cosmetic.SemanticDigest = migratekit.SemanticContentDigest(migrations[0].Content + "\n-- note")
	report = buildPostgresStatus(migratekit.Status{Applied: append([]migratekit.AppliedRecord{cosmetic}, all[1:]...)}, migrations)
	require.Len(t, report.Drift, 1)
	require.Equal(t, "content_hash_mismatch", report.Drift[0].Kind)

	names := []string{"10", "2", "legacy", "1"}
	sortMigrationNames(names)
	require.Equal(t, []string{"1", "2", "10", "legacy"}, names)
	require.Contains(t, (&OrphanedMigrationsError{Schema: "billing", Orphaned: []string{"9"}, Embedded: []string{"1"}}).Error(), "do NOT reset")
}

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

	got, err := postgresmigrations.RewriteSchema(`-- openrails stays documentation
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
		got, err := postgresmigrations.RewriteSchema(quoted, "shop")
		require.NoError(t, err)
		require.Equal(t, quoted, got)
	}
	for _, bad := range []string{"SELECT 'unfinished", "SELECT \"unfinished", "/* outer /* inner */", "DO $body$BEGIN"} {
		_, err := postgresmigrations.RewriteSchema(bad, "shop")
		require.Error(t, err, bad)
	}

	// Every default deployment runs the embedded chain through this rewrite.
	embedded, err := migratekit.LoadFromFS(postgresmigrations.FS)
	require.NoError(t, err)
	require.NotEmpty(t, embedded)
	relocated, err := rewriteMigrationsSchema(embedded, config.DefaultSchema)
	require.NoError(t, err)
	for _, m := range relocated {
		require.NotContains(t, m.Content, "CREATE TABLE openrails.", m.Name)
	}
}
