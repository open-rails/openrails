//go:build integration

package budgets_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/modules/budgets"
	"github.com/open-rails/openrails/pkg/identity"
	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"
)

// budgetEnv spins up the budget engine over the shared migrated Postgres with an
// injectable fake clock and a fresh payer+actor, and returns a cleanup-scoped
// context. State is scoped by the freshly generated payer id.
func budgetEnv(t *testing.T) (*budgets.Service, *clockwork.FakeClock, *bun.DB, identity.TenantSubjectID, string, context.Context) {
	t.Helper()
	dsn := dbtest.SharedPostgresDSN(t)
	sqlDB := sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dsn)))
	t.Cleanup(func() { _ = sqlDB.Close() })
	bunDB := bun.NewDB(sqlDB, pgdialect.New())
	models.RegisterModels(bunDB)
	ctx := context.Background()
	require.NoError(t, bunDB.PingContext(ctx))

	var hasTable bool
	require.NoError(t, bunDB.NewSelect().
		ColumnExpr("EXISTS (SELECT 1 FROM information_schema.tables WHERE table_schema='billing' AND table_name='budget_reservations')").
		Scan(ctx, &hasTable))
	if !hasTable {
		t.Skip("billing.budget_reservations missing; run migration 068")
	}

	dbi, err := db.NewWithBun(bunDB)
	require.NoError(t, err)

	payer := identity.TenantSubjectIDFromString(uuid.NewString())
	payerID := payer.UUID()
	actor := "actor_" + uuid.NewString()
	t.Cleanup(func() {
		_, _ = bunDB.NewDelete().Model((*models.BudgetReservation)(nil)).Where("tenant_subject_id = ?", payerID).Exec(ctx)
	})

	// Fixed wall clock so window math is deterministic; advance it to age rows out.
	clk := clockwork.NewFakeClockAt(time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC))
	return budgets.NewService(dbi, clk), clk, bunDB, payer, actor, ctx
}

// windows returns a "$2 per 4h, $5 per week" budget in micros.
// $1 = 100 cents = 100_000 micros.
func windows() []budgets.BudgetWindow {
	return []budgets.BudgetWindow{
		{Key: "4h", WindowSeconds: 4 * 3600, LimitMicros: 200_000},        // $2
		{Key: "week", WindowSeconds: 7 * 24 * 3600, LimitMicros: 500_000}, // $5
	}
}

func TestReserve_WithinBudget_Allowed(t *testing.T) {
	svc, _, _, payer, actor, ctx := budgetEnv(t)

	id, statuses, allowed, err := svc.Reserve(ctx, payer, actor, windows(), 100_000, "gen", "req-1", time.Hour)
	require.NoError(t, err)
	require.True(t, allowed)
	require.NotEqual(t, uuid.Nil, id)
	require.Len(t, statuses, 2)
	require.Equal(t, int64(100_000), statuses[0].Reserved)
	require.Equal(t, int64(0), statuses[0].Used)
	require.Equal(t, int64(100_000), statuses[0].Remaining) // 200k limit - 100k reserved
}

func TestReserve_OverWindowLimit_Denied(t *testing.T) {
	svc, _, _, payer, actor, ctx := budgetEnv(t)

	// $3 request exceeds the $2 / 4h window even though it fits the $5 / week.
	id, statuses, allowed, err := svc.Reserve(ctx, payer, actor, windows(), 300_000, "gen", "req-big", time.Hour)
	require.NoError(t, err)
	require.False(t, allowed)
	require.Equal(t, uuid.Nil, id)
	require.False(t, statuses[0].Allowed, "4h window must deny")
	require.True(t, statuses[1].Allowed, "week window alone would allow")
	require.Greater(t, statuses[0].RetryAfterSeconds, int64(0))

	// Nothing was inserted on a denied reserve: a follow-up Check shows zero
	// reserved/used.
	check, _, err := svc.Check(ctx, payer, actor, windows(), 0)
	require.NoError(t, err)
	require.Equal(t, int64(0), check[0].Reserved)
	require.Equal(t, int64(0), check[0].Used)
}

