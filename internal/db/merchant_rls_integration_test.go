//go:build integration

package db

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib" // database/sql driver for the migration bootstrap
	authpgmigrations "github.com/open-rails/authkit/migrations/postgres"
	"github.com/open-rails/migratekit"
	"github.com/open-rails/openrails/config"
	postgresmigrations "github.com/open-rails/openrails/migrations/postgres"
	"github.com/open-rails/openrails/pkg/merchant"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

// startRLSPostgres boots Postgres 18, applies the billing migrations (including
// 050 RLS + the openrails_app role), and gives the app role a password + LOGIN
// so the test can connect AS the unprivileged app role — the only role for which
// RLS actually enforces.
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
		CREATE SCHEMA IF NOT EXISTS openrails;
		CREATE EXTENSION IF NOT EXISTS pgcrypto WITH SCHEMA public;
		CREATE EXTENSION IF NOT EXISTS btree_gist;
	`)
	require.NoError(t, err)

	authMigrations, err := migratekit.LoadFromFS(authpgmigrations.FS)
	require.NoError(t, err)
	require.NoError(t, migratekit.NewPostgres(sqlDB, "authkit").ApplyMigrations(ctx, authMigrations))
	migrations, err := migratekit.LoadFromFS(postgresmigrations.FS)
	require.NoError(t, err)
	m := migratekit.NewPostgres(sqlDB, config.MigratekitApp)
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

func seedTenantsAndEntitlements(t *testing.T, ctx context.Context, superDSN string, tA, tB merchant.ID) {
	t.Helper()
	pool, err := pgxpool.New(ctx, superDSN)
	require.NoError(t, err)
	defer pool.Close()

	for _, id := range []merchant.ID{tA, tB} {
		_, err = pool.Exec(ctx, `
			INSERT INTO openrails.merchants (id, slug, status)
			VALUES ($1::uuid, $2, 'active') ON CONFLICT (id) DO NOTHING
		`, id.String(), "t-"+id.String()[:8])
		require.NoError(t, err)
		tenantSubjectID := uuid.New()
		_, err = pool.Exec(ctx, `
			INSERT INTO openrails.customers (id, merchant_id)
			VALUES ($1::uuid, $2::uuid)
			ON CONFLICT (id) DO NOTHING
		`, tenantSubjectID.String(), id.String())
		require.NoError(t, err)
		// One entitlement row per merchant.
		_, err = pool.Exec(ctx, `
			INSERT INTO openrails.entitlements
				(merchant_id, customer_id, entitlement, start_at, source_id, source_type)
			VALUES ($1::uuid, $2::uuid, 'premium', current_timestamp, gen_random_uuid(), 'admin')
		`, id.String(), tenantSubjectID.String())
		require.NoError(t, err)
	}
}

// TestRLS_AppRoleCannotSeeOtherTenantRows proves the migration-050 policies
// enforce when connecting AS the unprivileged app role: with app.merchant_id set
// to merchant A, a SELECT WITHOUT any merchant predicate returns ONLY merchant A's
// rows, and an attempt to read merchant B by explicit predicate returns nothing.
func TestRLS_AppRoleCannotSeeOtherTenantRows(t *testing.T) {
	superDSN, appDSN, ctx := startRLSPostgres(t)
	tA := merchant.ID(uuid.New())
	tB := merchant.ID(uuid.New())
	seedTenantsAndEntitlements(t, ctx, superDSN, tA, tB)

	appPool, err := pgxpool.New(ctx, appDSN)
	require.NoError(t, err)
	defer appPool.Close()

	// As the app role, in a tx scoped to merchant A: NO merchant predicate, yet only
	// merchant A's row is visible (RLS supplies the predicate).
	tx, err := appPool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(ctx) }()

	_, err = tx.Exec(ctx, `SELECT set_config('app.merchant_id', $1, true)`, tA.String())
	require.NoError(t, err)

	var total int
	require.NoError(t, tx.QueryRow(ctx, `SELECT count(*) FROM openrails.entitlements`).Scan(&total))
	require.Equal(t, 1, total, "RLS must restrict an un-predicated SELECT to the active merchant")

	// Even explicitly asking for merchant B returns nothing under merchant A's GUC.
	var leaked int
	require.NoError(t, tx.QueryRow(ctx,
		`SELECT count(*) FROM openrails.entitlements WHERE merchant_id = $1::uuid`, tB.String(),
	).Scan(&leaked))
	require.Equal(t, 0, leaked, "RLS must deny reading another merchant's rows even with an explicit predicate")
}

// TestRLS_GUCScopesCorrectly proves switching the GUC switches the visible
// merchant, and that an UNSET GUC sees nothing (fail-closed).
func TestRLS_GUCScopesCorrectly(t *testing.T) {
	superDSN, appDSN, ctx := startRLSPostgres(t)
	tA := merchant.ID(uuid.New())
	tB := merchant.ID(uuid.New())
	seedTenantsAndEntitlements(t, ctx, superDSN, tA, tB)

	appPool, err := pgxpool.New(ctx, appDSN)
	require.NoError(t, err)
	defer appPool.Close()

	count := func(setTenant string) int {
		tx, err := appPool.Begin(ctx)
		require.NoError(t, err)
		defer func() { _ = tx.Rollback(ctx) }()
		if setTenant != "" {
			_, err = tx.Exec(ctx, `SELECT set_config('app.merchant_id', $1, true)`, setTenant)
			require.NoError(t, err)
		}
		var n int
		require.NoError(t, tx.QueryRow(ctx, `SELECT count(*) FROM openrails.entitlements`).Scan(&n))
		return n
	}

	require.Equal(t, 1, count(tA.String()), "merchant A scope sees A's row")
	require.Equal(t, 1, count(tB.String()), "merchant B scope sees B's row")
	require.Equal(t, 0, count(""), "no GUC set => fail-closed, zero rows")
}

// TestMerchantTx_ScopesGUC proves the db.DB.MerchantTx helper sets the GUC from
// context so a merchant-owned query through it is RLS-scoped.
func TestMerchantTx_ScopesGUC(t *testing.T) {
	superDSN, appDSN, ctx := startRLSPostgres(t)
	tA := merchant.ID(uuid.New())
	tB := merchant.ID(uuid.New())
	seedTenantsAndEntitlements(t, ctx, superDSN, tA, tB)

	// Open over the APP role so RLS applies.
	d, err := NewDB(&config.DBConfig{URL: appDSN})
	require.NoError(t, err)
	defer d.Close()

	ctxA := merchant.WithID(ctx, tA)
	var n int
	err = d.MerchantTx(ctxA, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM openrails.entitlements`).Scan(&n)
	})
	require.NoError(t, err)
	require.Equal(t, 1, n, "MerchantTx must scope the query to the context merchant")
}

