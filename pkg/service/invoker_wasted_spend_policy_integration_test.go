//go:build integration

package service_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jonboulle/clockwork"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
	tcredis "github.com/testcontainers/testcontainers-go/modules/redis"

	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/modules/abuse"
	"github.com/open-rails/openrails/internal/modules/entitlements"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/pkg/identity"
	billingservice "github.com/open-rails/openrails/pkg/service"
)

// wastedSvcEnv builds a Service wired with Redis (so Admit + ReportWastedSpend
// run end-to-end) and a freshly seeded payer.
func wastedSvcEnv(t *testing.T) (*billingservice.Service, *money.MoneyService, identity.CustomerID, context.Context) {
	t.Helper()
	ctx := context.Background()
	dsn := dbtest.SharedPostgresDSN(t)
	dbi := dbtest.OpenAppDB(t, dsn)
	pool := dbi.Pool()
	dbtest.EnsureTestTenant(ctx, t, pool)
	ctx = dbtest.WithTestTenant(ctx)

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

	rt := &app.Runtime{
		DB:                 dbi,
		RedisClient:        rdb,
		MoneyService:       money.NewMoneyService(dbi),
		EntitlementService: entitlements.NewEntitlementService(dbi),
		Clock:              clockwork.NewRealClock(),
	}
	svc, err := billingservice.New(rt)
	require.NoError(t, err)

	payer := identity.CustomerIDFromString(uuid.NewString())
	payerID := payer.UUID()
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.invoker_wasted_spend_policies")
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.money_account_settings WHERE customer_id = $1", payerID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.money_transactions WHERE customer_id = $1", payerID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.usage_events WHERE customer_id = $1", payerID)
	})
	ms := money.NewMoneyService(dbi)
	_, err = ms.Deposit(ctx, money.DepositParams{CustomerID: &payer, Invoker: payer.UUID().String(), Amount: 100_000_000, Source: "seed"})
	require.NoError(t, err)
	return svc, ms, payer, ctx
}

// #496: with a merchant-wide invoker policy SMALLER than the hardcoded default
// ($1/15m vs $5/15m), an invoker that exceeds the CONFIGURED window is denied at
// admit — proving the operator-configured window (not DefaultInvokerWastedWindows)
// is the enforced budget.
func TestInvokerWastedSpendPolicy_ConfiguredDenies(t *testing.T) {
	svc, _, payer, ctx := wastedSvcEnv(t)

	// Configure the tenant-wide default: $1 / 15 min (tighter than the $5 default).
	require.NoError(t, svc.SetInvokerWastedSpendPolicy(ctx, []abuse.WastedWindow{
		{Key: "burst", Window: 15 * time.Minute, Limit: 1_000_000},
	}))

	// $2 wasted by the invoker — over the configured $1 window, UNDER the $5 default.
	_, err := svc.ReportWastedSpend(ctx, billingservice.WastedSpendInput{
		CustomerID: payer, Invoker: "user:configured", Amount: 2_000_000,
		Source: "test", SourceID: "configured", Reason: "test",
	})
	require.NoError(t, err)

	res, err := svc.Admit(ctx, billingservice.AdmitInput{
		CustomerID: payer, Invoker: "user:configured", Tier: "free", Resource: "r",
		Amounts: map[string]int64{"request": 1}, EstimatedAmount: 100,
		Source: "usage", SourceID: "blocked-configured",
	})
	require.NoError(t, err)
	require.False(t, res.Allowed, "invoker over the CONFIGURED $1 window must be denied")
	require.Equal(t, "abuse", res.BlockedBy)
}

// #496: with NO invoker policy stored, the resolver falls back to
// DefaultInvokerWastedWindows ($5/15m). $2 wasted is UNDER $5 -> still allowed,
// proving the default is the fallback and the $1 config above was load-bearing.
func TestInvokerWastedSpendPolicy_UnsetFallsBackToDefault(t *testing.T) {
	svc, _, payer, ctx := wastedSvcEnv(t)

	// No SetInvokerWastedSpendPolicy call -> hardcoded $5/15m default applies.
	_, err := svc.ReportWastedSpend(ctx, billingservice.WastedSpendInput{
		CustomerID: payer, Invoker: "user:default", Amount: 2_000_000,
		Source: "test", SourceID: "default", Reason: "test",
	})
	require.NoError(t, err)

	res, err := svc.Admit(ctx, billingservice.AdmitInput{
		CustomerID: payer, Invoker: "user:default", Tier: "free", Resource: "r",
		Amounts: map[string]int64{"request": 1}, EstimatedAmount: 100,
		Source: "usage", SourceID: "ok-default",
	})
	require.NoError(t, err)
	require.True(t, res.Allowed, "$2 wasted is under the $5 default backstop -> allowed")
}

