//go:build integration

package budgets_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/modules/budgets"
	"github.com/open-rails/openrails/pkg/identity"
	"github.com/stretchr/testify/require"
)

// budgetEnv spins up the budget engine over the shared migrated Postgres with an
// injectable fake clock and a fresh payer+invoker, and returns a cleanup-scoped
// context. State is scoped by the freshly generated payer id.
func budgetEnv(t *testing.T) (*budgets.Service, *clockwork.FakeClock, *pgxpool.Pool, identity.CustomerID, string, context.Context) {
	t.Helper()
	dsn := dbtest.SharedPostgresDSN(t)
	dbi := dbtest.OpenAppDB(t, dsn)
	pool := dbi.Pool()
	ctx := context.Background()

	var hasTable bool
	require.NoError(t, pool.QueryRow(ctx,
		"SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_schema='billing' AND table_name='budget_window_state')").
		Scan(&hasTable))
	if !hasTable {
		t.Skip("openrails.budget_window_state missing; run migration 005")
	}

	payer := identity.CustomerIDFromString(uuid.NewString())
	payerID := payer.UUID()
	invoker := "invoker_" + uuid.NewString()
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.budget_inflight_holds WHERE customer_id = $1", payerID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.budget_window_state WHERE customer_id = $1", payerID)
	})

	// Fixed wall clock so window math is deterministic; advance it to cross
	// window boundaries.
	clk := clockwork.NewFakeClockAt(time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC))
	return budgets.NewService(dbi, clk), clk, pool, payer, invoker, ctx
}

// Money literals in this test use USD internal units.
const (
	dollar = int64(1_000_000)
	cent   = int64(10_000)
)

// windows returns a "$2 per 5h session, $14 per 7d fixed" budget, the
// cozy-art-style tier shape: the 5h window opens at the user's first charged
// request and closes exactly 5h later; the 7d window ticks on a fixed cadence
// anchored at first use.
func windows() []budgets.BudgetWindow {
	return []budgets.BudgetWindow{
		{Key: "5h", WindowSeconds: 5 * 3600, Limit: 2 * dollar, Cadence: budgets.CadenceSession},
		{Key: "7d", WindowSeconds: 7 * 24 * 3600, Limit: 14 * dollar, Cadence: budgets.CadenceFixed},
	}
}

func TestReserve_WithinBudget_Allowed(t *testing.T) {
	svc, clk, _, payer, invoker, ctx := budgetEnv(t)
	t0 := clk.Now().UTC()

	id, statuses, allowed, err := svc.Reserve(ctx, payer, invoker, "USD", windows(), 1*dollar, "gen", "req-1", time.Hour)
	require.NoError(t, err)
	require.True(t, allowed)
	require.NotEqual(t, uuid.Nil, id)
	require.Len(t, statuses, 2)
	require.Equal(t, int64(1*dollar), statuses[0].Reserved)
	require.Equal(t, int64(0), statuses[0].Used)
	require.Equal(t, int64(1*dollar), statuses[0].Remaining) // $2 limit - $1 reserved

	// The charge opened both windows at now: boundaries are exact and knowable.
	require.Equal(t, t0, statuses[0].WindowStart, "5h window opened at the first charged request")
	require.Equal(t, t0.Add(5*time.Hour), statuses[0].ResetAt, "exact 5h reset boundary")
	require.Equal(t, budgets.CadenceSession, statuses[0].Cadence)
	require.Equal(t, t0, statuses[1].WindowStart, "7d window anchored at the first charged request")
	require.Equal(t, t0.Add(7*24*time.Hour), statuses[1].ResetAt)
	require.Equal(t, budgets.CadenceFixed, statuses[1].Cadence)
}

