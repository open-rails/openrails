//go:build integration

package money_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/modules/money"
	riverjobs "github.com/open-rails/openrails/internal/river"
	"github.com/open-rails/openrails/pkg/identity"
	"github.com/riverqueue/river"
	"github.com/stretchr/testify/require"
)

// windowEnv provisions a migrated DB + money service + one funded payer.
func windowEnv(t *testing.T, depositAmount int64) (context.Context, *pgxpool.Pool, *money.MoneyService, identity.CustomerID, string) {
	t.Helper()
	ctx := context.Background()

	dsn := dbtest.SharedPostgresDSN(t)
	dbi := dbtest.OpenAppDB(t, dsn)
	pool := dbi.Pool()
	dbtest.EnsureTestMerchant(ctx, t, pool)
	ctx = dbtest.WithTestMerchant(ctx)

	svc := money.NewMoneyService(dbi)

	payer := identity.CustomerIDFromString(uuid.NewString())
	payerID := payer.UUID()

	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.usage_events WHERE customer_id = $1", payerID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.money_windows WHERE customer_id = $1", payerID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.money_blocks WHERE customer_id = $1", payerID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.money_transactions WHERE customer_id = $1", payerID)
	})

	if depositAmount > 0 {
		_, err := svc.Deposit(ctx, money.DepositParams{
			CustomerID: &payer,
			Invoker:    payer.UUID().String(),
			Currency:   money.DefaultCurrency,
			Amount:     depositAmount,
			Source:     "test_deposit",
		})
		require.NoError(t, err)
	}
	return ctx, pool, svc, payer, money.DefaultCurrency
}

func requireBalance(t *testing.T, ctx context.Context, svc *money.MoneyService, payer identity.CustomerID, currency string, balance, held int64) {
	t.Helper()
	bal, err := svc.GetBalanceForCustomer(ctx, payer, currency)
	require.NoError(t, err)
	require.Equal(t, balance, bal.Balance, "balance")
	require.Equal(t, held, bal.HeldBalance, "held_balance")
}

// TestCreditWindows_Lifecycle covers open -> settle -> settle-replay
// (idempotent) -> over-settle rejected -> refill -> close-releases-remainder,
// asserting the ledger invariants at every step.
func TestCreditWindows_Lifecycle(t *testing.T) {
	ctx, pool, svc, payer, cur := windowEnv(t, 1000)

	// Open: funds leave available immediately (a REAL hold).
	w, err := svc.OpenWindow(ctx, money.OpenWindowParams{
		Payer: payer, Invoker: "user:alice", Currency: money.DefaultCurrency, Amount: 600,
		ExpiresAt: time.Now().Add(10 * time.Minute).UTC(),
	})
	require.NoError(t, err)
	require.Equal(t, "open", w.Status)
	require.Equal(t, int64(600), w.HeldAmount)
	requireBalance(t, ctx, svc, payer, cur, 1000, 600)

	// Settle two actuals in one batch.
	results, err := svc.SettleWindowItems(ctx, []money.WindowSettleItem{
		{WindowID: w.ID, RequestID: "req_1", Amount: 100, EventType: "test/endpoint", Resource: "test/endpoint"},
		{WindowID: w.ID, RequestID: "req_2", Amount: 50},
	})
	require.NoError(t, err)
	require.Len(t, results, 2)
	for _, res := range results {
		require.True(t, res.OK, "settle %s: %s", res.RequestID, res.ErrorCode)
		require.False(t, res.Replayed)
	}
	requireBalance(t, ctx, svc, payer, cur, 850, 450)

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
	results, err = svc.SettleWindowItems(ctx, []money.WindowSettleItem{
		{WindowID: w.ID, RequestID: "req_1", Amount: 100},
	})
	require.NoError(t, err)
	require.True(t, results[0].OK)
	require.True(t, results[0].Replayed)
	requireBalance(t, ctx, svc, payer, cur, 850, 450)

	// Over-settle: 500 > (600 held - 150 settled) -> rejected server-side; the
	// failure is per-item and the window/ledger are untouched.
	results, err = svc.SettleWindowItems(ctx, []money.WindowSettleItem{
		{WindowID: w.ID, RequestID: "req_over", Amount: 500},
	})
	require.NoError(t, err)
	require.False(t, results[0].OK)
	require.Equal(t, "window_exceeded", results[0].ErrorCode)
	requireBalance(t, ctx, svc, payer, cur, 850, 450)

	// Refill: extend held + TTL (gated on available funds like Hold).
	w2, err := svc.RefillWindow(ctx, w.ID, 200, time.Now().Add(20*time.Minute).UTC())
	require.NoError(t, err)
	require.Equal(t, int64(800), w2.HeldAmount)
	requireBalance(t, ctx, svc, payer, cur, 850, 650)

	// Refill past available (850-650=200 available, ask 300) -> denied.
	_, err = svc.RefillWindow(ctx, w.ID, 300, time.Time{})
	require.ErrorIs(t, err, money.ErrInsufficientCredits)

	// Close: the unsettled remainder (800-150=650) releases back to available.
	closed, err := svc.CloseWindow(ctx, w.ID)
	require.NoError(t, err)
	require.Equal(t, "closed", closed.Status)
	requireBalance(t, ctx, svc, payer, cur, 850, 0)

	// Close is idempotent.
	closed, err = svc.CloseWindow(ctx, w.ID)
	require.NoError(t, err)
	require.Equal(t, "closed", closed.Status)
	requireBalance(t, ctx, svc, payer, cur, 850, 0)

	// Replay of an already-settled item still succeeds after close...
	results, err = svc.SettleWindowItems(ctx, []money.WindowSettleItem{
		{WindowID: w.ID, RequestID: "req_2", Amount: 50},
	})
	require.NoError(t, err)
	require.True(t, results[0].OK)
	require.True(t, results[0].Replayed)

	// ...but a NEW settle against a closed window fails closed.
	results, err = svc.SettleWindowItems(ctx, []money.WindowSettleItem{
		{WindowID: w.ID, RequestID: "req_new", Amount: 10},
	})
	require.NoError(t, err)
	require.False(t, results[0].OK)
	require.Equal(t, "window_not_open", results[0].ErrorCode)
	requireBalance(t, ctx, svc, payer, cur, 850, 0)
}

