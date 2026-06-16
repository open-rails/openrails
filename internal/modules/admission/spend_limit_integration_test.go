//go:build integration

package admission_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/modules/admission"
	"github.com/open-rails/openrails/internal/modules/budgets"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/pkg/identity"
	"github.com/stretchr/testify/require"
)

// === Per-tier, per-delegated-user (per-invoker) fixed-window $-spend limits at
// the admission gate (cozy-art tier_1 = {$5/5h, $35/7d, cadence=fixed}) ===
//
// The admission-level tests above (admission_scopes_integration_test.go) cover
// scope composition but with single 1h windows and the REAL clock, so they
// cannot exercise fixed-cadence resets, multi-window ResetAt, or the
// RetryAfter-then-allow loop THROUGH the admitter. These tests close that gap:
// they inject a fake clock into the budgets engine that the Admitter holds, so
// the whole tier-policy -> admit -> reserve -> settle path runs against a
// deterministic clock.

// amounts use USD internal units.
const usd = int64(1_000_000)

// spendEnv wires an Admitter whose budget engine runs on an injectable fake
// clock, plus the tier-policy store + budget-policy store (for composed scopes)
// and a generously funded balance so only the budget axis gates. Returns the
// admitter, the fake clock, both policy stores, a fresh payer (the cozy-art
// customer) and a cleanup-scoped merchant context.
func spendEnv(t *testing.T) (*admission.Admitter, *budgets.Service, *clockwork.FakeClock, *admission.TierPolicyStore, *admission.BudgetPolicyStore, identity.CustomerID, *pgxpool.Pool, context.Context) {
	t.Helper()
	ctx := context.Background()

	dsn := dbtest.SharedPostgresDSN(t)
	dbi := dbtest.OpenAppDB(t, dsn)
	pool := dbi.Pool()
	dbtest.EnsureTestMerchant(ctx, t, pool)
	ctx = dbtest.WithTestMerchant(ctx)

	payer := identity.CustomerIDFromString(uuid.NewString())
	payerID := payer.UUID()
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.budget_policies WHERE customer_id = $1", payerID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.tier_policies WHERE customer_id = $1", payerID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.budget_inflight_holds WHERE customer_id = $1", payerID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.budget_window_state WHERE customer_id = $1", payerID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.money_settings WHERE customer_id = $1", payerID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.money_transactions WHERE customer_id = $1", payerID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.money_blocks WHERE customer_id = $1", payerID)
	})

	cs := money.NewMoneyService(dbi)
	// Fund the payer well above any test's spend so the money axis never gates;
	// the budget windows are the unit under test.
	_, err := cs.Deposit(ctx, money.DepositParams{CustomerID: &payer, Invoker: payer.UUID().String(), Currency: money.DefaultCurrency, Amount: 1_000_000_000_000, Source: "seed"})
	require.NoError(t, err)

	// Fixed wall clock so window math is deterministic; advance to cross windows.
	clk := clockwork.NewFakeClockAt(time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC))
	bsvc := budgets.NewService(dbi, clk)

	tierStore := admission.NewTierPolicyStore(dbi)
	bpStore := admission.NewBudgetPolicyStore(dbi)
	adm := admission.NewAdmitter(cs, tierStore, bsvc).WithBudgetScopes(bpStore)
	return adm, bsvc, clk, tierStore, bpStore, payer, pool, ctx
}

// cozyTier1Windows is the cozy-art tier_1 shape: $5 per 5h (fixed) AND $35 per
// 7d (fixed), stored as explicit invoker budget-policy grants.
func cozyTier1Windows() []models.BudgetWindowPolicy {
	return []models.BudgetWindowPolicy{
		{Key: "5h", WindowSeconds: 5 * 3600, Limit: 5 * usd, Cadence: budgets.CadenceFixed},
		{Key: "7d", WindowSeconds: 7 * 24 * 3600, Limit: 35 * usd, Cadence: budgets.CadenceFixed},
	}
}

func spendReq(payer identity.CustomerID, invoker, srcID string, amount int64) admission.AdmitRequest {
	return admission.AdmitRequest{
		CustomerID: payer, Currency: money.DefaultCurrency, Invoker: invoker, Tier: "tier_1", Resource: "gpt-4o",
		EstimatedAmount: amount,
		Source:          "gen", SourceID: srcID, ExpiresAt: time.Now().Add(time.Hour),
	}
}

func grantTier1(t *testing.T, ctx context.Context, store *admission.BudgetPolicyStore, payer identity.CustomerID, invokers ...string) {
	t.Helper()
	for _, invoker := range invokers {
		grantInvokerBudget(t, ctx, store, payer, invoker, cozyTier1Windows())
	}
}