// #496: the invoker wasted-spend policy is merchant-wide, while Redis usage for
// a delegated-user invoker is namespaced by payer to avoid cross-platform
// collisions for common delegated IDs like "user:123".
func TestInvokerWastedSpendPolicy_MerchantWidePolicyPayerScopedUsage(t *testing.T) {
	svc, ms, payerA, ctx := wastedSvcEnv(t)
	payerB := identity.CustomerIDFromString(uuid.NewString())
	payerBID := payerB.UUID()
	pool := dbtest.OpenAppDB(t, dbtest.SharedPostgresDSN(t)).Pool()
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.money_account_settings WHERE customer_id = $1", payerBID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.money_transactions WHERE customer_id = $1", payerBID)
	})
	_, err := ms.Deposit(ctx, money.DepositParams{CustomerID: &payerB, Invoker: payerB.UUID().String(), Amount: 100_000_000, Source: "seed"})
	require.NoError(t, err)

	require.NoError(t, svc.SetInvokerWastedSpendPolicy(ctx, []abuse.WastedWindow{
		{Key: "burst", Window: 15 * time.Minute, Limit: 1_000_000},
	}))

	const invoker = "user:same-delegated-id"
	_, err = svc.ReportWastedSpend(ctx, billingservice.WastedSpendInput{
		CustomerID: payerA, Invoker: invoker, Amount: 2_000_000,
		Source: "test", SourceID: "payer-a", Reason: "test",
	})
	require.NoError(t, err)

	resA, err := svc.Admit(ctx, billingservice.AdmitInput{
		CustomerID: payerA, Invoker: invoker, Tier: "free", Resource: "r",
		Amounts: map[string]int64{"request": 1}, EstimatedAmount: 100,
		Source: "usage", SourceID: "blocked-payer-a",
	})
	require.NoError(t, err)
	require.False(t, resA.Allowed, "payer A's invoker is over the merchant-wide $1 policy")

	resB, err := svc.Admit(ctx, billingservice.AdmitInput{
		CustomerID: payerB, Invoker: invoker, Tier: "free", Resource: "r",
		Amounts: map[string]int64{"request": 1}, EstimatedAmount: 100,
		Source: "usage", SourceID: "allowed-payer-b",
	})
	require.NoError(t, err)
	require.True(t, resB.Allowed, "same invoker label under payer B must not inherit payer A's Redis usage")

	_, err = svc.ReportWastedSpend(ctx, billingservice.WastedSpendInput{
		CustomerID: payerB, Invoker: invoker, Amount: 2_000_000,
		Source: "test", SourceID: "payer-b", Reason: "test",
	})
	require.NoError(t, err)
	resB, err = svc.Admit(ctx, billingservice.AdmitInput{
		CustomerID: payerB, Invoker: invoker, Tier: "free", Resource: "r",
		Amounts: map[string]int64{"request": 1}, EstimatedAmount: 100,
		Source: "usage", SourceID: "blocked-payer-b",
	})
	require.NoError(t, err)
	require.False(t, resB.Allowed, "the same merchant-wide policy still applies to payer B's invoker")
}

