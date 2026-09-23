//go:build integration

package db

import (
	"context"
	"database/sql"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib" // database/sql driver for the migration bootstrap
	"github.com/moby/moby/api/types/container"
	authkitembedded "github.com/open-rails/authkit/embedded"
	"github.com/open-rails/migratekit"
	"github.com/open-rails/openrails/config"
	postgresmigrations "github.com/open-rails/openrails/internal/migrate/postgres"
	"github.com/open-rails/openrails/pkg/merchant"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

// startRLSPostgres migrates as the owner, then provisions a test-host LOGIN
// with the library runtime privileges so merchant RLS actually enforces.
func startRLSPostgres(t *testing.T) (superDSN, appDSN string, ctx context.Context) {
	t.Helper()
	ctx = context.Background()

	container, err := postgres.Run(ctx,
		"postgres:18-alpine",
		postgres.WithDatabase("test_db"),
		postgres.WithUsername("test_user"),
		postgres.WithPassword("test_password"),
		testcontainers.WithHostConfigModifier(postgresTestLimits),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).WithStartupTimeout(60*time.Second),
		),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = container.Terminate(ctx) })

	superDSN, err = container.ConnectionString(ctx, "sslmode=disable")
	require.NoError(t, err)

	sqlDB, err := sql.Open("pgx", superDSN)
	require.NoError(t, err)
	t.Cleanup(func() { _ = sqlDB.Close() })
	require.NoError(t, sqlDB.PingContext(ctx))

	_, err = sqlDB.ExecContext(ctx, `
		CREATE SCHEMA IF NOT EXISTS billing;
		CREATE EXTENSION IF NOT EXISTS pgcrypto WITH SCHEMA public;
		CREATE EXTENSION IF NOT EXISTS btree_gist;
	`)
	require.NoError(t, err)

	profilesPool, err := pgxpool.New(ctx, superDSN)
	require.NoError(t, err)
	t.Cleanup(profilesPool.Close)
	require.NoError(t, authkitembedded.ApplyMigrations(ctx, profilesPool, "profiles"))
	migrations, err := migratekit.LoadFromFS(postgresmigrations.FS)
	require.NoError(t, err)
	for i := range migrations {
		migrations[i].Content, err = postgresmigrations.RewriteSchema(migrations[i].Content, config.DefaultSchema)
		require.NoError(t, err)
	}
	m := migratekit.NewPostgres(sqlDB, config.MigratekitApp).WithSchema(config.DefaultSchema)
	require.NoError(t, m.ApplyMigrations(ctx, migrations))

	// The test host creates its own regular login, then the library provisions access.
	_, err = sqlDB.ExecContext(ctx, `CREATE ROLE openrails_app LOGIN NOSUPERUSER NOBYPASSRLS PASSWORD 'app_pw'`)
	require.NoError(t, err)
	appDSN = "postgres://openrails_app:app_pw@" + dsnHostPart(t, superDSN)
	access, err := os.ReadFile("../migrate/runtime_access.sql")
	require.NoError(t, err)
	accessSQL, err := postgresmigrations.RewriteSchema(string(access), config.DefaultSchema)
	require.NoError(t, err)
	_, err = profilesPool.Exec(ctx, strings.ReplaceAll(accessSQL, `:"runtime_user"`, `"openrails_app"`))
	require.NoError(t, err)
	return superDSN, appDSN, ctx
}

// dsnHostPart extracts "host:port/db?params" from a postgres URL DSN.
func dsnHostPart(t *testing.T, dsn string) string {
	t.Helper()
	// dsn looks like postgres://user:pw@host:port/db?sslmode=disable
	at := -1
	for i := 0; i < len(dsn); i++ {
		if dsn[i] == '@' {
			at = i
			break
		}
	}
	require.GreaterOrEqual(t, at, 0, "DSN must contain @")
	return dsn[at+1:]
}