// windowByKey finds a budget window's status in a decision by its key, across
// every scope (the cozy-art tier windows live on the invoker scope).
func windowByKey(d admission.AdmitDecision, key string) (budgets.WindowStatus, bool) {
	for _, w := range d.BudgetWindows {
		if w.Key == key {
			return w, true
		}
	}
	return budgets.WindowStatus{}, false
}

// TestSpend_PerInvokerIsolation_5hCap: two delegated users (invokers) under ONE
// payer at tier_1 each get their OWN $5/5h window. Invoker A exhausting its $5
// does NOT block invoker B. This is the core per-invoker abuse-attribution
// guarantee (#491) exercised through admission with the cozy-art tier.
func TestSpend_PerInvokerIsolation_5hCap(t *testing.T) {
	adm, _, _, _, bpStore, payer, _, ctx := spendEnv(t)
	grantTier1(t, ctx, bpStore, payer,
		"du-019759a1-0aa1-7c31-8f01-000000000a01",
		"du-019759a1-0aa1-7c31-8f01-000000000b02",
	)

	// Invoker A spends its full $5 / 5h.
	d, err := adm.Admit(ctx, spendReq(payer, "du-019759a1-0aa1-7c31-8f01-000000000a01", "a1", 5*usd))
	require.NoError(t, err)
	require.True(t, d.Allowed, "A's first $5 fits the $5/5h window")

	// A's next dollar breaches A's own 5h window -> budget deny.
	d, err = adm.Admit(ctx, spendReq(payer, "du-019759a1-0aa1-7c31-8f01-000000000a01", "a2", 1*usd))
	require.NoError(t, err)
	require.False(t, d.Allowed)
	require.Equal(t, "budget", d.BlockedBy)
	w, ok := windowByKey(d, "5h")
	require.True(t, ok)
	require.False(t, w.Allowed, "A's 5h window is the breached window")

	// Invoker B is a DIFFERENT delegated user under the same payer: its $5/5h
	// window is independent and untouched by A's spend.
	d, err = adm.Admit(ctx, spendReq(payer, "du-019759a1-0aa1-7c31-8f01-000000000b02", "b1", 5*usd))
	require.NoError(t, err)
	require.True(t, d.Allowed, "B has its own independent $5/5h window")
}

// TestSpend_FixedCadenceReset_ThroughAdmit: spend invoker A to the $5/5h cap,
// confirm a deny with a RetryAfter, advance the fake clock past the 5h window,
// and confirm admit allows again. Fixed cadence: the window boundary ticks at
// anchor+k*5h (NOT "now"), so the post-reset ResetAt is anchor+2*5h.
func TestSpend_FixedCadenceReset_ThroughAdmit(t *testing.T) {
	adm, _, clk, _, bpStore, payer, _, ctx := spendEnv(t)
	grantTier1(t, ctx, bpStore, payer, "du-019759a1-0aa1-7c31-8f01-000000000a01")
	t0 := clk.Now().UTC()

	// Fill A's $5/5h window at t0 (opens [t0, t0+5h) on the fixed anchor).
	d, err := adm.Admit(ctx, spendReq(payer, "du-019759a1-0aa1-7c31-8f01-000000000a01", "f1", 5*usd))
	require.NoError(t, err)
	require.True(t, d.Allowed)
	w, _ := windowByKey(d, "5h")
	require.Equal(t, t0.Add(5*time.Hour), w.ResetAt, "5h ResetAt is the exact fixed boundary")

	// 2h later: still the same window, exhausted -> deny with a real RetryAfter
	// pointing at the boundary (3h remaining).
	clk.Advance(2 * time.Hour)
	d, err = adm.Admit(ctx, spendReq(payer, "du-019759a1-0aa1-7c31-8f01-000000000a01", "f2", 1*usd))
	require.NoError(t, err)
	require.False(t, d.Allowed)
	require.Equal(t, "budget", d.BlockedBy)
	require.Equal(t, 3*time.Hour, d.RetryAfter, "RetryAfter points at the 5h boundary (3h left)")
	w, _ = windowByKey(d, "5h")
	require.Equal(t, t0.Add(5*time.Hour), w.ResetAt, "ResetAt unchanged by usage within the window")

	// Advance past the 5h boundary: the fixed window resets, admit allows again.
	clk.Advance(3*time.Hour + time.Second) // t0 + 5h + 1s
	d, err = adm.Admit(ctx, spendReq(payer, "du-019759a1-0aa1-7c31-8f01-000000000a01", "f3", 5*usd))
	require.NoError(t, err)
	require.True(t, d.Allowed, "fixed window reset -> full $5 available again")
	w, _ = windowByKey(d, "5h")
	// Fixed cadence: the new period derives from the anchor (t0+5h), so the next
	// boundary is t0+2*5h = t0+10h, NOT now+5h.
	require.Equal(t, t0.Add(10*time.Hour), w.ResetAt,
		"fixed cadence: next boundary is anchor+2*window, not now+window")
}