func TestWastedSpendDirectPayer_GraceThenChargeIdempotently(t *testing.T) {
	svc, _, payer, ctx := wastedSvcEnv(t)
	pool := dbtest.OpenAppDB(t, dbtest.SharedPostgresDSN(t)).Pool()
	require.NoError(t, svc.SetTierPolicy(ctx, identity.CustomerID{}, billingservice.TierPolicyInput{
		Tier: "free",
		BadSpendWindows: []billingservice.TierBudgetWindowInput{
			{Key: "burst", WindowSeconds: int64((15 * time.Minute) / time.Second), Limit: 1_000_000},
		},
	}))

	res, err := svc.ReportWastedSpend(ctx, billingservice.WastedSpendInput{
		CustomerID: payer, Invoker: payer.UUID().String(), InvokerType: string(identity.InvokerTypePayer),
		Amount: 500_000, Source: "waste", SourceID: "under-grace", Reason: "test",
	})
	require.NoError(t, err)
	require.Equal(t, money.DefaultCurrency, res.Currency)
	require.Equal(t, int64(500_000), res.ForgivenAmount)
	require.Zero(t, res.ChargedAmount)
	requireUsageAmount(t, pool, ctx, payer, "under-grace", 0)

	res, err = svc.ReportWastedSpend(ctx, billingservice.WastedSpendInput{
		CustomerID: payer, Invoker: payer.UUID().String(), InvokerType: string(identity.InvokerTypePayer),
		Amount: 1_500_000, Source: "waste", SourceID: "over-grace", Reason: "test",
	})
	require.NoError(t, err)
	require.Equal(t, money.DefaultCurrency, res.Currency)
	require.Equal(t, int64(500_000), res.ForgivenAmount)
	require.Equal(t, int64(1_000_000), res.ChargedAmount)
	requireUsageAmount(t, pool, ctx, payer, "over-grace", 1_000_000)

	res, err = svc.ReportWastedSpend(ctx, billingservice.WastedSpendInput{
		CustomerID: payer, Invoker: payer.UUID().String(), InvokerType: string(identity.InvokerTypePayer),
		Amount: 1_500_000, Source: "waste", SourceID: "over-grace", Reason: "retry",
	})
	require.NoError(t, err)
	require.Equal(t, money.DefaultCurrency, res.Currency)
	require.True(t, res.Duplicate)
	requireUsageAmount(t, pool, ctx, payer, "over-grace", 1_000_000)

	res, err = svc.ReportWastedSpend(ctx, billingservice.WastedSpendInput{
		CustomerID: payer, Invoker: payer.UUID().String(), InvokerType: string(identity.InvokerTypePayer),
		Currency: "EUR",
		Amount:   1, Source: "waste", SourceID: "over-grace", Reason: "same source id, different currency",
	})
	require.NoError(t, err)
	require.False(t, res.Duplicate)
	require.Equal(t, "EUR", res.Currency)
	require.Equal(t, int64(1), res.ForgivenAmount)

	payerWindows, _, err := svc.AbuseUsage(ctx, payer, "", "EUR", "")
	require.NoError(t, err)
	require.NotEmpty(t, payerWindows)
	require.Equal(t, "EUR", payerWindows[0].Currency)
}

func TestWastedSpendDirectPayer_DoesNotHitDelegatedInvokerCutoff(t *testing.T) {
	svc, _, payer, ctx := wastedSvcEnv(t)
	require.NoError(t, svc.SetTierPolicy(ctx, identity.CustomerID{}, billingservice.TierPolicyInput{
		Tier: "free",
		BadSpendWindows: []billingservice.TierBudgetWindowInput{
			{Key: "burst", WindowSeconds: int64((15 * time.Minute) / time.Second), Limit: 10_000_000},
		},
	}))
	require.NoError(t, svc.SetInvokerWastedSpendPolicy(ctx, []abuse.WastedWindow{
		{Key: "burst", Window: 15 * time.Minute, Limit: 1_000_000},
	}))

	_, err := svc.ReportWastedSpend(ctx, billingservice.WastedSpendInput{
		CustomerID: payer, Invoker: "service-token:payer-owned", InvokerType: string(identity.InvokerTypePayer),
		Amount: 2_000_000, Source: "waste", SourceID: "direct-over-flat", Reason: "test",
	})
	require.NoError(t, err)

	res, err := svc.Admit(ctx, billingservice.AdmitInput{
		CustomerID: payer, Invoker: "service-token:payer-owned", InvokerType: string(identity.InvokerTypePayer),
		Tier: "free", Resource: "r",
		Amounts: map[string]int64{"request": 1}, EstimatedAmount: 100,
		Source: "usage", SourceID: "direct-admit",
	})
	require.NoError(t, err)
	require.True(t, res.Allowed, "direct payer credentials are charged after grace, not cut off by delegated invoker policy")
}

func requireUsageAmount(t *testing.T, pool interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, ctx context.Context, payer identity.CustomerID, sourceID string, want int64) {
	t.Helper()
	var got int64
	err := pool.QueryRow(ctx, `
		SELECT COALESCE(SUM(amount), 0)::bigint
		  FROM openrails.usage_events
		 WHERE customer_id = $1
		   AND event_type = 'wasted_spend'
		   AND source = 'waste'
		   AND source_id = $2
	`, payer.UUID(), sourceID).Scan(&got)
	require.NoError(t, err)
	require.Equal(t, want, got)
}