func TestReserve_OverWindowLimit_Denied(t *testing.T) {
	svc, _, _, payer, invoker, ctx := budgetEnv(t)

	// $3 request exceeds the $2 / 5h window even though it fits the $14 / 7d.
	id, statuses, allowed, err := svc.Reserve(ctx, payer, invoker, "USD", windows(), 3*dollar, "gen", "req-big", time.Hour)
	require.NoError(t, err)
	require.False(t, allowed)
	require.Equal(t, uuid.Nil, id)
	require.False(t, statuses[0].Allowed, "5h window must deny")
	require.True(t, statuses[1].Allowed, "7d window alone would allow")
	// The request is larger than the whole 5h window: no reset will ever allow
	// it, so no retry hint.
	require.Equal(t, int64(0), statuses[0].RetryAfterSeconds)

	// Nothing was inserted on a denied reserve — and crucially, a denied FIRST
	// request does not start the user's windows.
	check, _, err := svc.Check(ctx, payer, invoker, "USD", windows(), 0)
	require.NoError(t, err)
	require.Equal(t, int64(0), check[0].Reserved)
	require.Equal(t, int64(0), check[0].Used)
	require.True(t, check[0].WindowStart.IsZero(), "denied first request must not open a window")
	require.True(t, check[1].WindowStart.IsZero())
}

func TestCapture_ConsumesUsed(t *testing.T) {
	svc, _, _, payer, invoker, ctx := budgetEnv(t)

	id, _, allowed, err := svc.Reserve(ctx, payer, invoker, "USD", windows(), 1*dollar, "gen", "req-cap", time.Hour)
	require.NoError(t, err)
	require.True(t, allowed)

	require.NoError(t, svc.Capture(ctx, id, 80*cent))

	// A later Check sees the captured $0.80 as `used` (not reserved).
	statuses, _, err := svc.Check(ctx, payer, invoker, "USD", windows(), 0)
	require.NoError(t, err)
	require.Equal(t, int64(80*cent), statuses[0].Used)
	require.Equal(t, int64(0), statuses[0].Reserved)
	require.Equal(t, int64(120*cent), statuses[0].Remaining) // $2 - $0.80 used
}

func TestRelease_FreesReserved(t *testing.T) {
	svc, _, _, payer, invoker, ctx := budgetEnv(t)

	id, _, allowed, err := svc.Reserve(ctx, payer, invoker, "USD", windows(), 150*cent, "gen", "req-rel", time.Hour)
	require.NoError(t, err)
	require.True(t, allowed)

	// Before release: $1.50 reserved.
	statuses, _, err := svc.Check(ctx, payer, invoker, "USD", windows(), 0)
	require.NoError(t, err)
	require.Equal(t, int64(150*cent), statuses[0].Reserved)

	require.NoError(t, svc.Release(ctx, id))

	// After release: reservation no longer counts.
	statuses, _, err = svc.Check(ctx, payer, invoker, "USD", windows(), 0)
	require.NoError(t, err)
	require.Equal(t, int64(0), statuses[0].Reserved)
	require.Equal(t, int64(0), statuses[0].Used)
	require.Equal(t, int64(2*dollar), statuses[0].Remaining)
}

// TestSessionWindow_ResetsAtBoundary is the core fixed-vs-rolling semantic: the
// whole budget returns at the exact window boundary (no gradual age-out), and
// the next session window opens at the NEXT charged request, not on a cadence.
func TestSessionWindow_ResetsAtBoundary(t *testing.T) {
	svc, clk, _, payer, invoker, ctx := budgetEnv(t)
	t0 := clk.Now().UTC()

	// Capture the full $2 / 5h budget at t0 (opens the window: [t0, t0+5h)).
	id, _, allowed, err := svc.Reserve(ctx, payer, invoker, "USD", windows(), 2*dollar, "gen", "req-fill", time.Hour)
	require.NoError(t, err)
	require.True(t, allowed)
	require.NoError(t, svc.Capture(ctx, id, 2*dollar))

	// 4h30m in: still the SAME window — usage does not age out gradually the
	// way the old rolling engine behaved; the user waits for the boundary.
	clk.Advance(4*time.Hour + 30*time.Minute)
	statuses, _, err := svc.Check(ctx, payer, invoker, "USD", windows(), 1*dollar)
	require.NoError(t, err)
	require.Equal(t, int64(2*dollar), statuses[0].Used)
	require.False(t, statuses[0].Allowed)
	require.Equal(t, t0.Add(5*time.Hour), statuses[0].ResetAt, "reset boundary is exact and unchanged by usage")
	require.Equal(t, int64(1800), statuses[0].RetryAfterSeconds, "retry exactly at the boundary (30m)")

	// Past the boundary: the session window expired; the full budget is back.
	clk.Advance(31 * time.Minute)
	statuses, _, err = svc.Check(ctx, payer, invoker, "USD", windows(), 1*dollar)
	require.NoError(t, err)
	require.Equal(t, int64(0), statuses[0].Used, "expired session window reads fresh")
	require.True(t, statuses[0].Allowed)

	// The next charge OPENS a new session window at its own time t1 — not at
	// t0+5h. ResetAt is exactly t1+5h.
	t1 := clk.Now().UTC()
	_, statuses, allowed, err = svc.Reserve(ctx, payer, invoker, "USD", windows(), 1*dollar, "gen", "req-reopen", time.Hour)
	require.NoError(t, err)
	require.True(t, allowed)
	require.Equal(t, t1, statuses[0].WindowStart, "session window reopens at the next charged request")
	require.Equal(t, t1.Add(5*time.Hour), statuses[0].ResetAt)
}

