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
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/migratekit"
	postgresmigrations "github.com/open-rails/openrails/migrations/postgres"
	"github.com/open-rails/openrails/pkg/tenant"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"
)

// startRLSPostgres boots Postgres 18, applies the billing migrations (including
// 050 RLS + the openrails_app role), and gives the app role a password + LOGIN
// so the test can connect AS the unprivileged app role — the only role for which
// RLS actually enforces.
func startRLSPostgres(t *testing.T) (superDSN, appDSN string, ctx context.Context) {
	t.Helper()
	ctx = context.Background()
	dsn := strings.TrimSpace(os.Getenv("OPENRAILS_TEST_DB_DSN"))
	if dsn == "" {
		dsn = strings.TrimSpace(os.Getenv("OPENRAILS_TEST_DB_URL"))
	}
	if dsn != "" {
		superDSN = dsn
		sqlDB := sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(superDSN)))
		t.Cleanup(func() { _ = sqlDB.Close() })
		require.NoError(t, sqlDB.PingContext(ctx))

		_, err := sqlDB.ExecContext(ctx, `
			CREATE SCHEMA IF NOT EXISTS billing;
			CREATE EXTENSION IF NOT EXISTS pgcrypto WITH SCHEMA public;
			CREATE EXTENSION IF NOT EXISTS btree_gist;
		`)
		require.NoError(t, err)

		migrations, err := migratekit.LoadFromFS(postgresmigrations.FS)
		require.NoError(t, err)
		m := migratekit.NewPostgres(sqlDB, "billing")
		require.NoError(t, m.ApplyMigrations(ctx, migrations))

		_, err = sqlDB.ExecContext(ctx, `ALTER ROLE openrails_app WITH LOGIN PASSWORD 'app_pw'`)
		require.NoError(t, err)

		appDSN = "postgres://openrails_app:app_pw@" + dsnHostPart(t, superDSN)
		return superDSN, appDSN, ctx
	}

	container, err := postgres.Run(ctx,
		"postgres:18-alpine",
		postgres.WithDatabase("test_db"),
		postgres.WithUsername("test_user"),
		postgres.WithPassword("test_password"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).WithStartupTimeout(60*time.Second),
		),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = container.Terminate(ctx) })

	superDSN, err = container.ConnectionString(ctx, "sslmode=disable")
	require.NoError(t, err)

	sqlDB := sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(superDSN)))
	t.Cleanup(func() { _ = sqlDB.Close() })
	require.NoError(t, sqlDB.PingContext(ctx))

	_, err = sqlDB.ExecContext(ctx, `
		CREATE SCHEMA IF NOT EXISTS billing;
		CREATE EXTENSION IF NOT EXISTS pgcrypto WITH SCHEMA public;
		CREATE EXTENSION IF NOT EXISTS btree_gist;
	`)
	require.NoError(t, err)

	migrations, err := migratekit.LoadFromFS(postgresmigrations.FS)
	require.NoError(t, err)
	m := migratekit.NewPostgres(sqlDB, "billing")
	require.NoError(t, m.ApplyMigrations(ctx, migrations))

	// Production attaches LOGIN + a password to the app role out of band; do that
	// here so we can connect as it. The role keeps NOBYPASSRLS from 001_schema.up.sql.
	_, err = sqlDB.ExecContext(ctx, `ALTER ROLE openrails_app WITH LOGIN PASSWORD 'app_pw'`)
	require.NoError(t, err)

	// Build the app-role DSN from the superuser one by swapping credentials.
	appDSN = "postgres://openrails_app:app_pw@" + dsnHostPart(t, superDSN)
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

func seedTenantsAndEntitlements(t *testing.T, ctx context.Context, superDSN string, tA, tB tenant.ID) {
	t.Helper()
	pool, err := pgxpool.New(ctx, superDSN)
	require.NoError(t, err)
	defer pool.Close()

	for _, id := range []tenant.ID{tA, tB} {
		_, err = pool.Exec(ctx, `
			INSERT INTO billing.tenants (id, slug, name, status)
			VALUES ($1::uuid, $2, $2, 'active') ON CONFLICT (id) DO NOTHING
		`, id.String(), "t-"+id.String()[:8])
		require.NoError(t, err)
		// One entitlement row per tenant.
		_, err = pool.Exec(ctx, `
			INSERT INTO billing.entitlements
				(tenant_id, user_id, entitlement, start_at, source_id, source_type)
			VALUES ($1::uuid, $2, 'premium', current_timestamp, gen_random_uuid(), 'admin')
		`, id.String(), "user-"+id.String()[:8])
		require.NoError(t, err)
	}
}

