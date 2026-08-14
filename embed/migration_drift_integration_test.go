//go:build integration

package embed_test

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
	"github.com/open-rails/openrails/embed"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/migrate"
	postgresmigrations "github.com/open-rails/openrails/migrations/postgres"
	"github.com/open-rails/openrails/pkg/embedded"
)

// th#1627 / or#901: an EMBEDDED host must refuse to boot on a database whose
// migration ledger records names this build no longer carries.
//
// or#901 installed the drift fence in migrate.RunPostgres — the STANDALONE
// binary's entrypoint. An embedded host never calls it: tensorhub applies the
// migratekit chain itself and depends on the engine's own init-time validation
// (internal/app.validateDatabase), which only asked migratekit's one question,
// "is every embedded migration applied?". A re-squashed chain answers yes while
// applying nothing.
//
// The bill was th#1627: tensorhub's dev stack recorded openrails 1..12, the
// post-squash embedded set is {1, 2}, so the openrails schema stayed frozen at
// the pre-squash shape. openrails.billing_policies did not exist, and the
// trust-level ladder sync 500'd at every boot while every billed admission
// answered `403 credit_authorize_failed ... status=500 admission check failed`.
// Both ValidatePostgresMigrations and ApplyMigrations reported success
// throughout.
func TestEmbeddedRuntimeRefusesOrphanedMigrations(t *testing.T) {
	ctx := context.Background()

	const schema = config.DefaultSchema
	const cleanupPostgresMigration = `DELETE FROM public.migrations
		WHERE app = $1 AND database = 'postgres' AND schema = $2 AND name = $3`
	// Orphans start one past the highest prefix the build actually carries, so
	// adding a real migration never turns this fixture into a live prefix. It
	// used to be hardcoded from "3", which migration 0003 made real: the INSERT
	// then shadowed the genuine ledger row and the cleanup DELETE — matched on
	// (app, schema, name) only — removed it, leaving 0003 recorded as pending
	// and every later embed.New in the package refusing to boot.
	orphans := orphanPrefixesPastEmbedded(t, 10)

	superDSN := dbtest.SharedSuperuserDSN(t)
	sqlDB, err := sql.Open("pgx", superDSN)
	require.NoError(t, err)
	t.Cleanup(func() { _ = sqlDB.Close() })
	t.Cleanup(func() {
		for _, n := range orphans {
			_, _ = sqlDB.ExecContext(context.Background(), cleanupPostgresMigration,
				config.MigratekitApp, schema, n)
		}
	})

	// A same-key ClickHouse ledger row proves cleanup is database-scoped. The
	// old predicate omitted database and once deleted a real row from the other
	// migration ledger when a generated orphan prefix collided with it.
	sentinel := orphans[0]
	_, err = sqlDB.ExecContext(ctx,
		`INSERT INTO public.migrations (app, database, name, schema) VALUES ($1, 'clickhouse', $2, $3)`,
		config.MigratekitApp, sentinel, schema)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = sqlDB.ExecContext(context.Background(),
			`DELETE FROM public.migrations WHERE app = $1 AND database = 'clickhouse' AND schema = $2 AND name = $3`,
			config.MigratekitApp, schema, sentinel)
	})

	dsn := dbtest.SharedPostgresDSN(t)
	rdb, _ := dbtest.SharedRedisClient(t)
	newRuntime := func() (*embed.Runtime, error) {
		cfg := &config.Config{Env: "dev", TestMode: config.CredentialPostureLive, DB: &config.DBConfig{URL: dsn}}
		return embed.New(ctx, embed.Options{Options: embedded.Options{
			Config: cfg, Redis: rdb, River: embedded.RiverManagedByOpenRails(),
		}})
	}

	// Control: the same host boots cleanly when the ledger matches the build.
	rt, err := newRuntime()
	require.NoError(t, err, "an in-sync database must still boot")
	require.NoError(t, rt.Close(context.Background()))

	// Make it the stacks' database.
	for _, n := range orphans {
		_, err := sqlDB.ExecContext(ctx,
			`INSERT INTO public.migrations (app, database, name, schema) VALUES ($1, 'postgres', $2, $3)
			 ON CONFLICT DO NOTHING`,
			config.MigratekitApp, n, schema)
		require.NoError(t, err)
	}

	// This is the hole: migratekit's own validator still calls the frozen
	// database fully migrated, which is exactly why th#1627 reached production
	// as 500s instead of a failed deploy.
	migrations, err := migratekit.LoadFromFS(postgresmigrations.FS)
	require.NoError(t, err)
	require.NoError(t,
		migratekit.NewPostgres(sqlDB, config.MigratekitApp).WithSchema(schema).ValidateAllApplied(ctx, migrations),
		"migratekit reports a frozen schema as fully migrated — the fence is the only signal")

	drifted, err := newRuntime()
	if drifted != nil {
		t.Cleanup(func() { _ = drifted.Close(context.Background()) })
	}
	require.Error(t, err, "the embedded engine must refuse a drifted schema, not serve 500s on the money path")

	var orphanErr *migrate.OrphanedMigrationsError
	require.ErrorAs(t, err, &orphanErr, "the refusal is typed, so a host can act on it")
	require.Equal(t, schema, orphanErr.Schema)
	require.Equal(t, orphans, orphanErr.Orphaned, "every orphan named, in numeric order")

	_, err = sqlDB.ExecContext(ctx, cleanupPostgresMigration, config.MigratekitApp, schema, sentinel)
	require.NoError(t, err)
	var clickHouseSentinel int
	require.NoError(t, sqlDB.QueryRowContext(ctx, `
		SELECT count(*) FROM public.migrations
		 WHERE app = $1 AND database = 'clickhouse' AND schema = $2 AND name = $3`,
		config.MigratekitApp, schema, sentinel).Scan(&clickHouseSentinel))
	require.Equal(t, 1, clickHouseSentinel,
		"Postgres orphan cleanup must not delete a same-key ClickHouse migration")
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