type merchantBoundarySeed struct {
	customerID       uuid.UUID
	subscriptionID   uuid.UUID
	nextSubscription uuid.UUID
}

func seedMerchantBoundaryRows(t *testing.T, ctx context.Context, superDSN string, merchantID merchant.ID) merchantBoundarySeed {
	t.Helper()
	pool, err := pgxpool.New(ctx, superDSN)
	require.NoError(t, err)
	defer pool.Close()

	customerID := uuid.New()
	productID := uuid.New()
	subscriptionID := uuid.New()
	nextSubscriptionID := uuid.New()
	suffix := strings.ReplaceAll(merchantID.String()[:13], "-", "")

	_, err = pool.Exec(ctx, `
		INSERT INTO openrails.merchants (id, slug, status)
		VALUES ($1::uuid, $2, 'active')
		ON CONFLICT (id) DO NOTHING
	`, merchantID.String(), "merchant-"+suffix)
	require.NoError(t, err)

	_, err = pool.Exec(ctx, `
		INSERT INTO openrails.customers (id, merchant_id)
		VALUES ($1::uuid, $2::uuid)
	`, customerID.String(), merchantID.String())
	require.NoError(t, err)

	_, err = pool.Exec(ctx, `
		INSERT INTO openrails.products (id, merchant_id, key, display_name)
		VALUES ($1::uuid, $2::uuid, $3, $3)
	`, productID.String(), merchantID.String(), "prod-"+suffix)
	require.NoError(t, err)

	for _, sub := range []struct {
		id     uuid.UUID
		status string
	}{
		{id: subscriptionID, status: "active"},
		{id: nextSubscriptionID, status: "expired"},
	} {
		_, err = pool.Exec(ctx, `
			INSERT INTO openrails.subscriptions (id, merchant_id, customer_id, product_id, status)
			VALUES ($1::uuid, $2::uuid, $3::uuid, $4::uuid, $5)
		`, sub.id.String(), merchantID.String(), customerID.String(), productID.String(), sub.status)
		require.NoError(t, err)
	}

	_, err = pool.Exec(ctx, `
		INSERT INTO openrails.solana_subscriptions
			(id, merchant_id, subscription_id, subscriber_wallet, authority_pda, subscription_pda,
			 plan_pda, merchant_address, mint, plan_created_at_fingerprint, next_pull_at)
		VALUES
			(gen_random_uuid(), $1::uuid, $2::uuid, $3, $4, $5, $6, $7, $8, 1, current_timestamp)
	`, merchantID.String(), subscriptionID.String(), "subscriber-"+suffix, "authority-"+suffix,
		"subscription-"+suffix, "plan-"+suffix, "merchant-wallet-"+suffix, "mint-"+suffix)
	require.NoError(t, err)

	_, err = pool.Exec(ctx, `
		INSERT INTO openrails.merchant_configurations (merchant_id, config)
		VALUES ($1::uuid, '{"source":"seed"}'::jsonb)
	`, merchantID.String())
	require.NoError(t, err)

	return merchantBoundarySeed{customerID: customerID, subscriptionID: subscriptionID, nextSubscription: nextSubscriptionID}
}

