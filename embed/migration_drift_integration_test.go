//go:build integration

package embed_test

import (
	"context"
	"database/sql"
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
	orphans := []string{"3", "4", "5", "6", "7", "8", "9", "10", "11", "12"}

	superDSN := dbtest.SharedSuperuserDSN(t)
	sqlDB, err := sql.Open("pgx", superDSN)
	require.NoError(t, err)
	t.Cleanup(func() { _ = sqlDB.Close() })
	t.Cleanup(func() {
		for _, n := range orphans {
			_, _ = sqlDB.ExecContext(context.Background(),
				`DELETE FROM public.migrations WHERE app = $1 AND schema = $2 AND name = $3`,
				config.MigratekitApp, schema, n)
		}
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
}
