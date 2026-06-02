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
	riverjobs "github.com/open-rails/openrails/internal/river"
	postgresmigrations "github.com/open-rails/openrails/migrations/postgres"
	"github.com/riverqueue/river"
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

// TestSchema_EnforcesNotNullTenantAndOwner proves the HARDCUT schema (migrations
// 039 + 040, applied fresh / greenfield) enforces NOT NULL on tenant_id and
// owner_id for the credit tables: an insert that omits either is rejected by the
// database, so there is no path to a tenant-less or owner-less credit row.
func TestSchema_EnforcesNotNullTenantAndOwner(t *testing.T) {
	bunDB, ctx := startOwnerOrgPostgres(t)

	creditTypeID := uuid.New()
	_, err := bunDB.NewInsert().Model(&models.CreditType{
		ID:            creditTypeID,
		Name:          "notnull_" + uuid.NewString(),
		DisplayName:   "NotNull",
		Unit:          "units",
		DecimalPlaces: 0,
		IsActive:      true,
		CreatedAt:     time.Now().UTC(),
	}).Exec(ctx)
	require.NoError(t, err)

	owner := uuid.New()

	// owner_id has no default: an insert that leaves it unset must be rejected.
	_, err = bunDB.ExecContext(ctx,
		`INSERT INTO billing.user_credit_balances (id, user_id, credit_type_id, balance, held_balance)
		 VALUES (?, ?, ?, 0, 0)`,
		uuid.New(), uuid.NewString(), creditTypeID)
	require.Error(t, err, "insert with NULL owner_id must violate NOT NULL")

	// tenant_id is NOT NULL but defaulted; an explicit NULL must still be rejected.
	_, err = bunDB.ExecContext(ctx,
		`INSERT INTO billing.user_credit_balances (id, user_id, credit_type_id, owner_id, tenant_id, balance, held_balance)
		 VALUES (?, ?, ?, ?, NULL, 0, 0)`,
		uuid.New(), uuid.NewString(), creditTypeID, owner)
	require.Error(t, err, "insert with explicit NULL tenant_id must violate NOT NULL")

	// A fully-specified owner+tenant row inserts cleanly.
	_, err = bunDB.ExecContext(ctx,
		`INSERT INTO billing.user_credit_balances (id, user_id, credit_type_id, owner_id, balance, held_balance)
		 VALUES (?, ?, ?, ?, 0, 0)`,
		uuid.New(), uuid.NewString(), creditTypeID, owner)
	require.NoError(t, err, "owner+tenant-defaulted insert must succeed")
}

