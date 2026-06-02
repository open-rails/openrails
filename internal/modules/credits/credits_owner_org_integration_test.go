//go:build integration

package credits_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"
	"github.com/open-rails/migratekit"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/credits"
	postgresmigrations "github.com/open-rails/openrails/migrations/postgres"
	"github.com/open-rails/openrails/pkg/identity"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"
)

// startOwnerOrgPostgres boots a Postgres 18 container, creates the billing
// schema + pgcrypto, and applies the billing migratekit migrations (which now
// include 040). It returns a ready bun.DB. River migrations are intentionally
// skipped — the credit tables do not depend on River.
func startOwnerOrgPostgres(t *testing.T) (*bun.DB, context.Context) {
	t.Helper()
	ctx := context.Background()

	container, err := postgres.Run(ctx,
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

	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	require.NoError(t, err)

	sqlDB := sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dsn)))
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
	m := migratekit.NewPostgres(sqlDB, "billing")
	require.NoError(t, m.ApplyMigrations(ctx, migrations))

	bunDB := bun.NewDB(sqlDB, pgdialect.New())
	models.RegisterModels(bunDB)
	return bunDB, ctx
}

// TestMigration040_BackfillPreservesBalancesExactly is the MANDATORY balance-
// preservation test for issue #221: it seeds legacy user-owned balances WITHOUT
// owner_id, runs the deterministic backfill exactly as migration 040 does, and
// proves SUM(balance) per (user -> owner) is identical before and after — no
// credits created or destroyed, and every user maps to exactly one owner.
func TestMigration040_BackfillPreservesBalancesExactly(t *testing.T) {
	bunDB, ctx := startOwnerOrgPostgres(t)

	creditTypeID := uuid.New()
	_, err := bunDB.NewInsert().Model(&models.CreditType{
		ID:            creditTypeID,
		Name:          "owner_backfill_" + uuid.NewString(),
		DisplayName:   "Owner Backfill",
		Unit:          "units",
		DecimalPlaces: 0,
		IsActive:      true,
		CreatedAt:     time.Now().UTC(),
	}).Exec(ctx)
	require.NoError(t, err)

	// Seed legacy balances with owner_id NULL (as if written before 040), by
	// nulling owner_id after insert. Distinct users + amounts.
	users := map[string]int64{
		uuid.NewString(): 1000,
		uuid.NewString(): 2500,
		uuid.NewString(): 0,
		uuid.NewString(): 777,
	}
	var wantTotal int64
	for userID, bal := range users {
		wantTotal += bal
		_, err := bunDB.NewInsert().Model(&models.UserCreditBalance{
			ID:           uuid.New(),
			UserID:       userID,
			CreditTypeID: creditTypeID,
			Balance:      bal,
			HeldBalance:  0,
			CreatedAt:    time.Now().UTC(),
			UpdatedAt:    time.Now().UTC(),
		}).Exec(ctx)
		require.NoError(t, err)
	}
	// Force owner_id NULL to simulate pre-040 rows.
	_, err = bunDB.NewUpdate().Model((*models.UserCreditBalance)(nil)).
		Set("owner_id = NULL").
		Where("credit_type_id = ?", creditTypeID).
		Exec(ctx)
	require.NoError(t, err)

	// BEFORE: sum of all balances for this credit type.
	var beforeTotal int64
	require.NoError(t, bunDB.NewSelect().Model((*models.UserCreditBalance)(nil)).
		ColumnExpr("COALESCE(SUM(balance),0)").
		Where("credit_type_id = ?", creditTypeID).
		Scan(ctx, &beforeTotal))
	require.Equal(t, wantTotal, beforeTotal)

	// Re-run the EXACT backfill expression migration 040 uses. (Idempotent:
	// only touches owner_id IS NULL rows.)
	backfill := `
		UPDATE billing.user_credit_balances AS tbl SET owner_id = (
			WITH h AS (
				SELECT substring(
					public.digest(decode('6f1c9b3e2a445d7c8e109a2b3c4d5e6f','hex')::bytea
						|| convert_to('openrails:personal-org:' || tbl.user_id, 'UTF8'), 'sha1')
					FROM 1 FOR 16
				) AS b
			)
			SELECT encode(
				set_byte(set_byte(h.b, 6, (get_byte(h.b,6) & 15) | 80), 8, (get_byte(h.b,8) & 63) | 128),
				'hex'
			)::uuid FROM h
		)
		WHERE tbl.owner_id IS NULL`
	_, err = bunDB.ExecContext(ctx, backfill)
	require.NoError(t, err)

	// AFTER: every row now has owner_id; total unchanged.
	var afterTotal int64
	require.NoError(t, bunDB.NewSelect().Model((*models.UserCreditBalance)(nil)).
		ColumnExpr("COALESCE(SUM(balance),0)").
		Where("credit_type_id = ?", creditTypeID).
		Scan(ctx, &afterTotal))
	require.Equal(t, beforeTotal, afterTotal, "backfill must not create or destroy credits")

	var nullOwners int
	require.NoError(t, bunDB.NewSelect().Model((*models.UserCreditBalance)(nil)).
		ColumnExpr("COUNT(*)").
		Where("credit_type_id = ? AND owner_id IS NULL", creditTypeID).
		Scan(ctx, &nullOwners))
	require.Zero(t, nullOwners, "every legacy balance must be assigned an owner")

	// Per-user invariant: SQL-derived owner_id must equal the Go derivation, and
	// each user's balance is preserved 1:1 under its owner.
	for userID, bal := range users {
		var gotOwner string
		var gotBalI int64
		require.NoError(t, bunDB.NewSelect().
			Model((*models.UserCreditBalance)(nil)).
			ColumnExpr("owner_id::text AS owner, balance").
			Where("credit_type_id = ? AND user_id = ?", creditTypeID, userID).
			Scan(ctx, &gotOwner, &gotBalI))
		require.Equal(t, identity.PersonalOrgID(userID).String(), gotOwner,
			"SQL backfill owner_id must match Go identity.PersonalOrgID for %s", userID)
		require.Equal(t, bal, gotBalI, "per-user balance must be preserved exactly")
	}
}