// TestCreditWindows_OpenInsufficientFunds: NO optimistic approval — a window
// open (or refill) beyond available funds is denied exactly like Hold.
func TestCreditWindows_OpenInsufficientFunds(t *testing.T) {
	ctx, _, svc, payer, cur := windowEnv(t, 100)

	_, err := svc.OpenWindow(ctx, money.OpenWindowParams{
		Payer: payer, Invoker: "user:poor", Currency: money.DefaultCurrency, Amount: 200,
		ExpiresAt: time.Now().Add(10 * time.Minute).UTC(),
	})
	require.ErrorIs(t, err, money.ErrInsufficientCredits)

	// A $0 payer cannot open a window at all.
	broke := identity.CustomerIDFromString(uuid.NewString())
	_, err = svc.OpenWindow(ctx, money.OpenWindowParams{
		Payer: broke, Invoker: "user:broke", Currency: money.DefaultCurrency, Amount: 1,
		ExpiresAt: time.Now().Add(10 * time.Minute).UTC(),
	})
	require.ErrorIs(t, err, money.ErrInsufficientCredits)

	// Partial funds work; the SECOND open is gated on what's left available.
	w, err := svc.OpenWindow(ctx, money.OpenWindowParams{
		Payer: payer, Invoker: "user:poor", Currency: money.DefaultCurrency, Amount: 80,
		ExpiresAt: time.Now().Add(10 * time.Minute).UTC(),
	})
	require.NoError(t, err)
	requireBalance(t, ctx, svc, payer, cur, 100, 80)
	_, err = svc.OpenWindow(ctx, money.OpenWindowParams{
		Payer: payer, Invoker: "user:poor", Currency: money.DefaultCurrency, Amount: 50,
		ExpiresAt: time.Now().Add(10 * time.Minute).UTC(),
	})
	require.ErrorIs(t, err, money.ErrInsufficientCredits)

	_, err = svc.CloseWindow(ctx, w.ID)
	require.NoError(t, err)
}

