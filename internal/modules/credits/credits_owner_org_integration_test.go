//go:build integration

package credits_test

import (
	"context"
	"database/sql"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib" // database/sql driver for migratekit
	"github.com/jonboulle/clockwork"
	"github.com/open-rails/migratekit"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/modules/credits"
	postgresmigrations "github.com/open-rails/openrails/migrations/postgres"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

// startOwnerTenantPostgres boots a Postgres 18 container, creates the billing
// schema + pgcrypto, and applies the billing migratekit migrations (which now
// include 040). It returns a ready app DB handle. River migrations are
// intentionally skipped — the credit tables do not depend on River.
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
		CREATE SCHEMA IF NOT EXISTS billing;
		CREATE EXTENSION IF NOT EXISTS pgcrypto WITH SCHEMA public;
		CREATE EXTENSION IF NOT EXISTS btree_gist;
	`)
	require.NoError(t, err)

	migrations, err := migratekit.LoadFromFS(postgresmigrations.FS)
	require.NoError(t, err)
	m := migratekit.NewPostgres(sqlDB, "openrails")
	require.NoError(t, m.ApplyMigrations(ctx, migrations))

	dbi := dbtest.OpenAppDB(t, dsn)
	return dbi, dsn, ctx
}

// TestSchema_EnforcesNotNullTenantAndPayer proves the HARDCUT schema (migrations
// 039 + 040, applied fresh / greenfield) enforces NOT NULL on tenant_id and
// tenant_subject_id for the credit tables: an insert that omits either is rejected by the
// database, so there is no path to a tenant-less or payer-less credit row.
func TestSchema_EnforcesNotNullTenantAndPayer(t *testing.T) {
	dbi, _, ctx := startOwnerTenantPostgres(t)
	pool := dbi.Pool()

	creditTypeID := uuid.New()
	_, err := gen.New(pool).CreateCreditType(ctx, gen.CreateCreditTypeParams{
		ID:            creditTypeID,
		Name:          "notnull_" + uuid.NewString(),
		DisplayName:   "NotNull",
		Unit:          "units",
		DecimalPlaces: 0,
		IsActive:      true,
		CreatedAt:     time.Now().UTC(),
	})
	require.NoError(t, err)

	payer := uuid.New()
	seedTenantSubject(t, ctx, pool, payer)

	// tenant_subject_id has no default: an insert that leaves it unset must be rejected.
	_, err = pool.Exec(ctx,
		`INSERT INTO openrails.credit_balances (id, credit_type_id, balance, held_balance)
		 VALUES ($1, $2, 0, 0)`,
		uuid.New(), creditTypeID)
	require.Error(t, err, "insert with NULL tenant_subject_id must violate NOT NULL")

	// tenant_id is NOT NULL but defaulted; an explicit NULL must still be rejected.
	_, err = pool.Exec(ctx,
		`INSERT INTO openrails.credit_balances (id, credit_type_id, tenant_subject_id, tenant_id, balance, held_balance)
		 VALUES ($1, $2, $3, NULL, 0, 0)`,
		uuid.New(), creditTypeID, payer)
	require.Error(t, err, "insert with explicit NULL tenant_id must violate NOT NULL")

	// A fully-specified payer+tenant row inserts cleanly. tenant_id must match the
	// tenant_subject's tenant (seeded by seedTenantSubject: 0…0001).
	_, err = pool.Exec(ctx,
		`INSERT INTO openrails.credit_balances (id, credit_type_id, tenant_subject_id, tenant_id, balance, held_balance)
		 VALUES ($1, $2, $3, $4, 0, 0)`,
		uuid.New(), creditTypeID, payer, uuid.MustParse("00000000-0000-0000-0000-000000000001"))
	require.NoError(t, err, "fully-specified payer+tenant insert must succeed")
}

// TestSchema_EnforcesOwnerTenantScopedBalanceUniqueness proves the HARDCUT
// payer+tenant-scoped uniqueness on credit_balances: two rows with the same
// (tenant, payer, credit_type) collide, while the SAME payer across DISTINCT
// tenants does NOT collide (tenant is part of the key).
func TestSchema_EnforcesOwnerTenantScopedBalanceUniqueness(t *testing.T) {
	dbi, _, ctx := startOwnerTenantPostgres(t)
	pool := dbi.Pool()

	creditTypeID := uuid.New()
	_, err := gen.New(pool).CreateCreditType(ctx, gen.CreateCreditTypeParams{
		ID:            creditTypeID,
		Name:          "uniq_" + uuid.NewString(),
		DisplayName:   "Uniq",
		Unit:          "units",
		DecimalPlaces: 0,
		IsActive:      true,
		CreatedAt:     time.Now().UTC(),
	})
	require.NoError(t, err)

	payer := uuid.New()
	seedTenantSubject(t, ctx, pool, payer)

	// First row in the tenant_subject's tenant (seeded by seedTenantSubject: 0…0001).
	_, err = pool.Exec(ctx,
		`INSERT INTO openrails.credit_balances (id, credit_type_id, tenant_subject_id, tenant_id, balance, held_balance)
		 VALUES ($1, $2, $3, $4, 0, 0)`,
		uuid.New(), creditTypeID, payer, uuid.MustParse("00000000-0000-0000-0000-000000000001"))
	require.NoError(t, err)

	// Duplicate (tenant, payer, credit_type) -> unique violation. Same tenant as above.
	_, err = pool.Exec(ctx,
		`INSERT INTO openrails.credit_balances (id, credit_type_id, tenant_subject_id, tenant_id, balance, held_balance)
		 VALUES ($1, $2, $3, $4, 0, 0)`,
		uuid.New(), creditTypeID, payer, uuid.MustParse("00000000-0000-0000-0000-000000000001"))
	require.Error(t, err, "duplicate (tenant, payer, credit_type) must violate uniqueness")

	// Same payer + credit_type in a DIFFERENT tenant -> allowed.
	otherTenant := uuid.New()
	_, err = pool.Exec(ctx,
		`INSERT INTO openrails.tenants (id, slug, name, status) VALUES ($1, $2, 'Other', 'active')`,
		otherTenant, "other_"+uuid.NewString())
	require.NoError(t, err)
	_, err = pool.Exec(ctx,
		`INSERT INTO openrails.credit_balances (id, credit_type_id, tenant_subject_id, tenant_id, balance, held_balance)
		 VALUES ($1, $2, $3, $4, 0, 0)`,
		uuid.New(), creditTypeID, payer, otherTenant)
	require.NoError(t, err, "same payer in a different tenant must NOT collide")
}

// seedSpendable deposits `amount` credits for a user via the service, creating a
// block and balance. Returns the credit type name.
func seedSpendable(t *testing.T, ctx context.Context, svc *credits.CreditsService, creditType string, userID string, amount int64) {
	t.Helper()
	src := uuid.New()
	_, err := svc.Deposit(ctx, credits.CreditDepositParams{
		Actor:      userID,
		CreditType: creditType,
		Amount:     amount,
		Source:     "test_seed",
		SourceID:   &src,
	})
	require.NoError(t, err)
}

// TestReserveCapturePartial_ConservesTotal proves Reserve(Hold) ->
// CaptureHold(partial) -> remainder released conserves the payer's total:
// available + held returns to (initial - captured), with the unused reservation
// fully released back to available.
func TestReserveCapturePartial_ConservesTotal(t *testing.T) {
	dbi, _, ctx := startOwnerTenantPostgres(t)
	svc := credits.NewCreditsService(dbi, clockwork.NewRealClock())

	ctName := "reserve_capture_" + uuid.NewString()
	creditTypeID := uuid.New()
	_, err := gen.New(dbi.Pool()).CreateCreditType(ctx, gen.CreateCreditTypeParams{
		ID: creditTypeID, Name: ctName, DisplayName: "RC", Unit: "u",
		DecimalPlaces: 0, IsActive: true, CreatedAt: time.Now().UTC(),
	})
	require.NoError(t, err)

	userID := uuid.NewString()
	const initial = int64(1000)
	seedSpendable(t, ctx, svc, ctName, userID, initial)

	bal0, err := svc.GetBalance(ctx, userID, ctName)
	require.NoError(t, err)
	require.Equal(t, initial, bal0.Balance)
	require.Equal(t, int64(0), bal0.HeldBalance)

	// Reserve 400.
	hold, err := svc.Hold(ctx, nil, userID, ctName, 400, "api", "req-rc-1", time.Now().Add(time.Hour).UTC())
	require.NoError(t, err)

	balHeld, err := svc.GetBalance(ctx, userID, ctName)
	require.NoError(t, err)
	require.Equal(t, initial, balHeld.Balance)
	require.Equal(t, int64(400), balHeld.HeldBalance)
	require.Equal(t, initial, balHeld.Balance-balHeld.HeldBalance+400) // available + held == initial

	// Capture only 150 of the 400; the remaining 250 reservation is released.
	_, err = svc.CaptureHold(ctx, hold.ID, 150)
	require.NoError(t, err)

	balFinal, err := svc.GetBalance(ctx, userID, ctName)
	require.NoError(t, err)
	// CONSERVATION: balance dropped by exactly the captured amount, hold released.
	require.Equal(t, initial-150, balFinal.Balance, "only the captured amount leaves the balance")
	require.Equal(t, int64(0), balFinal.HeldBalance, "unused reservation fully released")
	require.Equal(t, initial-150, balFinal.Balance-balFinal.HeldBalance, "available == initial - captured")
}

// TestReserveRelease_RestoresFullBalance proves Reserve(Hold) -> ReleaseHold
// restores the full available balance: no credits consumed.
func TestReserveRelease_RestoresFullBalance(t *testing.T) {
	dbi, _, ctx := startOwnerTenantPostgres(t)
	svc := credits.NewCreditsService(dbi, clockwork.NewRealClock())

	ctName := "reserve_release_" + uuid.NewString()
	creditTypeID := uuid.New()
	_, err := gen.New(dbi.Pool()).CreateCreditType(ctx, gen.CreateCreditTypeParams{
		ID: creditTypeID, Name: ctName, DisplayName: "RR", Unit: "u",
		DecimalPlaces: 0, IsActive: true, CreatedAt: time.Now().UTC(),
	})
	require.NoError(t, err)

	userID := uuid.NewString()
	const initial = int64(500)
	seedSpendable(t, ctx, svc, ctName, userID, initial)

	hold, err := svc.Hold(ctx, nil, userID, ctName, 300, "api", "req-rr-1", time.Now().Add(time.Hour).UTC())
	require.NoError(t, err)

	require.NoError(t, svc.ReleaseHold(ctx, hold.ID))

	bal, err := svc.GetBalance(ctx, userID, ctName)
	require.NoError(t, err)
	require.Equal(t, initial, bal.Balance, "release must not consume credits")
	require.Equal(t, int64(0), bal.HeldBalance, "release must clear the hold")
	require.Equal(t, initial, bal.Balance-bal.HeldBalance, "full available balance restored")
}
