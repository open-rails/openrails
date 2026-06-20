//go:build integration

package money_test

import (
	"context"
	"database/sql"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib" // database/sql driver for migratekit
	"github.com/open-rails/migratekit"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/modules/money"
	postgresmigrations "github.com/open-rails/openrails/migrations/postgres"
	"github.com/open-rails/openrails/pkg/identity"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

// startOwnerTenantPostgres boots a Postgres 18 container, creates the billing
// schema + pgcrypto, and applies the billing migratekit migrations. It returns a
// ready app DB handle. River migrations are intentionally skipped — the money
// tables do not depend on River.
func startOwnerTenantPostgres(t *testing.T) (*db.DB, string, context.Context) {
	t.Helper()
	ctx := context.Background()

	dsn := strings.TrimSpace(os.Getenv("OPENRAILS_TEST_DB_DSN"))
	if dsn == "" {
		dsn = strings.TrimSpace(os.Getenv("OPENRAILS_TEST_DB_URL"))
	}
	var err error
	if dsn == "" {
		var container *postgres.PostgresContainer
		container, err = postgres.Run(ctx,
			"postgres:18-alpine",
			postgres.WithDatabase("test_db"),
			postgres.WithUsername("test_user"),
			postgres.WithPassword("test_password"),
			dbtest.WithPostgresLimits(),
			testcontainers.WithWaitStrategy(
				wait.ForLog("database system is ready to accept connections").
					WithOccurrence(2).
					WithStartupTimeout(60*time.Second),
			),
		)
		require.NoError(t, err)
		t.Cleanup(func() { _ = container.Terminate(ctx) })

		dsn, err = container.ConnectionString(ctx, "sslmode=disable")
		require.NoError(t, err)
	}

	sqlDB, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = sqlDB.Close() })
	require.NoError(t, sqlDB.PingContext(ctx))

	// Bootstrap schema + extensions normally created by the deploy init SQL.
	_, err = sqlDB.ExecContext(ctx, `
		CREATE SCHEMA IF NOT EXISTS openrails;
		CREATE EXTENSION IF NOT EXISTS pgcrypto WITH SCHEMA public;
		CREATE EXTENSION IF NOT EXISTS btree_gist;
	`)
	require.NoError(t, err)

	migrations, err := migratekit.LoadFromFS(postgresmigrations.FS)
	require.NoError(t, err)
	m := migratekit.NewPostgres(sqlDB, "openrails")
	require.NoError(t, m.ApplyMigrations(ctx, migrations))

	dbi := dbtest.OpenAppDB(t, dsn)
	dbtest.EnsureTestMerchant(ctx, t, dbi.Pool())
	return dbi, dsn, dbtest.WithTestMerchant(ctx)
}

// money_balances schema constraint tests removed (#491): the cache table is
// gone — balance + held are derived from money_blocks + active holds/windows, so
// there is no money_balances row to enforce NOT NULL / uniqueness on.

// seedSpendable deposits `amount` in the default currency for a user via the
// service, creating a spendable lot.
func seedSpendable(t *testing.T, ctx context.Context, svc *money.MoneyService, userID string, amount int64) {
	t.Helper()
	src := uuid.New().String()
	_, err := svc.Deposit(ctx, money.DepositParams{
		Invoker:  userID,
		Currency: money.DefaultCurrency,
		Amount:   amount,
		Source:   "test_seed",
		SourceID: &src,
	})
	require.NoError(t, err)
}

func TestPostedSpend_ConservesTotal(t *testing.T) {
	dbi, _, ctx := startOwnerTenantPostgres(t)
	svc := money.NewMoneyService(dbi)

	userID := uuid.NewString()
	const initial = int64(1000)
	seedSpendable(t, ctx, svc, userID, initial)

	bal0, err := svc.GetBalance(ctx, userID, money.DefaultCurrency)
	require.NoError(t, err)
	require.Equal(t, initial, bal0.Balance)
	require.Equal(t, int64(0), bal0.HeldBalance)

	payer := identity.CustomerIDFromString(userID)
	err = svc.SpendCredits(ctx, money.SpendParams{
		Payer:    &payer,
		Invoker:  userID,
		Currency: money.DefaultCurrency,
		Amount:   150,
		Source:   "api",
		SourceID: "req-spend-1",
	})
	require.NoError(t, err)

	balFinal, err := svc.GetBalance(ctx, userID, money.DefaultCurrency)
	require.NoError(t, err)
	require.Equal(t, initial-150, balFinal.Balance, "only the posted spend leaves the balance")
	require.Equal(t, int64(0), balFinal.HeldBalance, "request holds are Redis state, not durable money held")
	require.Equal(t, initial-150, balFinal.Balance-balFinal.HeldBalance, "available == initial - captured")
}

func TestSpendIdempotency_RestoresSameBalanceOnReplay(t *testing.T) {
	dbi, _, ctx := startOwnerTenantPostgres(t)
	svc := money.NewMoneyService(dbi)

	userID := uuid.NewString()
	const initial = int64(500)
	seedSpendable(t, ctx, svc, userID, initial)

	payer := identity.CustomerIDFromString(userID)
	err := svc.SpendCredits(ctx, money.SpendParams{
		Payer:    &payer,
		Invoker:  userID,
		Currency: money.DefaultCurrency,
		Amount:   300,
		Source:   "api",
		SourceID: "req-replay-1",
	})
	require.NoError(t, err)
	err = svc.SpendCredits(ctx, money.SpendParams{
		Payer:    &payer,
		Invoker:  userID,
		Currency: money.DefaultCurrency,
		Amount:   300,
		Source:   "api",
		SourceID: "req-replay-1",
	})
	require.NoError(t, err)

	bal, err := svc.GetBalance(ctx, userID, money.DefaultCurrency)
	require.NoError(t, err)
	require.Equal(t, initial-300, bal.Balance, "replaying the same source/source_id must not spend twice")
	require.Equal(t, int64(0), bal.HeldBalance)
}
