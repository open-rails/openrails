//go:build integration

package credits_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/modules/credits"
	riverjobs "github.com/open-rails/openrails/internal/river"
	"github.com/open-rails/openrails/pkg/identity"
	"github.com/riverqueue/river"
	"github.com/stretchr/testify/require"
)

// windowEnv provisions a migrated DB + credits service + one funded payer.
func windowEnv(t *testing.T, depositMicros int64) (context.Context, *pgxpool.Pool, *credits.CreditsService, identity.TenantSubjectID, string) {
	t.Helper()
	ctx := context.Background()

	dsn := dbtest.SharedPostgresDSN(t)
	dbi := dbtest.OpenAppDB(t, dsn)
	pool := dbi.Pool()

	var hasWindows bool
	require.NoError(t, pool.QueryRow(ctx,
		"SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_schema='billing' AND table_name='credit_windows')").
		Scan(&hasWindows))
	if !hasWindows {
		t.Skip("openrails.credit_windows not found; run migrations before integration tests")
	}

	svc := credits.NewCreditsService(dbi)

	now := time.Now().UTC().Truncate(time.Second)
	ctName := "test_windows_" + uuid.NewString()
	ctID := uuid.New()
	_, err := gen.New(pool).CreateCreditType(ctx, gen.CreateCreditTypeParams{
		ID: ctID, Name: ctName, DisplayName: "Test Windows", Unit: "usd", DecimalPlaces: 6, IsActive: true, CreatedAt: now,
	})
	require.NoError(t, err)

	payer := identity.TenantSubjectIDFromString(uuid.NewString())

	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.usage_events WHERE credit_type_id = $1", ctID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.credit_windows WHERE credit_type_id = $1", ctID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.credit_blocks WHERE credit_type_id = $1", ctID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.credit_transactions WHERE credit_type_id = $1", ctID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.credit_balances WHERE credit_type_id = $1", ctID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.credit_types WHERE id = $1", ctID)
	})

	if depositMicros > 0 {
		_, err = svc.Deposit(ctx, credits.CreditDepositParams{
			TenantSubjectID: &payer,
			Actor:           payer.UUID().String(),
			CreditType:      ctName,
			Amount:          depositMicros,
			Source:          "test_deposit",
		})
		require.NoError(t, err)
	}
	return ctx, pool, svc, payer, ctName
}

func requireBalance(t *testing.T, ctx context.Context, svc *credits.CreditsService, payer identity.TenantSubjectID, ctName string, balance, held int64) {
	t.Helper()
	bal, err := svc.GetBalanceForTenantSubject(ctx, payer, ctName)
	require.NoError(t, err)
	require.Equal(t, balance, bal.Balance, "balance")
	require.Equal(t, held, bal.HeldBalance, "held_balance")
}

