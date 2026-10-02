//go:build greenfield && integration

package ci_test

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/open-rails/migratekit"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/embed"
	"github.com/open-rails/openrails/internal/migrate/retired"
)

// A database built by the retired v0.147–v0.152 chain (0001 plus
// 0002_creator_catalogs) converts in place: the schema ends equal to a fresh
// one, its rows survive, and a runtime serves catalog writes on it.
//
// OPENRAILS_LEGACY_SCHEMA_DUMPS names schema-only pg_dumps of real databases
// (comma-separated paths, one OpenRails schema each) to convert as well.
func TestRetiredChainConvertsInPlace(t *testing.T) {
	type source struct {
		name, schema string
		build        func(t *testing.T, f *fixture)
	}
	sources := []source{{
		name:   "v0.147 chain",
		schema: "retired_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12],
		build: func(t *testing.T, f *fixture) {
			chain, err := retired.Conversions()[0].Retired(f.schema)
			require.NoError(t, err)
			require.NoError(t, migratekit.NewPostgres(sqlHandle(t), config.MigratekitApp).WithSchema(f.schema).ApplyMigrations(t.Context(), chain))
		},
	}}
	for _, path := range strings.Split(os.Getenv("OPENRAILS_LEGACY_SCHEMA_DUMPS"), ",") {
		if path = strings.TrimSpace(path); path == "" {
			continue
		}
		dump, err := os.ReadFile(path)
		require.NoError(t, err)
		m := regexp.MustCompile(`(?m)^CREATE SCHEMA ([A-Za-z0-9_]+);$`).FindAllStringSubmatch(string(dump), -1)
		require.Len(t, m, 1, "a legacy dump holds exactly one OpenRails schema")
		sources = append(sources, source{name: filepath.Base(path), schema: m[0][1], build: func(t *testing.T, f *fixture) {
			restoreRetiredDump(t, f, string(dump))
		}})
	}

	for _, src := range sources {
		t.Run(src.name, func(t *testing.T) {
			ctx := t.Context()
			f := legacyFixture(t, src.schema)
			src.build(t, f)
			var merchantID string
			slug := "legacy-" + strings.ReplaceAll(uuid.NewString(), "-", "")[:8]
			require.NoError(t, f.pool.QueryRow(ctx, `INSERT INTO `+f.q()+`.merchants (slug, display_name) VALUES ($1, 'Legacy Merchant') RETURNING id`, slug).Scan(&merchantID))
			_, err := f.pool.Exec(ctx, `INSERT INTO `+f.q()+`.merchant_configurations (merchant_id, config) VALUES ($1, '{"display_name":"Legacy Merchant"}')`, merchantID)
			require.NoError(t, err)

			for i := range 2 {
				started := time.Now()
				require.NoError(t, embed.ApplyMigrations(ctx, f.pool, embed.MigrationOptions{
					Schema: f.schema, River: embed.RiverManagedByOpenRails(f.schema), RuntimePool: f.pool,
				}))
				t.Logf("boot %d: migrations took %s", i+1, time.Since(started).Round(time.Millisecond))
			}
			fresh := newFixture(t)
			diff, err := migratekit.SchemaDiff(ctx, sqlHandle(t), f.schema, fresh.schema)
			require.NoError(t, err)
			require.Empty(t, diff, "converted schema differs from a fresh one")

			var converted int
			require.NoError(t, f.pool.QueryRow(ctx, `SELECT count(*) FROM public.migration_repairs WHERE app=$1 AND schema=$2 AND verb='convert'`, config.MigratekitApp, f.schema).Scan(&converted))
			require.NotZero(t, converted)
			var revision int64
			var display string
			require.NoError(t, f.pool.QueryRow(ctx, `SELECT m.catalog_revision, c.config->>'display_name' FROM `+f.q()+`.merchants m JOIN `+f.q()+`.merchant_configurations c ON c.merchant_id=m.id WHERE m.id=$1`, merchantID).Scan(&revision, &display))
			require.Equal(t, int64(0), revision)
			require.Equal(t, "Legacy Merchant", display)

			_, client := f.runtime(t, slug)
			_, err = client.Products.Create(ctx, &openrails.ProductCreateParams{
				Key: "converted-" + uuid.NewString()[:8], DisplayName: "Converted", EntitlementsSpec: map[string]*int{"content:converted": nil},
			})
			require.NoError(t, err)
			require.NoError(t, f.pool.QueryRow(ctx, `SELECT catalog_revision FROM `+f.q()+`.merchants WHERE slug=$1`, slug).Scan(&revision))
			require.Positive(t, revision, "catalog authoring advances the merchant's revision on the converted schema")
		})
	}
}

// legacyFixture owns a schema that starts empty, unlike newFixture.
func legacyFixture(t *testing.T, schema string) *fixture {
	t.Helper()
	f := &fixture{schema: schema}
	pool, err := pgxpool.New(t.Context(), strings.TrimSpace(os.Getenv("OPENRAILS_GREENFIELD_DSN")))
	require.NoError(t, err)
	f.pool = pool
	cleanup := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = pool.Exec(ctx, "DROP SCHEMA IF EXISTS "+f.q()+" CASCADE")
		_, _ = pool.Exec(ctx, `DELETE FROM public.migrations WHERE app=$1 AND schema=$2`, config.MigratekitApp, schema)
		_, _ = pool.Exec(ctx, `DELETE FROM public.migration_repairs WHERE app=$1 AND schema=$2`, config.MigratekitApp, schema)
	}
	cleanup()
	t.Cleanup(func() { cleanup(); pool.Close() })
	return f
}

func (f *fixture) q() string { return pgx.Identifier{f.schema}.Sanitize() }

// restoreRetiredDump restores a schema-only pg_dump and records the ledger the
// retired release wrote, which pg_dump -n does not carry.
func restoreRetiredDump(t *testing.T, f *fixture, dump string) {
	t.Helper()
	ctx := t.Context()
	for _, ext := range []string{"pgcrypto", "btree_gist", "citext"} {
		_, err := f.pool.Exec(ctx, "CREATE EXTENSION IF NOT EXISTS "+ext+" WITH SCHEMA public")
		require.NoError(t, err)
	}
	conn, err := pgx.Connect(ctx, f.dsn(t))
	require.NoError(t, err)
	defer conn.Close(context.Background())
	_, err = conn.Exec(ctx, regexp.MustCompile(`(?m)^\\(un)?restrict .*$`).ReplaceAllString(dump, ""))
	require.NoError(t, err)
	db := sqlHandle(t)
	require.NoError(t, migratekit.NewPostgres(db, config.MigratekitApp).ApplyMigrations(ctx, nil))
	chain, err := retired.Conversions()[0].Retired(f.schema)
	require.NoError(t, err)
	for i, m := range chain {
		_, err := db.ExecContext(ctx, `INSERT INTO public.migrations (app, database, schema, sequence, filename, content_sha256, semantic_sha256)
			VALUES ($1, 'postgres', $2, $3, $4, $5, $6)`, config.MigratekitApp, f.schema, i+1, m.Name,
			migratekit.ContentDigest(m.Content), migratekit.SemanticContentDigest(m.Content))
		require.NoError(t, err)
	}
}

func sqlHandle(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("pgx", strings.TrimSpace(os.Getenv("OPENRAILS_GREENFIELD_DSN")))
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	return db
}
