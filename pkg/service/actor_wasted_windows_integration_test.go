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
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.actor_wasted_windows WHERE customer_id = $1", payerID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.actor_wasted_windows WHERE customer_id IS NULL")
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.money_account_settings WHERE customer_id = $1", payerID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.money_transactions WHERE customer_id = $1", payerID)
	})
	ms := money.NewMoneyService(dbi)
	_, err = ms.Deposit(ctx, money.DepositParams{CustomerID: &payer, Actor: payer.UUID().String(), Amount: 100_000_000, Source: "seed"})
	require.NoError(t, err)
	return svc, ms, payer, ctx
}

// #492: with a tenant-wide actor-window config SMALLER than the hardcoded default
// ($1/15m vs $5/15m), an actor that exceeds the CONFIGURED window is denied at
// admit — proving the operator-configured window (not DefaultActorWastedWindows)
// is the enforced budget.
func TestActorWastedWindows_ConfiguredDenies(t *testing.T) {
	svc, _, payer, ctx := wastedSvcEnv(t)

	// Configure the tenant-wide default: $1 / 15 min (tighter than the $5 default).
	require.NoError(t, svc.SetActorWastedWindows(ctx, identity.CustomerID{}, []abuse.WastedWindow{
		{Key: "burst", Window: 15 * time.Minute, LimitMicros: 1_000_000},
	}))

	// $2 wasted by the actor — over the configured $1 window, UNDER the $5 default.
	require.NoError(t, svc.ReportWastedSpend(ctx, payer, "user:configured", 2_000_000, "test"))

	res, err := svc.Admit(ctx, billingservice.AdmitInput{
		CustomerID: payer, Actor: "user:configured", Tier: "free", Resource: "r",
		Amounts: map[string]int64{"request": 1}, EstimateMicros: 100,
		Source: "usage", SourceID: "blocked-configured",
	})
	require.NoError(t, err)
	require.False(t, res.Allowed, "actor over the CONFIGURED $1 window must be denied")
	require.Equal(t, "abuse", res.BlockedBy)
}

// #492: with NO actor-window config stored, the resolver falls back to
// DefaultActorWastedWindows ($5/15m). $2 wasted is UNDER $5 -> still allowed,
// proving the default is the fallback and the $1 config above was load-bearing.
func TestActorWastedWindows_UnsetFallsBackToDefault(t *testing.T) {
	svc, _, payer, ctx := wastedSvcEnv(t)

	// No SetActorWastedWindows call -> hardcoded $5/15m default applies.
	require.NoError(t, svc.ReportWastedSpend(ctx, payer, "user:default", 2_000_000, "test"))

	res, err := svc.Admit(ctx, billingservice.AdmitInput{
		CustomerID: payer, Actor: "user:default", Tier: "free", Resource: "r",
		Amounts: map[string]int64{"request": 1}, EstimateMicros: 100,
		Source: "usage", SourceID: "ok-default",
	})
	require.NoError(t, err)
	require.True(t, res.Allowed, "$2 wasted is under the $5 default backstop -> allowed")
}

// #492: the per-customer override takes precedence over the tenant-wide default
// for that customer; resolution mirrors tier_schedules' "= $2 OR IS NULL".
func TestActorWastedWindows_CustomerOverride(t *testing.T) {
	svc, _, payer, ctx := wastedSvcEnv(t)

	// Tenant default: a generous $50 window. Customer override: a tight $1 window.
	require.NoError(t, svc.SetActorWastedWindows(ctx, identity.CustomerID{}, []abuse.WastedWindow{
		{Key: "burst", Window: 15 * time.Minute, LimitMicros: 50_000_000},
	}))
	require.NoError(t, svc.SetActorWastedWindows(ctx, payer, []abuse.WastedWindow{
		{Key: "burst", Window: 15 * time.Minute, LimitMicros: 1_000_000},
	}))

	// $2 wasted -> over the customer's $1 override (even though under the $50 default).
	require.NoError(t, svc.ReportWastedSpend(ctx, payer, "user:override", 2_000_000, "test"))

	res, err := svc.Admit(ctx, billingservice.AdmitInput{
		CustomerID: payer, Actor: "user:override", Tier: "free", Resource: "r",
		Amounts: map[string]int64{"request": 1}, EstimateMicros: 100,
		Source: "usage", SourceID: "blocked-override",
	})
	require.NoError(t, err)
	require.False(t, res.Allowed, "actor over the per-customer $1 override must be denied")
}