// seedSpendable deposits `amount` credits for a user via the service, creating a
// block and balance. Returns the credit type name.
func seedSpendable(t *testing.T, ctx context.Context, svc *credits.CreditsService, creditType string, userID string, amount int64) {
	t.Helper()
	src := uuid.New()
	_, err := svc.Deposit(ctx, credits.CreditDepositParams{
		UserID:     userID,
		CreditType: creditType,
		Amount:     amount,
		Source:     "test_seed",
		SourceID:   &src,
	})
	require.NoError(t, err)
}

// TestReserveCapturePartial_ConservesTotal proves Reserve(Hold) ->
// CaptureHold(partial) -> remainder released conserves the owner's total:
// available + held returns to (initial - captured), with the unused reservation
// fully released back to available.
func TestReserveCapturePartial_ConservesTotal(t *testing.T) {
	bunDB, ctx := startOwnerOrgPostgres(t)
	dbi, err := db.NewWithBun(bunDB)
	require.NoError(t, err)
	svc := credits.NewCreditsService(dbi, clockwork.NewRealClock())

	ctName := "reserve_capture_" + uuid.NewString()
	creditTypeID := uuid.New()
	_, err = bunDB.NewInsert().Model(&models.CreditType{
		ID: creditTypeID, Name: ctName, DisplayName: "RC", Unit: "u",
		DecimalPlaces: 0, IsActive: true, CreatedAt: time.Now().UTC(),
	}).Exec(ctx)
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
	bunDB, ctx := startOwnerOrgPostgres(t)
	dbi, err := db.NewWithBun(bunDB)
	require.NoError(t, err)
	svc := credits.NewCreditsService(dbi, clockwork.NewRealClock())

	ctName := "reserve_release_" + uuid.NewString()
	creditTypeID := uuid.New()
	_, err = bunDB.NewInsert().Model(&models.CreditType{
		ID: creditTypeID, Name: ctName, DisplayName: "RR", Unit: "u",
		DecimalPlaces: 0, IsActive: true, CreatedAt: time.Now().UTC(),
	}).Exec(ctx)
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
