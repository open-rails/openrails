//go:build e2e && integration

package ci_test

import (
	"context"
	"io/fs"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/internal/app"
	postgresmigrations "github.com/open-rails/openrails/internal/migrate/postgres"
)

// emptyDatabase creates a database for one test and returns a DSN naming it.
func emptyDatabase(t *testing.T) string {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv("OPENRAILS_E2E_DSN"))
	require.NotEmpty(t, dsn, "OPENRAILS_E2E_DSN must point at a disposable PostgreSQL server")
	admin, err := pgx.Connect(t.Context(), dsn)
	require.NoError(t, err)
	name := "new_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
	_, err = admin.Exec(t.Context(), "CREATE DATABASE "+pgx.Identifier{name}.Sanitize())
	require.NoError(t, err)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_, _ = admin.Exec(ctx, "DROP DATABASE IF EXISTS "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)")
		_ = admin.Close(ctx)
	})
	u, err := url.Parse(dsn)
	require.NoError(t, err)
	u.Path = "/" + name
	return u.String()
}

func openPool(t *testing.T, dsn string) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(t.Context(), dsn)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	return pool
}

// The least a host writes: the two posture fields and a pool on an empty
// database. No schema, catalog, merchant or PSP.
func TestMinimalNewOnAnEmptyDatabase(t *testing.T) {
	pool := openPool(t, emptyDatabase(t))
	client, err := openrails.New(t.Context(), openrails.Config{
		TestMode:          openrails.Sandbox,
		ProviderWriteMode: openrails.ProviderWritesReadOnly,
	}, openrails.Deps{Postgres: pool})
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close(context.Background()) })

	for _, table := range []string{"billing.merchants", "billing_river.river_job"} {
		var found *string
		require.NoError(t, pool.QueryRow(t.Context(), "SELECT to_regclass($1)::text", table).Scan(&found))
		require.NotNil(t, found, table)
	}
	require.NoError(t, client.Start(t.Context()))
	require.Eventually(t, func() bool { return client.Ready(t.Context()) == nil }, 20*time.Second, 50*time.Millisecond)
	require.NoError(t, client.Close(t.Context()))
}

// A misspelled PSP setting or secret refuses New, naming the PSP, the key and
// the keys its rail takes, before anything is migrated.
func TestNewRefusesUnknownPSPKeys(t *testing.T) {
	pool := openPool(t, emptyDatabase(t))
	for want, psp := range map[string]openrails.PSPConfig{
		`openrails: Config.Merchant.psps.p.settings: unknown field "tokenisation_key" (nmi takes card_entry, endpoint_deployment, tokenization_key, tokenization_url, webhook_overlap_expires_at)`: {
			Rail: "nmi", AccountID: "100001", Settings: map[string]any{"tokenisation_key": "tk"},
		},
		`openrails: Config.Merchant.psps.p.secrets: unknown field "secret_kye" (stripe takes secret_key, webhook_signing_secret, webhook_signing_secret_previous, webhook_signing_secret_thin)`: {
			Rail: "stripe", AccountID: "acct_1", Secrets: map[string]string{"secret_kye": "sk_test_1"},
		},
	} {
		_, err := openrails.New(t.Context(), openrails.Config{
			TestMode: openrails.Sandbox, ProviderWriteMode: openrails.ProviderWritesReadOnly,
			Merchant: openrails.MerchantDeclaration{Slug: "host-one", DisplayName: "Host One", PSPs: map[string]openrails.PSPConfig{"p": psp}},
		}, openrails.Deps{Postgres: pool})
		require.EqualError(t, err, want)
	}
	var found *string
	require.NoError(t, pool.QueryRow(t.Context(), "SELECT to_regclass('billing.merchants')::text").Scan(&found))
	require.Nil(t, found, "refused before migrating")
}

// Replicas boot together on an empty database: each New migrates, migratekit
// and River serialize them, and every one comes up. Booting together into a
// new month, they race to create the same partitions.
func TestReplicasRaceNewOnAnEmptyDatabase(t *testing.T) {
	dsn := emptyDatabase(t)
	cfg := openrails.Config{TestMode: openrails.Sandbox, ProviderWriteMode: openrails.ProviderWritesReadOnly}
	pools := make([]*pgxpool.Pool, 4)
	for i := range pools {
		pools[i] = openPool(t, dsn)
	}
	boot := func() {
		t.Helper()
		start := make(chan struct{})
		errs := make([]error, len(pools))
		var wg sync.WaitGroup
		for i, pool := range pools {
			wg.Go(func() {
				<-start
				client, err := openrails.New(t.Context(), cfg, openrails.Deps{Postgres: pool})
				if err == nil {
					err = client.Close(context.Background())
				}
				errs[i] = err
			})
		}
		close(start)
		wg.Wait()
		for i, err := range errs {
			require.NoError(t, err, "replica %d", i)
		}
	}
	boot()
	var applied int
	require.NoError(t, pools[0].QueryRow(t.Context(), `SELECT count(*) FROM public.migrations WHERE app = 'openrails'`).Scan(&applied))
	files, err := fs.Glob(postgresmigrations.FS, "*.up.sql")
	require.NoError(t, err)
	require.Equal(t, len(files), applied, "each migration applied once")

	latest := func(table string) (partition string) {
		require.NoError(t, pools[0].QueryRow(t.Context(),
			`SELECT partition FROM billing.month_partitions($1) ORDER BY range_from DESC LIMIT 1`, table).Scan(&partition))
		return partition
	}
	tables := map[string]string{"usage_events": "", "admission_operations": ""}
	for table := range tables {
		tables[table] = latest(table)
		_, err := pools[0].Exec(t.Context(), "DROP TABLE "+pgx.Identifier{"billing", tables[table]}.Sanitize())
		require.NoError(t, err)
	}
	boot()
	for table, partition := range tables {
		require.Equal(t, partition, latest(table))
	}
}

// Rolling back a deploy: a build one migration behind boots against the
// schema the newer build migrated, and leaves the newer migration in place.
func TestOlderBuildBootsOnANewerSchema(t *testing.T) {
	f := newFixture(t) // this build migrated the schema
	files, err := fs.Glob(postgresmigrations.FS, "*.up.sql")
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(files), 2)
	newest := files[len(files)-1]
	older := fstest.MapFS{}
	for _, name := range files[:len(files)-1] {
		body, err := fs.ReadFile(postgresmigrations.FS, name)
		require.NoError(t, err)
		older[name] = &fstest.MapFile{Data: body}
	}

	cfg := f.config()
	cfg.DB = &openrails.DBConfig{URL: f.dsn(t)}
	application, err := app.BootstrapWithOptions(t.Context(), &cfg, &app.BootstrapOptions{PGXPool: f.pool, Migrations: older})
	require.NoError(t, err, "a build without %s boots", newest)
	require.NoError(t, application.Close(t.Context()))

	var kept int
	require.NoError(t, f.pool.QueryRow(t.Context(),
		`SELECT count(*) FROM public.migrations WHERE app = 'openrails' AND schema = $1 AND filename = $2`, f.schema, newest).Scan(&kept))
	require.Equal(t, 1, kept, "the newer migration stays applied")

	// And the newer build boots again on the same schema.
	client, err := openrails.New(t.Context(), f.config(), openrails.Deps{Postgres: f.pool})
	require.NoError(t, err)
	require.NoError(t, client.Close(t.Context()))
}