// TestFixedCadence_AdvancesOnSchedule: the 7d window's boundaries tick at
// anchor + k*7d regardless of activity — same wall-clock reset each week.
func TestFixedCadence_AdvancesOnSchedule(t *testing.T) {
	svc, clk, _, payer, invoker, ctx := budgetEnv(t)
	t0 := clk.Now().UTC()

	// Wide-open 5h window so only the 7d budget binds in this test.
	w := []budgets.BudgetWindow{
		{Key: "5h", WindowSeconds: 5 * 3600, Limit: 1000 * dollar, Cadence: budgets.CadenceSession},
		{Key: "7d", WindowSeconds: 7 * 24 * 3600, Limit: 14 * dollar, Cadence: budgets.CadenceFixed},
	}

	// Anchor the 7d window at t0 and consume all of it.
	id, _, allowed, err := svc.Reserve(ctx, payer, invoker, "USD", w, 14*dollar, "gen", "req-week-fill", time.Hour)
	require.NoError(t, err)
	require.True(t, allowed)
	require.NoError(t, svc.Capture(ctx, id, 14*dollar))

	// 3 days in: same period, still exhausted, boundary still t0+7d exactly.
	clk.Advance(72 * time.Hour)
	statuses, _, err := svc.Check(ctx, payer, invoker, "USD", w, 1*dollar)
	require.NoError(t, err)
	require.Equal(t, int64(14*dollar), statuses[1].Used)
	require.False(t, statuses[1].Allowed)
	require.Equal(t, t0, statuses[1].WindowStart)
	require.Equal(t, t0.Add(7*24*time.Hour), statuses[1].ResetAt)

	// Past the weekly boundary (idle the whole time): the new period derives
	// from the ANCHOR — WindowStart is exactly t0+7d, not "now".
	clk.Advance((4*24 + 1) * time.Hour) // t0 + 7d + 1h
	statuses, _, err = svc.Check(ctx, payer, invoker, "USD", w, 1*dollar)
	require.NoError(t, err)
	require.Equal(t, int64(0), statuses[1].Used, "new weekly period")
	require.True(t, statuses[1].Allowed)
	require.Equal(t, t0.Add(7*24*time.Hour), statuses[1].WindowStart, "period start derives from the anchor")
	require.Equal(t, t0.Add(14*24*time.Hour), statuses[1].ResetAt, "next reset = anchor + 2 weeks")

	// Charging in the new period keeps the anchor-derived boundary.
	_, statuses, allowed, err = svc.Reserve(ctx, payer, invoker, "USD", w, 1*dollar, "gen", "req-week-2", time.Hour)
	require.NoError(t, err)
	require.True(t, allowed)
	require.Equal(t, t0.Add(7*24*time.Hour), statuses[1].WindowStart)
}