func TestCapture_ConsumesUsed(t *testing.T) {
	svc, _, _, payer, actor, ctx := budgetEnv(t)

	id, _, allowed, err := svc.Reserve(ctx, payer, actor, windows(), 100_000, "gen", "req-cap", time.Hour)
	require.NoError(t, err)
	require.True(t, allowed)

	require.NoError(t, svc.Capture(ctx, id, 80_000))

	// A later Check sees the captured 80k as `used` (not reserved).
	statuses, _, err := svc.Check(ctx, payer, actor, windows(), 0)
	require.NoError(t, err)
	require.Equal(t, int64(80_000), statuses[0].Used)
	require.Equal(t, int64(0), statuses[0].Reserved)
	require.Equal(t, int64(120_000), statuses[0].Remaining) // 200k - 80k used
}

func TestRelease_FreesReserved(t *testing.T) {
	svc, _, _, payer, actor, ctx := budgetEnv(t)

	id, _, allowed, err := svc.Reserve(ctx, payer, actor, windows(), 150_000, "gen", "req-rel", time.Hour)
	require.NoError(t, err)
	require.True(t, allowed)

	// Before release: 150k reserved.
	statuses, _, err := svc.Check(ctx, payer, actor, windows(), 0)
	require.NoError(t, err)
	require.Equal(t, int64(150_000), statuses[0].Reserved)

	require.NoError(t, svc.Release(ctx, id))

	// After release: reservation no longer counts.
	statuses, _, err = svc.Check(ctx, payer, actor, windows(), 0)
	require.NoError(t, err)
	require.Equal(t, int64(0), statuses[0].Reserved)
	require.Equal(t, int64(0), statuses[0].Used)
	require.Equal(t, int64(200_000), statuses[0].Remaining)
}

func TestReserve_RollsOutOfWindow(t *testing.T) {
	svc, clk, _, payer, actor, ctx := budgetEnv(t)

	// Capture the full $2 / 4h budget.
	id, _, allowed, err := svc.Reserve(ctx, payer, actor, windows(), 200_000, "gen", "req-roll", time.Hour)
	require.NoError(t, err)
	require.True(t, allowed)
	require.NoError(t, svc.Capture(ctx, id, 200_000))

	// Immediately, the 4h window is full -> a new request is denied.
	_, statuses, allowed, err := svc.Reserve(ctx, payer, actor, windows(), 100_000, "gen", "req-roll-2", time.Hour)
	require.NoError(t, err)
	require.False(t, allowed)
	require.Equal(t, int64(200_000), statuses[0].Used)

	// Advance the fake clock 5h: the captured row ages out of the 4h window
	// (but is still within the 1-week window).
	clk.Advance(5 * time.Hour)

	statuses, _, err = svc.Check(ctx, payer, actor, windows(), 100_000)
	require.NoError(t, err)
	require.Equal(t, int64(0), statuses[0].Used, "4h window: captured row aged out")
	require.True(t, statuses[0].Allowed, "4h window now allows")
	require.Equal(t, int64(200_000), statuses[1].Used, "week window: still counts")
	require.True(t, statuses[1].Allowed, "week window still under $5")
}

func TestReserve_Idempotent(t *testing.T) {
	svc, _, _, payer, actor, ctx := budgetEnv(t)

	id1, _, allowed1, err := svc.Reserve(ctx, payer, actor, windows(), 100_000, "gen", "req-idem", time.Hour)
	require.NoError(t, err)
	require.True(t, allowed1)

	// Replay with the same (source, source_id): same id, still allowed, no double-count.
	id2, statuses, allowed2, err := svc.Reserve(ctx, payer, actor, windows(), 100_000, "gen", "req-idem", time.Hour)
	require.NoError(t, err)
	require.True(t, allowed2)
	require.Equal(t, id1, id2, "idempotent reserve returns the same reservation id")
	require.Equal(t, int64(100_000), statuses[0].Reserved, "no double reservation")
}
