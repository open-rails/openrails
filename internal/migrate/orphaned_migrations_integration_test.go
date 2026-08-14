//go:build integration

package migrate_test

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/open-rails/migratekit"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/migrate"
	postgresmigrations "github.com/open-rails/openrails/migrations/postgres"
)

// or#901: reproduce, through the REAL migrator entrypoint, the condition that
// silently killed both money-path reconcile jobs for 15 days.
//
// Both e2e stacks recorded openrails migrations 1..12 (from a tensorhub pin
// whose chain reached 12). or#893's re-squash then folded 0002-0085 into a
// rewritten 0001 and deleted the rest, leaving the embedded set {0001, 0002}.
// migratekit only ever asks "is every embedded migration applied?", so names
// "1" and "2" both read as already done, NOTHING is applied, and the database
// keeps the old schema — which has no openrails.psp_rail_merchant_ids — while
// the binary runs code that calls it. Every catalog_reconciliation_pull then
// failed SQLSTATE 42883, 25 times per job, with no other signal anywhere.
//
// RunPostgres must refuse such a database instead of reporting success.

const orphanedSchema = "openrails" // the production default; the stacks' schema

func seedAppliedMigration(t *testing.T, sqlDB *sql.DB, name string) {
	t.Helper()
	_, err := sqlDB.ExecContext(context.Background(),
		`INSERT INTO public.migrations (app, database, name, schema) VALUES ($1, 'postgres', $2, $3)
		 ON CONFLICT DO NOTHING`,
		config.MigratekitApp, name, orphanedSchema)
	require.NoError(t, err)
}

func TestRunPostgres_RefusesOrphanedMigrations(t *testing.T) {
	ctx := context.Background()
	// An isolated, fully-migrated database built by the real migrator.
	dsn := dbtest.SharedSuperuserDSN(t)
	sqlDB, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = sqlDB.Close() })

	// Orphans start one past the highest prefix the build actually carries, so
	// adding a real migration never turns this fixture into a live prefix.
	orphans := orphanPrefixesPastEmbedded(t, 10)
	t.Cleanup(func() {
		for _, n := range orphans {
			_, _ = sqlDB.ExecContext(context.Background(),
				`DELETE FROM public.migrations WHERE app = $1 AND database = 'postgres' AND schema = $2 AND name = $3`,
				config.MigratekitApp, orphanedSchema, n)
		}
	})

	cfg := &config.Config{Env: "dev", DB: &config.DBConfig{URL: dsn}}

	// Re-running the migrator over a database it built is a clean no-op.
	require.NoError(t, migrate.RunPostgres(ctx, cfg),
		"a database whose applied set matches the embedded set migrates cleanly")

	// Now make it the stacks' database: migrations recorded that this build no
	// longer carries anywhere.
	for _, n := range orphans {
		seedAppliedMigration(t, sqlDB, n)
	}

	err = migrate.RunPostgres(ctx, cfg)
	require.Error(t, err, "a re-squash that can never re-apply must refuse, not report success")

	var orphanErr *migrate.OrphanedMigrationsError
	require.ErrorAs(t, err, &orphanErr, "the refusal is typed")
	require.Equal(t, orphanedSchema, orphanErr.Schema)
	require.Equal(t, orphans, orphanErr.Orphaned, "every orphan named, in numeric order")
	require.Contains(t, orphanErr.Error(), "does not carry")

	// The control that matters most: migratekit itself is still perfectly happy
	// with this database. Its two checks (ApplyMigrations, ValidateAllApplied)
	// only ask embedded-subset-of-applied, never the converse — so without the
	// fence a frozen schema is indistinguishable from an up-to-date one.
	migrations, err := migratekit.LoadFromFS(postgresmigrations.FS)
	require.NoError(t, err)
	m := migratekit.NewPostgres(sqlDB, config.MigratekitApp).WithSchema(orphanedSchema)
	require.NoError(t, m.ValidateAllApplied(ctx, migrations),
		"this is the hole or#901 closes: migratekit reports a frozen schema as fully migrated")
}

// orphanPrefixesPastEmbedded returns n numeric prefixes starting just past the
// highest *.up.sql prefix in the embedded postgres migration set.
func orphanPrefixesPastEmbedded(t *testing.T, n int) []string {
	t.Helper()
	entries, err := postgresmigrations.FS.ReadDir(".")
	require.NoError(t, err)
	maxPrefix := 0
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".up.sql") {
			continue
		}
		var p int
		if _, err := fmt.Sscanf(name, "%d_", &p); err == nil && p > maxPrefix {
			maxPrefix = p
		}
	}
	require.Positive(t, maxPrefix, "no migrations found in the embedded FS")
	out := make([]string, 0, n)
	for i := 1; i <= n; i++ {
		out = append(out, strconv.Itoa(maxPrefix+i))
	}
	return out
}