// TestSpend_MultiWindow_TighterBinds_ThroughAdmit: tier_1 enforces BOTH $5/5h
// AND $35/7d. The 5h window binds first (it's tighter); once the day's spend
// would breach the weekly $35 the 7d window binds instead. Each window's ResetAt
// is reported on the correct boundary.
func TestSpend_MultiWindow_TighterBinds_ThroughAdmit(t *testing.T) {
	adm, _, clk, _, bpStore, payer, _, ctx := spendEnv(t)
	grantTier1(t, ctx, bpStore, payer, "du-019759a1-0aa1-7c31-8f01-000000000a01")
	t0 := clk.Now().UTC()

	// Spend $5 every 5h. After 7 such windows the invoker has spent $35 — the
	// weekly cap — across the week; the 8th $5 (still within 7d) must be denied
	// by the 7d window even though a fresh 5h window has room.
	spent := int64(0)
	for i := 0; i < 7; i++ {
		d, err := adm.Admit(ctx, spendReq(payer, "du-019759a1-0aa1-7c31-8f01-000000000a01", uuidID(), 5*usd))
		require.NoError(t, err, "5h window %d", i)
		require.True(t, d.Allowed, "each $5 fits a fresh 5h window (week spend so far $%d)", spent/usd)
		spent += 5 * usd
		clk.Advance(5 * time.Hour) // roll to the next fixed 5h window
	}
	require.Equal(t, 35*usd, spent)

	// 35h elapsed (< 7d): a fresh 5h window has full room, but the 7d window is
	// now at $35/$35. The next $1 must be denied by the WEEKLY window.
	d, err := adm.Admit(ctx, spendReq(payer, "du-019759a1-0aa1-7c31-8f01-000000000a01", uuidID(), 1*usd))
	require.NoError(t, err)
	require.False(t, d.Allowed)
	require.Equal(t, "budget", d.BlockedBy)

	w5, ok := windowByKey(d, "5h")
	require.True(t, ok)
	require.True(t, w5.Allowed, "the 5h window has room — it is NOT the binding window")
	w7, ok := windowByKey(d, "7d")
	require.True(t, ok)
	require.False(t, w7.Allowed, "the 7d window is the binding (tighter-on-the-week) window")
	require.Equal(t, t0.Add(7*24*time.Hour), w7.ResetAt, "7d ResetAt is the exact weekly boundary")
	require.Greater(t, d.RetryAfter, time.Duration(0), "deny carries a RetryAfter toward the weekly reset")

	// Advance past the weekly boundary: the $35/7d window resets, admit allows.
	clk.Advance((7*24-35)*time.Hour + time.Second) // to just past t0+7d
	d, err = adm.Admit(ctx, spendReq(payer, "du-019759a1-0aa1-7c31-8f01-000000000a01", uuidID(), 5*usd))
	require.NoError(t, err)
	require.True(t, d.Allowed, "weekly window reset -> spend allowed again")
}

// TestSpend_SettleAccounting_CaptureUnderEstimate: Reserve(estimate) ->
// Capture(actual < estimate) reduces the window's used to the ACTUAL, freeing
// the difference back into the same window. Verified end-to-end: a reserve that
// would have exhausted the window, settled below estimate, leaves room for a
// follow-up that the full estimate would have blocked.
func TestSpend_SettleAccounting_CaptureUnderEstimate(t *testing.T) {
	adm, bsvc, _, _, bpStore, payer, _, ctx := spendEnv(t)
	grantTier1(t, ctx, bpStore, payer, "du-019759a1-0aa1-7c31-8f01-000000000a01")

	// Reserve the full $5/5h via an admit (estimate $5).
	d, err := adm.Admit(ctx, spendReq(payer, "du-019759a1-0aa1-7c31-8f01-000000000a01", "s1", 5*usd))
	require.NoError(t, err)
	require.True(t, d.Allowed)
	w, _ := windowByKey(d, "5h")
	require.Equal(t, int64(0), w.Used)
	require.Equal(t, 5*usd, w.Reserved, "the full $5 is reserved")

	// Before settlement: a second dollar is denied (window fully reserved).
	d, err = adm.Admit(ctx, spendReq(payer, "du-019759a1-0aa1-7c31-8f01-000000000a01", "s2", 1*usd))
	require.NoError(t, err)
	require.False(t, d.Allowed, "window fully reserved by the in-flight $5")

	// Settle the in-flight request at the ACTUAL $1 (it used far less than the $5
	// estimate). CaptureByCoords settles every scope for the request by coords.
	require.NoError(t, bsvc.CaptureByCoords(ctx, payer, money.DefaultCurrency, "gen", "s1", 1*usd))

	// Now the window shows used=$1, reserved=$0, remaining=$4.
	chk, _, err := bsvc.Check(ctx, payer, "du-019759a1-0aa1-7c31-8f01-000000000a01", money.DefaultCurrency, tier1Windows(), 0)
	require.NoError(t, err)
	c5 := byKey(chk, "5h")
	require.Equal(t, 1*usd, c5.Used, "captured actual replaces the reservation")
	require.Equal(t, int64(0), c5.Reserved)
	require.Equal(t, 4*usd, c5.Remaining)

	// A follow-up $4 now fits (it would have been denied against the $5 estimate).
	d, err = adm.Admit(ctx, spendReq(payer, "du-019759a1-0aa1-7c31-8f01-000000000a01", "s3", 4*usd))
	require.NoError(t, err)
	require.True(t, d.Allowed, "capture-under-estimate freed the difference back into the window")
}