func TestRLS_AppRoleCannotCrossMerchantBoundariesForIssue504Tables(t *testing.T) {
	superDSN, appDSN, ctx := startRLSPostgres(t)
	tA := merchant.ID(uuid.New())
	tB := merchant.ID(uuid.New())
	seedMerchantBoundaryRows(t, ctx, superDSN, tA)
	seedB := seedMerchantBoundaryRows(t, ctx, superDSN, tB)

	appPool, err := pgxpool.New(ctx, appDSN)
	require.NoError(t, err)
	defer appPool.Close()

	tx, err := appPool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(ctx) }()
	_, err = tx.Exec(ctx, `SELECT set_config('app.merchant_id', $1, true)`, tA.String())
	require.NoError(t, err)

	assertOneOwnZeroOther := func(table string) {
		t.Helper()
		var own int
		require.NoError(t, tx.QueryRow(ctx, "SELECT count(*) FROM openrails."+table).Scan(&own))
		require.Equal(t, 1, own, "%s should expose only merchant A's row under merchant A GUC", table)
		var other int
		require.NoError(t, tx.QueryRow(ctx, "SELECT count(*) FROM openrails."+table+" WHERE merchant_id = $1::uuid", tB.String()).Scan(&other))
		require.Equal(t, 0, other, "%s should hide merchant B's row under merchant A GUC", table)
	}

	for _, table := range []string{
		"customers",
		"solana_subscriptions",
		"merchant_configurations",
	} {
		assertOneOwnZeroOther(table)
	}

	_, err = tx.Exec(ctx, `
		INSERT INTO openrails.customers (id, merchant_id)
		VALUES (gen_random_uuid(), $1::uuid)
	`, tB.String())
	require.Error(t, err, "customers must reject writes for another merchant")
	require.NoError(t, tx.Rollback(ctx))

	tx, err = appPool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(ctx) }()
	_, err = tx.Exec(ctx, `SELECT set_config('app.merchant_id', $1, true)`, tA.String())
	require.NoError(t, err)

	_, err = tx.Exec(ctx, `
		INSERT INTO openrails.solana_subscriptions
			(id, merchant_id, subscription_id, subscriber_wallet, authority_pda, subscription_pda,
			 plan_pda, merchant_address, mint, plan_created_at_fingerprint, next_pull_at)
		VALUES
			(gen_random_uuid(), $1::uuid, $2::uuid, 'subscriber-cross', 'authority-cross',
			 'subscription-cross', 'plan-cross', 'merchant-wallet-cross', 'mint-cross', 1, current_timestamp)
	`, tB.String(), seedB.nextSubscription.String())
	require.Error(t, err, "solana_subscriptions must reject writes for another merchant")
	require.NoError(t, tx.Rollback(ctx))

	tx, err = appPool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(ctx) }()
	_, err = tx.Exec(ctx, `SELECT set_config('app.merchant_id', $1, true)`, tA.String())
	require.NoError(t, err)

	tag, err := tx.Exec(ctx, `
		UPDATE openrails.merchant_configurations
		   SET config = '{"source":"cross"}'::jsonb
		 WHERE merchant_id = $1::uuid
	`, tB.String())
	require.NoError(t, err)
	require.Zero(t, tag.RowsAffected(), "merchant_configurations must not update another merchant's row")
}