// TestCreditWindows_Lifecycle covers open -> settle -> settle-replay
// (idempotent) -> over-settle rejected -> refill -> close-releases-remainder,
// asserting the ledger invariants at every step.
func TestCreditWindows_Lifecycle(t *testing.T) {
	ctx, pool, svc, payer, ctName := windowEnv(t, 1000)

	// Open: funds leave available immediately (a REAL hold).
	w, err := svc.OpenWindow(ctx, credits.OpenWindowParams{
		Payer: payer, Actor: "user:alice", CreditType: ctName, Amount: 600,
		ExpiresAt: time.Now().Add(10 * time.Minute).UTC(),
	})
	require.NoError(t, err)
	require.Equal(t, "open", w.Status)
	require.Equal(t, int64(600), w.HeldAmount)
	requireBalance(t, ctx, svc, payer, ctName, 1000, 600)

	// Settle two actuals in one batch.
	results, err := svc.SettleWindowItems(ctx, []credits.WindowSettleItem{
		{WindowID: w.ID, RequestID: "req_1", Amount: 100, EventType: "test/endpoint", Resource: "test/endpoint"},
		{WindowID: w.ID, RequestID: "req_2", Amount: 50},
	})
	require.NoError(t, err)
	require.Len(t, results, 2)
	for _, res := range results {
		require.True(t, res.OK, "settle %s: %s", res.RequestID, res.ErrorCode)
		require.False(t, res.Replayed)
	}
	requireBalance(t, ctx, svc, payer, ctName, 850, 450)

	got, err := svc.GetWindow(ctx, w.ID)
	require.NoError(t, err)
	require.Equal(t, int64(150), got.SettledAmount)

	// Usage attribution rode along with req_1.
	var usageCount int
	require.NoError(t, pool.QueryRow(ctx,
		"SELECT count(*) FROM openrails.usage_events WHERE source = 'window_settle' AND source_id = 'req_1'").
		Scan(&usageCount))
	require.Equal(t, 1, usageCount)

	// Idempotent replay: same request_id -> success, NOTHING re-charged.
	results, err = svc.SettleWindowItems(ctx, []credits.WindowSettleItem{
		{WindowID: w.ID, RequestID: "req_1", Amount: 100},
	})
	require.NoError(t, err)
	require.True(t, results[0].OK)
	require.True(t, results[0].Replayed)
	requireBalance(t, ctx, svc, payer, ctName, 850, 450)

	// Over-settle: 500 > (600 held - 150 settled) -> rejected server-side; the
	// failure is per-item and the window/ledger are untouched.
	results, err = svc.SettleWindowItems(ctx, []credits.WindowSettleItem{
		{WindowID: w.ID, RequestID: "req_over", Amount: 500},
	})
	require.NoError(t, err)
	require.False(t, results[0].OK)
	require.Equal(t, "window_exceeded", results[0].ErrorCode)
	requireBalance(t, ctx, svc, payer, ctName, 850, 450)

	// Refill: extend held + TTL (gated on available funds like Hold).
	w2, err := svc.RefillWindow(ctx, w.ID, 200, time.Now().Add(20*time.Minute).UTC())
	require.NoError(t, err)
	require.Equal(t, int64(800), w2.HeldAmount)
	requireBalance(t, ctx, svc, payer, ctName, 850, 650)

	// Refill past available (850-650=200 available, ask 300) -> denied.
	_, err = svc.RefillWindow(ctx, w.ID, 300, time.Time{})
	require.ErrorIs(t, err, credits.ErrInsufficientCredits)

	// Close: the unsettled remainder (800-150=650) releases back to available.
	closed, err := svc.CloseWindow(ctx, w.ID)
	require.NoError(t, err)
	require.Equal(t, "closed", closed.Status)
	requireBalance(t, ctx, svc, payer, ctName, 850, 0)

	// Close is idempotent.
	closed, err = svc.CloseWindow(ctx, w.ID)
	require.NoError(t, err)
	require.Equal(t, "closed", closed.Status)
	requireBalance(t, ctx, svc, payer, ctName, 850, 0)

	// Replay of an already-settled item still succeeds after close...
	results, err = svc.SettleWindowItems(ctx, []credits.WindowSettleItem{
		{WindowID: w.ID, RequestID: "req_2", Amount: 50},
	})
	require.NoError(t, err)
	require.True(t, results[0].OK)
	require.True(t, results[0].Replayed)

	// ...but a NEW settle against a closed window fails closed.
	results, err = svc.SettleWindowItems(ctx, []credits.WindowSettleItem{
		{WindowID: w.ID, RequestID: "req_new", Amount: 10},
	})
	require.NoError(t, err)
	require.False(t, results[0].OK)
	require.Equal(t, "window_not_open", results[0].ErrorCode)
	requireBalance(t, ctx, svc, payer, ctName, 850, 0)
}

// TestCreditWindows_OpenInsufficientFunds: NO optimistic approval — a window
// open (or refill) beyond available funds is denied exactly like Hold.
func TestCreditWindows_OpenInsufficientFunds(t *testing.T) {
	ctx, _, svc, payer, ctName := windowEnv(t, 100)

	_, err := svc.OpenWindow(ctx, credits.OpenWindowParams{
		Payer: payer, Actor: "user:poor", CreditType: ctName, Amount: 200,
		ExpiresAt: time.Now().Add(10 * time.Minute).UTC(),
	})
	require.ErrorIs(t, err, credits.ErrInsufficientCredits)

	// A $0 payer cannot open a window at all.
	broke := identity.TenantSubjectIDFromString(uuid.NewString())
	_, err = svc.OpenWindow(ctx, credits.OpenWindowParams{
		Payer: broke, Actor: "user:broke", CreditType: ctName, Amount: 1,
		ExpiresAt: time.Now().Add(10 * time.Minute).UTC(),
	})
	require.ErrorIs(t, err, credits.ErrInsufficientCredits)

	// Partial funds work; the SECOND open is gated on what's left available.
	w, err := svc.OpenWindow(ctx, credits.OpenWindowParams{
		Payer: payer, Actor: "user:poor", CreditType: ctName, Amount: 80,
		ExpiresAt: time.Now().Add(10 * time.Minute).UTC(),
	})
	require.NoError(t, err)
	requireBalance(t, ctx, svc, payer, ctName, 100, 80)
	_, err = svc.OpenWindow(ctx, credits.OpenWindowParams{
		Payer: payer, Actor: "user:poor", CreditType: ctName, Amount: 50,
		ExpiresAt: time.Now().Add(10 * time.Minute).UTC(),
	})
	require.ErrorIs(t, err, credits.ErrInsufficientCredits)

	_, err = svc.CloseWindow(ctx, w.ID)
	require.NoError(t, err)
}