// TestSchema_EnforcesOwnerTenantScopedBalanceUniqueness proves the HARDCUT
// owner+tenant-scoped uniqueness on user_credit_balances: two rows with the same
// (tenant, owner, credit_type) collide, while the SAME owner across DISTINCT
// tenants does NOT collide (tenant is part of the key).
func TestSchema_EnforcesOwnerTenantScopedBalanceUniqueness(t *testing.T) {
	bunDB, ctx := startOwnerOrgPostgres(t)

	creditTypeID := uuid.New()
	_, err := bunDB.NewInsert().Model(&models.CreditType{
		ID:            creditTypeID,
		Name:          "uniq_" + uuid.NewString(),
		DisplayName:   "Uniq",
		Unit:          "units",
		DecimalPlaces: 0,
		IsActive:      true,
		CreatedAt:     time.Now().UTC(),
	}).Exec(ctx)
	require.NoError(t, err)

	owner := uuid.New()

	// First row in the default tenant.
	_, err = bunDB.ExecContext(ctx,
		`INSERT INTO billing.user_credit_balances (id, user_id, credit_type_id, owner_id, balance, held_balance)
		 VALUES (?, ?, ?, ?, 0, 0)`,
		uuid.New(), uuid.NewString(), creditTypeID, owner)
	require.NoError(t, err)

	// Duplicate (tenant, owner, credit_type) -> unique violation.
	_, err = bunDB.ExecContext(ctx,
		`INSERT INTO billing.user_credit_balances (id, user_id, credit_type_id, owner_id, balance, held_balance)
		 VALUES (?, ?, ?, ?, 0, 0)`,
		uuid.New(), uuid.NewString(), creditTypeID, owner)
	require.Error(t, err, "duplicate (tenant, owner, credit_type) must violate uniqueness")

	// Same owner + credit_type in a DIFFERENT tenant -> allowed.
	otherTenant := uuid.New()
	_, err = bunDB.ExecContext(ctx,
		`INSERT INTO billing.tenants (id, slug, name, status) VALUES (?, ?, 'Other', 'active')`,
		otherTenant, "other_"+uuid.NewString())
	require.NoError(t, err)
	_, err = bunDB.ExecContext(ctx,
		`INSERT INTO billing.user_credit_balances (id, user_id, credit_type_id, owner_id, tenant_id, balance, held_balance)
		 VALUES (?, ?, ?, ?, ?, 0, 0)`,
		uuid.New(), uuid.NewString(), creditTypeID, owner, otherTenant)
	require.NoError(t, err, "same owner in a different tenant must NOT collide")
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

func newCreditType(t *testing.T, ctx context.Context, bunDB *bun.DB, prefix string) string {
	t.Helper()
	name := prefix + "_" + uuid.NewString()
	_, err := bunDB.NewInsert().Model(&models.CreditType{
		ID: uuid.New(), Name: name, DisplayName: prefix, Unit: "u",
		DecimalPlaces: 0, IsActive: true, CreatedAt: time.Now().UTC(),
	}).Exec(ctx)
	require.NoError(t, err)
	return name
}

// TestHoldExpireWorker_ReleasesOwnerHeldBalance proves the unified-record expiry
// path: an 'active' hold past its expires_at is transitioned to 'expired' by the
// worker and its reservation is released back to the OWNER's balance row. This is
// the regression guard for the worker keying held_balance by (tenant, owner,
// credit_type) — not by actor user_id, which is not part of the balance identity.
func TestHoldExpireWorker_ReleasesOwnerHeldBalance(t *testing.T) {
	bunDB, ctx := startOwnerOrgPostgres(t)
	dbi, err := db.NewWithBun(bunDB)
	require.NoError(t, err)
	svc := credits.NewCreditsService(dbi, clockwork.NewRealClock())

	ctName := newCreditType(t, ctx, bunDB, "hold_expire")
	userID := uuid.NewString()
	const initial = int64(1000)
	seedSpendable(t, ctx, svc, ctName, userID, initial)

	// Hold that is already expired (deadline in the past).
	hold, err := svc.Hold(ctx, nil, userID, ctName, 250, "api", "req-exp-1", time.Now().Add(-time.Minute).UTC())
	require.NoError(t, err)

	held, err := svc.GetBalance(ctx, userID, ctName)
	require.NoError(t, err)
	require.Equal(t, int64(250), held.HeldBalance, "hold reserves held_balance")

	w := &riverjobs.HoldExpiryWorker{DB: dbi, Clock: clockwork.NewRealClock()}
	require.NoError(t, w.Work(ctx, &river.Job[riverjobs.HoldExpiryArgs]{Args: riverjobs.HoldExpiryArgs{}}))

	row := new(models.CreditTransaction)
	require.NoError(t, bunDB.NewSelect().Model(row).Where("id = ?", hold.ID).Scan(ctx))
	require.Equal(t, "expired", row.Status, "worker transitions the lifecycle record to expired")

	bal, err := svc.GetBalance(ctx, userID, ctName)
	require.NoError(t, err)
	require.Equal(t, initial, bal.Balance, "expiry consumes no credits")
	require.Equal(t, int64(0), bal.HeldBalance, "expiry releases the reservation to the owner balance")
}

// TestDepositWithdraw_TerminalTransactionsAndFIFO proves deposits/withdrawals are
// terminal lifecycle rows (status='posted') and that withdrawals consume credit
// blocks oldest-expiry-first (FIFO).
func TestDepositWithdraw_TerminalTransactionsAndFIFO(t *testing.T) {
	bunDB, ctx := startOwnerOrgPostgres(t)
	dbi, err := db.NewWithBun(bunDB)
	require.NoError(t, err)
	svc := credits.NewCreditsService(dbi, clockwork.NewRealClock())

	ctName := newCreditType(t, ctx, bunDB, "fifo")
	userID := uuid.NewString()

	earlyExpiry := time.Now().Add(time.Hour).UTC()
	lateExpiry := time.Now().Add(48 * time.Hour).UTC()

	// Block A: 100 credits expiring soon. Block B: 100 expiring later.
	srcA, srcB := uuid.New(), uuid.New()
	depA, err := svc.Deposit(ctx, credits.CreditDepositParams{
		UserID: userID, CreditType: ctName, Amount: 100, Source: "purchase",
		SourceID: &srcA, ExpiresAt: &earlyExpiry,
	})
	require.NoError(t, err)
	require.Equal(t, "deposit", depA.TransactionType)
	require.Equal(t, "posted", depA.Status)
	require.Equal(t, int64(100), depA.Amount)
	require.NotNil(t, depA.BalanceAfter)
	require.Equal(t, int64(100), *depA.BalanceAfter)

	_, err = svc.Deposit(ctx, credits.CreditDepositParams{
		UserID: userID, CreditType: ctName, Amount: 100, Source: "purchase",
		SourceID: &srcB, ExpiresAt: &lateExpiry,
	})
	require.NoError(t, err)

	// Withdraw 150: must drain block A (100, earliest expiry) then 50 from block B.
	srcW := uuid.New()
	wTrx, err := svc.Withdraw(ctx, credits.CreditWithdrawParams{
		UserID: userID, CreditType: ctName, Amount: 150, Source: "usage", SourceID: &srcW,
	})
	require.NoError(t, err)
	require.Equal(t, "withdrawal", wTrx.TransactionType)
	require.Equal(t, "posted", wTrx.Status)
	require.Equal(t, int64(-150), wTrx.Amount)

	bal, err := svc.GetBalance(ctx, userID, ctName)
	require.NoError(t, err)
	require.Equal(t, int64(50), bal.Balance, "200 deposited - 150 withdrawn")

	var blocks []models.CreditBlock
	require.NoError(t, bunDB.NewSelect().Model(&blocks).
		Where("user_id = ?", userID).
		OrderExpr("expires_at ASC").
		Scan(ctx))
	require.Len(t, blocks, 2)
	require.Equal(t, int64(0), blocks[0].RemainingAmount, "earliest-expiry block drained first (FIFO)")
	require.Equal(t, int64(50), blocks[1].RemainingAmount, "later block holds the remainder")

	// Idempotent withdrawal: same SourceID returns the same row, no double-spend.
	wTrx2, err := svc.Withdraw(ctx, credits.CreditWithdrawParams{
		UserID: userID, CreditType: ctName, Amount: 150, Source: "usage", SourceID: &srcW,
	})
	require.NoError(t, err)
	require.Equal(t, wTrx.ID, wTrx2.ID, "withdrawal idempotency by (owner, type, source, source_id)")

	bal, err = svc.GetBalance(ctx, userID, ctName)
	require.NoError(t, err)
	require.Equal(t, int64(50), bal.Balance, "retry must not double-spend")
}
