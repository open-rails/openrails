//go:build integration

package service_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
	tcredis "github.com/testcontainers/testcontainers-go/modules/redis"

	"github.com/open-rails/openrails/internal/app"
	identity "github.com/open-rails/openrails/internal/billingidentity"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/integrations/fx"
	"github.com/open-rails/openrails/internal/modules/entitlements"
	"github.com/open-rails/openrails/internal/modules/money"
	billingservice "github.com/open-rails/openrails/internal/service"
)

// requireBoundBadSpendPolicy declares a billing policy carrying only the #497
// wasted-spend grace window and binds it to one tier (or#897). Wasted-spend
// grace is orthogonal to the capped quantity, so it rides on the neutral
// outstanding_cap kind with no declared amount — the payer's own arrears limit
// (none here) still governs.
func requireBoundBadSpendPolicy(t *testing.T, svc *billingservice.Service, ctx context.Context, tier string, limit int64) {
	t.Helper()
	name := "grace_" + tier
	require.NoError(t, svc.SetBillingPolicy(ctx, billingservice.BillingPolicyInput{
		Name: name,
		Kind: "outstanding_cap",
		BadSpendWindows: []billingservice.SpendLimitWindowInput{
			{Key: "burst", WindowSeconds: int64((15 * time.Minute) / time.Second), Limit: limit},
		},
	}))
	require.NoError(t, svc.BindBillingPolicy(ctx, billingservice.BillingPolicyBindingInput{
		PolicyName: name, Tier: tier,
	}))
}

// wastedSvcEnv builds a Service wired with Redis (so Admit + ReportWastedSpend
// run end-to-end) and a freshly seeded payer.
func wastedSvcEnv(t *testing.T) (*billingservice.Service, *money.MoneyService, identity.CustomerID, context.Context) {
	t.Helper()
	svc, ms, payer, ctx, _ := wastedSvcEnvWithRedis(t)
	return svc, ms, payer, ctx
}

// wastedSvcEnvWithRedis is the same environment, handing back the Redis client
// so a test can FLUSH it mid-run — the only way to prove a claim is durable
// rather than merely cached (or#903).
func wastedSvcEnvWithRedis(t *testing.T) (*billingservice.Service, *money.MoneyService, identity.CustomerID, context.Context, *redis.Client) {
	t.Helper()
	ctx := context.Background()
	dbi := dbtest.OpenMerchantDB(t, dbtest.TestMerchantID.UUID())
	pool := dbi.Pool()
	dbtest.EnsureTestMerchant(ctx, t, pool)
	ctx = dbtest.WithTestMerchant(ctx)

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
		FXProvider:         fx.NewMockProvider(map[string]float64{"eur": 2}),
		Clock:              clockwork.NewRealClock(),
	}
	svc, err := billingservice.New(rt)
	require.NoError(t, err)

	payer := identity.CustomerIDFromString(uuid.NewString())
	payerID := payer.UUID()
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM billing.merchant_configurations")
		_, _ = pool.Exec(ctx, "DELETE FROM billing.invoker_spend_limits WHERE customer_id = $1", payerID)
		_, _ = pool.Exec(ctx, "DELETE FROM billing.billing_policy_bindings WHERE customer_id IS NULL OR customer_id = $1", payerID)
		_, _ = pool.Exec(ctx, "DELETE FROM billing.billing_policies")
		_, _ = pool.Exec(ctx, "DELETE FROM billing.money_settings WHERE customer_id = $1", payerID)
		_, _ = pool.Exec(ctx, "DELETE FROM billing.usage_events WHERE customer_id = $1", payerID)
	})
	ms := money.NewMoneyService(dbi)
	_, err = ms.Deposit(ctx, money.DepositParams{CustomerID: &payer, Invoker: payer.UUID().String(), Currency: money.DefaultCurrency, Amount: 100_000_000, Source: "seed"})
	require.NoError(t, err)
	return svc, ms, payer, ctx, rdb
}

func grantDelegatedSpend(t *testing.T, svc *billingservice.Service, ctx context.Context, payer identity.CustomerID, invoker string) {
	t.Helper()
	require.NoError(t, svc.SetInvokerSpendLimits(ctx, payer, billingservice.InvokerSpendLimitInput{
		Scope:    "invoker",
		ScopeKey: invoker,
		Windows: []billingservice.SpendLimitWindowInput{
			{Key: "delegated", WindowSeconds: 3600, Limit: 100_000_000},
		},
	}))
}