func seedTenantsAndEntitlements(t *testing.T, ctx context.Context, superDSN string, tA, tB merchant.ID) {
	t.Helper()
	pool, err := pgxpool.New(ctx, superDSN)
	require.NoError(t, err)
	defer pool.Close()

	for _, id := range []merchant.ID{tA, tB} {
		_, err = pool.Exec(ctx, `
			INSERT INTO billing.merchants (id, slug, status)
			VALUES ($1::uuid, $2, 'active') ON CONFLICT (id) DO NOTHING
		`, id.String(), "t-"+id.String()[:8])
		require.NoError(t, err)
		tenantSubjectID := uuid.New()
		_, err = pool.Exec(ctx, `
			INSERT INTO billing.customers (id, merchant_id)
			VALUES ($1::uuid, $2::uuid)
			ON CONFLICT (merchant_id, id) DO NOTHING
		`, tenantSubjectID.String(), id.String())
		require.NoError(t, err)
		// One entitlement row per merchant.
		_, err = pool.Exec(ctx, `
			INSERT INTO billing.entitlements
				(merchant_id, customer_id, entitlement, start_at, source_id, source_type)
			VALUES ($1::uuid, $2::uuid, 'premium', current_timestamp, gen_random_uuid(), 'admin')
		`, id.String(), tenantSubjectID.String())
		require.NoError(t, err)
	}
}

// The schema deliberately performs no hidden tenant filtering. Both connection
// kinds see all rows through unscoped raw SQL; tenant methods must predicate it.
func TestMerchantSchemaHasNoRoleDependentFiltering(t *testing.T) {
	ownerDSN, appDSN, ctx := startRLSPostgres(t)
	a, b := merchant.ID(uuid.New()), merchant.ID(uuid.New())
	seedTenantsAndEntitlements(t, ctx, ownerDSN, a, b)
	for name, dsn := range map[string]string{"owner": ownerDSN, "runtime": appDSN} {
		t.Run(name, func(t *testing.T) {
			pool, err := pgxpool.New(ctx, dsn)
			require.NoError(t, err)
			defer pool.Close()
			tx, err := pool.Begin(ctx)
			require.NoError(t, err)
			defer tx.Rollback(ctx)
			_, err = tx.Exec(ctx, "SELECT set_config('app.merchant_id',$1,true)", a.String())
			require.NoError(t, err)
			var total int
			require.NoError(t, tx.QueryRow(ctx, "SELECT count(*) FROM billing.entitlements").Scan(&total))
			require.Equal(t, 2, total)
			require.NoError(t, tx.QueryRow(ctx, "SELECT count(*) FROM billing.entitlements WHERE merchant_id=$1", a.UUID()).Scan(&total))
			require.Equal(t, 1, total)
		})
	}
}

// Session binding remains required by the explicitly scoped legacy SQL and
// restore guards; it no longer changes visibility of unrelated raw queries.
func TestMerchantTx_ScopesGUC(t *testing.T) {
	ownerDSN, appDSN, ctx := startRLSPostgres(t)
	a, b := merchant.ID(uuid.New()), merchant.ID(uuid.New())
	seedTenantsAndEntitlements(t, ctx, ownerDSN, a, b)
	d, err := NewDB(t.Context(), &config.DBConfig{URL: appDSN})
	require.NoError(t, err)
	defer d.Close()
	require.NoError(t, d.MerchantTx(merchant.WithID(ctx, a), func(ctx context.Context, tx pgx.Tx) error {
		var id string
		if err := tx.QueryRow(ctx, "SELECT current_setting('app.merchant_id')").Scan(&id); err != nil {
			return err
		}
		require.Equal(t, a.String(), id)
		var n int
		if err := tx.QueryRow(ctx, "SELECT count(*) FROM billing.entitlements WHERE merchant_id=nullif(current_setting('app.merchant_id',true),'')::uuid").Scan(&n); err != nil {
			return err
		}
		require.Equal(t, 1, n)
		return nil
	}))
}

func postgresTestLimits(hc *container.HostConfig) {
	hc.Resources.Memory = 2 << 30
	hc.Resources.NanoCPUs = 2_000_000_000
}