// TestSpend_SettleAccounting_ReleaseFrees: a Release frees the whole
// reservation back into the window (failed/aborted request path).
func TestSpend_SettleAccounting_ReleaseFrees(t *testing.T) {
	adm, bsvc, _, _, bpStore, payer, _, ctx := spendEnv(t)
	grantTier1(t, ctx, bpStore, payer, "du-019759a1-0aa1-7c31-8f01-000000000a01")

	d, err := adm.Admit(ctx, spendReq(payer, "du-019759a1-0aa1-7c31-8f01-000000000a01", "r1", 5*usd))
	require.NoError(t, err)
	require.True(t, d.Allowed)

	// Release the whole in-flight hold across all scopes.
	require.NoError(t, bsvc.ReleaseByCoords(ctx, payer, money.DefaultCurrency, "gen", "r1"))

	chk, _, err := bsvc.Check(ctx, payer, "du-019759a1-0aa1-7c31-8f01-000000000a01", money.DefaultCurrency, tier1Windows(), 0)
	require.NoError(t, err)
	c5 := byKey(chk, "5h")
	require.Equal(t, int64(0), c5.Used)
	require.Equal(t, int64(0), c5.Reserved)
	require.Equal(t, 5*usd, c5.Remaining, "release frees the full window")

	// The full $5 is admittable again.
	d, err = adm.Admit(ctx, spendReq(payer, "du-019759a1-0aa1-7c31-8f01-000000000a01", "r2", 5*usd))
	require.NoError(t, err)
	require.True(t, d.Allowed)
}

// TestSpend_DenyShape: a budget deny carries BlockedBy="budget", a positive
// RetryAfter, and a populated per-window ResetAt on the breached window.
func TestSpend_DenyShape(t *testing.T) {
	adm, _, clk, _, bpStore, payer, _, ctx := spendEnv(t)
	grantTier1(t, ctx, bpStore, payer, "du-019759a1-0aa1-7c31-8f01-000000000a01")
	t0 := clk.Now().UTC()

	d, err := adm.Admit(ctx, spendReq(payer, "du-019759a1-0aa1-7c31-8f01-000000000a01", "d1", 5*usd))
	require.NoError(t, err)
	require.True(t, d.Allowed)

	clk.Advance(1 * time.Hour)
	d, err = adm.Admit(ctx, spendReq(payer, "du-019759a1-0aa1-7c31-8f01-000000000a01", "d2", 1*usd))
	require.NoError(t, err)
	require.False(t, d.Allowed)
	require.Equal(t, "budget", d.BlockedBy, "deny axis is budget")
	require.Equal(t, 4*time.Hour, d.RetryAfter, "RetryAfter = time to the 5h boundary (4h)")
	w, ok := windowByKey(d, "5h")
	require.True(t, ok)
	require.False(t, w.Allowed)
	require.Equal(t, t0.Add(5*time.Hour), w.ResetAt, "per-window ResetAt populated on the deny")
	require.Equal(t, int64(4*3600), w.RetryAfterSeconds)
}

// --- helpers shared by the settle tests ---

// tier1Windows mirrors cozyTier1Windows as budgets.BudgetWindow for
// direct Check() introspection (the engine API takes BudgetWindow, not policy).
func tier1Windows() []budgets.BudgetWindow {
	return []budgets.BudgetWindow{
		{Key: "5h", WindowSeconds: 5 * 3600, Limit: 5 * usd, Cadence: budgets.CadenceFixed},
		{Key: "7d", WindowSeconds: 7 * 24 * 3600, Limit: 35 * usd, Cadence: budgets.CadenceFixed},
	}
}

func byKey(ws []budgets.WindowStatus, key string) budgets.WindowStatus {
	for _, w := range ws {
		if w.Key == key {
			return w
		}
	}
	return budgets.WindowStatus{}
}

func uuidID() string { return uuid.NewString() }