// TestCreditWindows_ExpiryReleasesRemainder: the hold-expiry sweep also expires
// windows — the unsettled remainder returns to available, settled spend stays
// spent, and post-expiry settles fail closed (replays still succeed).
func TestCreditWindows_ExpiryReleasesRemainder(t *testing.T) {
	ctx, _, svc, payer, cur := windowEnv(t, 500)

	dsn := dbtest.SharedPostgresDSN(t)
	dbi := dbtest.OpenAppDB(t, dsn)

	// Already past expiry; the sweep hasn't run yet.
	w, err := svc.OpenWindow(ctx, money.OpenWindowParams{
		Payer: payer, Invoker: "user:exp", Currency: money.DefaultCurrency, Amount: 300,
		ExpiresAt: time.Now().Add(-1 * time.Minute).UTC(),
	})
	require.NoError(t, err)
	requireBalance(t, ctx, svc, payer, cur, 500, 300)

	// Funds are still held until the sweep runs, so a pre-sweep settle lands.
	results, err := svc.SettleWindowItems(ctx, []money.WindowSettleItem{
		{WindowID: w.ID, RequestID: "req_pre_sweep", Amount: 100},
	})
	require.NoError(t, err)
	require.True(t, results[0].OK)
	requireBalance(t, ctx, svc, payer, cur, 400, 200)

	// The shared periodic job (hold expiry) sweeps windows too.
	worker := &riverjobs.HoldExpiryWorker{DB: dbi}
	job := &river.Job[riverjobs.HoldExpiryArgs]{Args: riverjobs.HoldExpiryArgs{}}
	require.NoError(t, worker.Work(ctx, job))

	got, err := svc.GetWindow(ctx, w.ID)
	require.NoError(t, err)
	require.Equal(t, "expired", got.Status)
	requireBalance(t, ctx, svc, payer, cur, 400, 0)

	// New settles against the expired window fail closed; replays still succeed.
	results, err = svc.SettleWindowItems(ctx, []money.WindowSettleItem{
		{WindowID: w.ID, RequestID: "req_post_sweep", Amount: 50},
		{WindowID: w.ID, RequestID: "req_pre_sweep", Amount: 100},
	})
	require.NoError(t, err)
	require.False(t, results[0].OK)
	require.Equal(t, "window_not_open", results[0].ErrorCode)
	require.True(t, results[1].OK)
	require.True(t, results[1].Replayed)
	requireBalance(t, ctx, svc, payer, cur, 400, 0)
}

// TestCreditWindows_CrossPayerSettleBatch: one settle flush mixes windows and
// payers freely, with per-item isolation — bad items error individually while
// every good item lands.
func TestCreditWindows_CrossPayerSettleBatch(t *testing.T) {
	ctx, pool, svc, payerA, cur := windowEnv(t, 1000)

	payerB := identity.CustomerIDFromString(uuid.NewString())
	t.Cleanup(func() {
		pid := payerB.UUID()
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.usage_events WHERE customer_id = $1", pid)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.money_windows WHERE customer_id = $1", pid)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.money_blocks WHERE customer_id = $1", pid)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.money_transactions WHERE customer_id = $1", pid)
	})
	_, err := svc.Deposit(ctx, money.DepositParams{
		CustomerID: &payerB,
		Invoker:    payerB.UUID().String(),
		Currency:   money.DefaultCurrency,
		Amount:     400,
		Source:     "test_deposit",
	})
	require.NoError(t, err)

	wa, err := svc.OpenWindow(ctx, money.OpenWindowParams{
		Payer: payerA, Invoker: "user:a", Currency: money.DefaultCurrency, Amount: 300,
		ExpiresAt: time.Now().Add(10 * time.Minute).UTC(),
	})
	require.NoError(t, err)
	wb, err := svc.OpenWindow(ctx, money.OpenWindowParams{
		Payer: payerB, Invoker: "user:b", Currency: money.DefaultCurrency, Amount: 200,
		ExpiresAt: time.Now().Add(10 * time.Minute).UTC(),
	})
	require.NoError(t, err)

	results, err := svc.SettleWindowItems(ctx, []money.WindowSettleItem{
		{WindowID: wa.ID, RequestID: "xp_a1", Amount: 120},                 // payer A — ok
		{WindowID: wb.ID, RequestID: "xp_b1", Amount: 30},                  // payer B — ok
		{WindowID: uuid.New(), RequestID: "xp_missing", Amount: 10},        // unknown window
		{WindowID: wb.ID, RequestID: "xp_b_over", Amount: 1000},            // over-settle B
		{WindowID: wa.ID, RequestID: "xp_a2", Amount: 80, Invoker: "u:a2"}, // payer A again — ok
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

	requireBalance(t, ctx, svc, payerA, cur, 800, 100) // 1000-200 settled; 300-200 still held
	requireBalance(t, ctx, svc, payerB, cur, 370, 170) // 400-30; 200-30

	_, err = svc.CloseWindow(ctx, wa.ID)
	require.NoError(t, err)
	_, err = svc.CloseWindow(ctx, wb.ID)
	require.NoError(t, err)
	requireBalance(t, ctx, svc, payerA, cur, 800, 0)
	requireBalance(t, ctx, svc, payerB, cur, 370, 0)
}