// TestRLS_AppRoleCannotSeeOtherTenantRows proves the migration-050 policies
// enforce when connecting AS the unprivileged app role: with app.tenant_id set
// to tenant A, a SELECT WITHOUT any tenant predicate returns ONLY tenant A's
// rows, and an attempt to read tenant B by explicit predicate returns nothing.
func TestRLS_AppRoleCannotSeeOtherTenantRows(t *testing.T) {
	superDSN, appDSN, ctx := startRLSPostgres(t)
	tA := tenant.ID(uuid.New())
	tB := tenant.ID(uuid.New())
	seedTenantsAndEntitlements(t, ctx, superDSN, tA, tB)

	appPool, err := pgxpool.New(ctx, appDSN)
	require.NoError(t, err)
	defer appPool.Close()

	// As the app role, in a tx scoped to tenant A: NO tenant predicate, yet only
	// tenant A's row is visible (RLS supplies the predicate).
	tx, err := appPool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(ctx) }()

	_, err = tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, tA.String())
	require.NoError(t, err)

	var total int
	require.NoError(t, tx.QueryRow(ctx, `SELECT count(*) FROM billing.entitlements`).Scan(&total))
	require.Equal(t, 1, total, "RLS must restrict an un-predicated SELECT to the active tenant")

	// Even explicitly asking for tenant B returns nothing under tenant A's GUC.
	var leaked int
	require.NoError(t, tx.QueryRow(ctx,
		`SELECT count(*) FROM billing.entitlements WHERE tenant_id = $1::uuid`, tB.String(),
	).Scan(&leaked))
	require.Equal(t, 0, leaked, "RLS must deny reading another tenant's rows even with an explicit predicate")
}

// TestRLS_GUCScopesCorrectly proves switching the GUC switches the visible
// tenant, and that an UNSET GUC sees nothing (fail-closed).
func TestRLS_GUCScopesCorrectly(t *testing.T) {
	superDSN, appDSN, ctx := startRLSPostgres(t)
	tA := tenant.ID(uuid.New())
	tB := tenant.ID(uuid.New())
	seedTenantsAndEntitlements(t, ctx, superDSN, tA, tB)

	appPool, err := pgxpool.New(ctx, appDSN)
	require.NoError(t, err)
	defer appPool.Close()

	count := func(setTenant string) int {
		tx, err := appPool.Begin(ctx)
		require.NoError(t, err)
		defer func() { _ = tx.Rollback(ctx) }()
		if setTenant != "" {
			_, err = tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, setTenant)
			require.NoError(t, err)
		}
		var n int
		require.NoError(t, tx.QueryRow(ctx, `SELECT count(*) FROM billing.entitlements`).Scan(&n))
		return n
	}

	require.Equal(t, 1, count(tA.String()), "tenant A scope sees A's row")
	require.Equal(t, 1, count(tB.String()), "tenant B scope sees B's row")
	require.Equal(t, 0, count(""), "no GUC set => fail-closed, zero rows")
}

// TestRunInTenantTx_ScopesGUC proves the db.DB.RunInTenantTx helper sets the GUC
// from context so a tenant-owned query through it is RLS-scoped.
func TestRunInTenantTx_ScopesGUC(t *testing.T) {
	superDSN, appDSN, ctx := startRLSPostgres(t)
	tA := tenant.ID(uuid.New())
	tB := tenant.ID(uuid.New())
	seedTenantsAndEntitlements(t, ctx, superDSN, tA, tB)

	// Open a bun DB over the APP role so RLS applies.
	sqlDB := sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(appDSN)))
	defer func() { _ = sqlDB.Close() }()
	bunDB := bun.NewDB(sqlDB, pgdialect.New())
	d, err := NewWithBun(bunDB)
	require.NoError(t, err)

	ctxA := tenant.WithID(ctx, tA)
	var n int
	err = d.RunInTenantTx(ctxA, func(ctx context.Context, tx bun.Tx) error {
		return tx.NewRaw(`SELECT count(*) FROM billing.entitlements`).Scan(ctx, &n)
	})
	require.NoError(t, err)
	require.Equal(t, 1, n, "RunInTenantTx must scope the query to the context tenant")
}