// TestCreditWindows_ExpiryReleasesRemainder: the hold-expiry sweep also expires
// windows — the unsettled remainder returns to available, settled spend stays
// spent, and post-expiry settles fail closed (replays still succeed).
func TestCreditWindows_ExpiryReleasesRemainder(t *testing.T) {
	ctx, _, svc, payer, ctName := windowEnv(t, 500)

	dsn := dbtest.SharedPostgresDSN(t)
	dbi := dbtest.OpenAppDB(t, dsn)

	// Already past expiry; the sweep hasn't run yet.
	w, err := svc.OpenWindow(ctx, credits.OpenWindowParams{
		Payer: payer, Actor: "user:exp", CreditType: ctName, Amount: 300,
		ExpiresAt: time.Now().Add(-1 * time.Minute).UTC(),
	})
	require.NoError(t, err)
	requireBalance(t, ctx, svc, payer, ctName, 500, 300)

	// Funds are still held until the sweep runs, so a pre-sweep settle lands.
	results, err := svc.SettleWindowItems(ctx, []credits.WindowSettleItem{
		{WindowID: w.ID, RequestID: "req_pre_sweep", Amount: 100},
	})
	require.NoError(t, err)
	require.True(t, results[0].OK)
	requireBalance(t, ctx, svc, payer, ctName, 400, 200)

	// The shared periodic job (hold expiry) sweeps windows too.
	worker := &riverjobs.HoldExpiryWorker{DB: dbi}
	job := &river.Job[riverjobs.HoldExpiryArgs]{Args: riverjobs.HoldExpiryArgs{}}
	require.NoError(t, worker.Work(ctx, job))

	got, err := svc.GetWindow(ctx, w.ID)
	require.NoError(t, err)
	require.Equal(t, "expired", got.Status)
	requireBalance(t, ctx, svc, payer, ctName, 400, 0)

	// New settles against the expired window fail closed; replays still succeed.
	results, err = svc.SettleWindowItems(ctx, []credits.WindowSettleItem{
		{WindowID: w.ID, RequestID: "req_post_sweep", Amount: 50},
		{WindowID: w.ID, RequestID: "req_pre_sweep", Amount: 100},
	})
	require.NoError(t, err)
	require.False(t, results[0].OK)
	require.Equal(t, "window_not_open", results[0].ErrorCode)
	require.True(t, results[1].OK)
	require.True(t, results[1].Replayed)
	requireBalance(t, ctx, svc, payer, ctName, 400, 0)
}

// TestCreditWindows_CrossPayerSettleBatch: one settle flush mixes windows and
// payers freely, with per-item isolation — bad items error individually while
// every good item lands.
func TestCreditWindows_CrossPayerSettleBatch(t *testing.T) {
	ctx, _, svc, payerA, ctName := windowEnv(t, 1000)

	payerB := identity.TenantSubjectIDFromString(uuid.NewString())
	_, err := svc.Deposit(ctx, credits.CreditDepositParams{
		TenantSubjectID: &payerB,
		Actor:           payerB.UUID().String(),
		CreditType:      ctName,
		Amount:          400,
		Source:          "test_deposit",
	})
	require.NoError(t, err)

	wa, err := svc.OpenWindow(ctx, credits.OpenWindowParams{
		Payer: payerA, Actor: "user:a", CreditType: ctName, Amount: 300,
		ExpiresAt: time.Now().Add(10 * time.Minute).UTC(),
	})
	require.NoError(t, err)
	wb, err := svc.OpenWindow(ctx, credits.OpenWindowParams{
		Payer: payerB, Actor: "user:b", CreditType: ctName, Amount: 200,
		ExpiresAt: time.Now().Add(10 * time.Minute).UTC(),
	})
	require.NoError(t, err)

	results, err := svc.SettleWindowItems(ctx, []credits.WindowSettleItem{
		{WindowID: wa.ID, RequestID: "xp_a1", Amount: 120},               // payer A — ok
		{WindowID: wb.ID, RequestID: "xp_b1", Amount: 30},                // payer B — ok
		{WindowID: uuid.New(), RequestID: "xp_missing", Amount: 10},      // unknown window
		{WindowID: wb.ID, RequestID: "xp_b_over", Amount: 1000},          // over-settle B
		{WindowID: wa.ID, RequestID: "xp_a2", Amount: 80, Actor: "u:a2"}, // payer A again — ok
	})
	require.NoError(t, err)
	require.Len(t, results, 5)

	require.True(t, results[0].OK)
	require.True(t, results[1].OK)
	require.False(t, results[2].OK)
	require.Equal(t, "window_not_found", results[2].ErrorCode)
	require.False(t, results[3].OK)
	require.Equal(t, "window_exceeded", results[3].ErrorCode)
	require.True(t, results[4].OK, "a later good item must land despite earlier failures")

	requireBalance(t, ctx, svc, payerA, ctName, 800, 100) // 1000-200 settled; 300-200 still held
	requireBalance(t, ctx, svc, payerB, ctName, 370, 170) // 400-30; 200-30

	_, err = svc.CloseWindow(ctx, wa.ID)
	require.NoError(t, err)
	_, err = svc.CloseWindow(ctx, wb.ID)
	require.NoError(t, err)
	requireBalance(t, ctx, svc, payerA, ctName, 800, 0)
	requireBalance(t, ctx, svc, payerB, ctName, 370, 0)
}