// TestPerUserStaggeredBoundaries: two users' windows anchor at their own first
// charged request — no shared/global reset boundary.
func TestPerUserStaggeredBoundaries(t *testing.T) {
	svc, clk, _, payerA, invokerA, ctx := budgetEnv(t)
	payerB := identity.CustomerIDFromString(uuid.NewString())
	invokerB := "invoker_" + uuid.NewString()

	tA := clk.Now().UTC()
	_, stA, allowed, err := svc.Reserve(ctx, payerA, invokerA, "USD", windows(), 1*dollar, "gen", "req-a", time.Hour)
	require.NoError(t, err)
	require.True(t, allowed)

	clk.Advance(37 * time.Minute)
	tB := clk.Now().UTC()
	_, stB, allowed, err := svc.Reserve(ctx, payerB, invokerB, "USD", windows(), 1*dollar, "gen", "req-b", time.Hour)
	require.NoError(t, err)
	require.True(t, allowed)

	require.Equal(t, tA.Add(5*time.Hour), stA[0].ResetAt)
	require.Equal(t, tB.Add(5*time.Hour), stB[0].ResetAt)
	require.NotEqual(t, stA[0].ResetAt, stB[0].ResetAt, "boundaries are per-user, staggered by first use")
}

func TestReserve_Idempotent(t *testing.T) {
	svc, _, _, payer, invoker, ctx := budgetEnv(t)

	id1, _, allowed1, err := svc.Reserve(ctx, payer, invoker, "USD", windows(), 1*dollar, "gen", "req-idem", time.Hour)
	require.NoError(t, err)
	require.True(t, allowed1)

	// Replay with the same (source, source_id): same id, still allowed, no double-count.
	id2, statuses, allowed2, err := svc.Reserve(ctx, payer, invoker, "USD", windows(), 1*dollar, "gen", "req-idem", time.Hour)
	require.NoError(t, err)
	require.True(t, allowed2)
	require.Equal(t, id1, id2, "idempotent reserve returns the same reservation id")
	require.Equal(t, int64(1*dollar), statuses[0].Reserved, "no double reservation")
}

// TestConcurrentReserves_SerializeOnWindowState: once a window-state row
// exists, concurrent reserves lock it FOR UPDATE, so two requests that
// together exceed the budget cannot both pass (the rolling engine had no such
// serialization point).
func TestConcurrentReserves_SerializeOnWindowState(t *testing.T) {
	svc, _, _, payer, invoker, ctx := budgetEnv(t)

	// Open the windows with a tiny charge so the state rows exist to lock.
	_, _, allowed, err := svc.Reserve(ctx, payer, invoker, "USD", windows(), 10*cent, "gen", "req-warm", time.Hour)
	require.NoError(t, err)
	require.True(t, allowed)

	// Remaining is $1.90; each request wants $1.50 — only one may pass.
	var wg sync.WaitGroup
	results := make([]bool, 2)
	errs := make([]error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _, ok, err := svc.Reserve(ctx, payer, invoker, "USD", windows(), 150*cent, "gen", "req-race-"+uuid.NewString(), time.Hour)
			results[i] = ok
			errs[i] = err
		}(i)
	}
	wg.Wait()
	require.NoError(t, errs[0])
	require.NoError(t, errs[1])
	require.NotEqual(t, results[0], results[1], "exactly one of two over-budget concurrent reserves may pass, got %v", results)
}

// TestBoundaryStraddle_DocumentsAcceptedTradeoff: a user can spend up to ~2x a
// window's limit straddling their own boundary — the accepted, industry-standard
// fixed-window behavior (decided 2026-06-10).
func TestBoundaryStraddle_DocumentsAcceptedTradeoff(t *testing.T) {
	svc, clk, _, payer, invoker, ctx := budgetEnv(t)

	id, _, allowed, err := svc.Reserve(ctx, payer, invoker, "USD", windows(), 2*dollar, "gen", "req-straddle-1", time.Hour)
	require.NoError(t, err)
	require.True(t, allowed)
	require.NoError(t, svc.Capture(ctx, id, 2*dollar))

	clk.Advance(5*time.Hour + time.Second)

	_, _, allowed, err = svc.Reserve(ctx, payer, invoker, "USD", windows(), 2*dollar, "gen", "req-straddle-2", time.Hour)
	require.NoError(t, err)
	require.True(t, allowed, "full budget available immediately after the boundary")
}
