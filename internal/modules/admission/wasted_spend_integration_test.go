//go:build integration

package admission_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/modules/abuse"
	"github.com/open-rails/openrails/internal/modules/admission"
	"github.com/open-rails/openrails/internal/modules/budgets"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/internal/modules/ratelimit"
	"github.com/open-rails/openrails/pkg/identity"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
	tcredis "github.com/testcontainers/testcontainers-go/modules/redis"
)

// wastedEnv builds an Admitter wired with the wasted-spend guard + the guard +
// the limiter (so the test can both report and admit against the same Redis).
func wastedEnv(t *testing.T) (*admission.Admitter, *abuse.WastedSpendGuard, *ratelimit.Limiter, *money.MoneyService, *admission.TierPolicyStore, identity.CustomerID, context.Context) {
	t.Helper()
	ctx := context.Background()

	dsn := dbtest.SharedPostgresDSN(t)
	dbi := dbtest.OpenAppDB(t, dsn)
	pool := dbi.Pool()
	dbtest.EnsureTestTenant(ctx, t, pool)
	ctx = dbtest.WithTestTenant(ctx)

	payer := identity.CustomerIDFromString(uuid.NewString())
	payerID := payer.UUID()
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.tier_policies WHERE customer_id = $1", payerID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.money_settings WHERE customer_id = $1", payerID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.money_transactions WHERE customer_id = $1", payerID)
	})

	rc, err := tcredis.Run(ctx, "redis:7-alpine")
	require.NoError(t, err)
	t.Cleanup(func() { _ = rc.Terminate(ctx) })
	conn, err := rc.ConnectionString(ctx)
	require.NoError(t, err)
	opt, err := redis.ParseURL(conn)
	require.NoError(t, err)
	rdb := redis.NewClient(opt)
	t.Cleanup(func() { _ = rdb.Close() })
	require.NoError(t, rdb.Ping(ctx).Err())

	lim := ratelimit.NewLimiter(rdb)
	guard := abuse.NewWastedSpendGuard(lim)
	cs := money.NewMoneyService(dbi)
	store := admission.NewTierPolicyStore(dbi)
	bl := abuse.NewBlocklistService(dbi)
	bsvc := budgets.NewService(dbi)
	actorFlat := []abuse.WastedWindow{{Key: "burst", Window: 15 * time.Minute, Limit: 5_000}}
	adm := admission.NewAdmitter(lim, cs, store, bl, bsvc).WithWastedSpend(guard, actorFlat)
	return adm, guard, lim, cs, store, payer, ctx
}

// #497: PAYER bad_spend is direct-payer grace for report-time charging, not an
// admit-time cutoff.
func TestAdmit_WastedSpend_PayerOverBudgetDoesNotDeny(t *testing.T) {
	adm, guard, _, cs, store, payer, ctx := wastedEnv(t)
	require.NoError(t, store.UpsertTierPolicyFull(ctx, payer, "free", models.ThroughputPolicy{
		Windows:         []models.ThroughputWindow{{Unit: "request", WindowSeconds: 60, Max: 100}},
		BadSpendWindows: []models.BudgetWindowPolicy{{Key: "burst", WindowSeconds: 900, Limit: 1_000}},
	}))
	_, err := cs.Deposit(ctx, money.DepositParams{CustomerID: &payer, Invoker: payer.UUID().String(), Amount: 1_000_000, Source: "seed"})
	require.NoError(t, err)

	// Under budget: allowed.
	d, err := adm.Admit(ctx, admission.AdmitRequest{
		CustomerID: payer, Invoker: "user:a", Tier: "free", Resource: "r",
		Amounts: map[string]int64{"request": 1}, EstimatedAmount: 100,
		Source: "usage", SourceID: "ok1", ExpiresAt: time.Now().Add(time.Hour),
	})
	require.NoError(t, err)
	require.True(t, d.Allowed)

	// Report wasted $ that meets/exceeds the payer's per-tier bad_spend budget.
	tenant := dbtest.TestTenantID.UUID().String()
	require.NoError(t, guard.Record(ctx, tenant, payer.UUID().String(), "user:a", money.DefaultCurrency, 1_000,
		[]abuse.WastedWindow{{Key: "burst", Window: 15 * time.Minute, Limit: 1_000}},
		[]abuse.WastedWindow{{Key: "burst", Window: 15 * time.Minute, Limit: 5_000}}))

	// Payer over grace is not an admit deny; direct-payer waste is charged at
	// report time instead.
	d, err = adm.Admit(ctx, admission.AdmitRequest{
		CustomerID: payer, Invoker: "user:a", Tier: "free", Resource: "r",
		Amounts: map[string]int64{"request": 1}, EstimatedAmount: 100,
		Source: "usage", SourceID: "allowed-after-payer-waste", ExpiresAt: time.Now().Add(time.Hour),
	})
	require.NoError(t, err)
	require.True(t, d.Allowed)
	require.NotNil(t, d.Hold)
}

// #488: INVOKER over the flat per-invoker wasted budget -> admit denies
// failure_rate_limited (independent of the payer budget).
func TestAdmit_WastedSpend_InvokerOverBudget(t *testing.T) {
	adm, guard, _, cs, store, payer, ctx := wastedEnv(t)
	// No payer bad_spend windows -> only the invoker flat budget (5_000) applies.
	require.NoError(t, store.UpsertTierPolicy(ctx, payer, "free",
		[]models.ThroughputWindow{{Unit: "request", WindowSeconds: 60, Max: 100}}))
	_, err := cs.Deposit(ctx, money.DepositParams{CustomerID: &payer, Invoker: payer.UUID().String(), Amount: 1_000_000, Source: "seed"})
	require.NoError(t, err)

	tenant := dbtest.TestTenantID.UUID().String()
	// Report 5_000 wasted to the invoker's flat burst window (limit 5_000) -> over.
	require.NoError(t, guard.Record(ctx, tenant, payer.UUID().String(), "user:b", money.DefaultCurrency, 5_000,
		nil,
		[]abuse.WastedWindow{{Key: "burst", Window: 15 * time.Minute, Limit: 5_000}}))

	d, err := adm.Admit(ctx, admission.AdmitRequest{
		CustomerID: payer, Invoker: "user:b", Tier: "free", Resource: "r",
		Amounts: map[string]int64{"request": 1}, EstimatedAmount: 100,
		Source: "usage", SourceID: "blocked-invoker", ExpiresAt: time.Now().Add(time.Hour),
	})
	require.NoError(t, err)
	require.False(t, d.Allowed)
	require.Equal(t, "abuse", d.BlockedBy)
	require.Equal(t, admission.DenyFailureRateLimited, d.DenyCode)

	// A DIFFERENT invoker under the same payer is unaffected (per-invoker budget).
	d, err = adm.Admit(ctx, admission.AdmitRequest{
		CustomerID: payer, Invoker: "user:c", Tier: "free", Resource: "r",
		Amounts: map[string]int64{"request": 1}, EstimatedAmount: 100,
		Source: "usage", SourceID: "ok-invoker", ExpiresAt: time.Now().Add(time.Hour),
	})
	require.NoError(t, err)
	require.True(t, d.Allowed)
}
