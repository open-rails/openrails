//go:build integration

package service_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
	tcredis "github.com/testcontainers/testcontainers-go/modules/redis"

	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/integrations/fx"
	"github.com/open-rails/openrails/internal/modules/entitlements"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/pkg/identity"
	billingservice "github.com/open-rails/openrails/pkg/service"
)

// captureFallbackEnv is wastedSvcEnv plus the raw Redis client, so tests can
// simulate a flush/failover between admit and capture (#676).
func captureFallbackEnv(t *testing.T) (*billingservice.Service, *money.MoneyService, *redis.Client, identity.CustomerID, context.Context) {
	t.Helper()
	ctx := context.Background()
	dsn := dbtest.SharedPostgresDSN(t)
	dbi := dbtest.OpenAppDB(t, dsn)
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
		FXProvider:         fx.NewMockProvider(nil),
		Clock:              clockwork.NewRealClock(),
	}
	svc, err := billingservice.New(rt)
	require.NoError(t, err)

	payer := identity.CustomerIDFromString(uuid.NewString())
	payerID := payer.UUID()
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.money_settings WHERE customer_id = $1", payerID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.usage_events WHERE customer_id = $1", payerID)
	})
	ms := money.NewMoneyService(dbi)
	_, err = ms.Deposit(ctx, money.DepositParams{CustomerID: &payer, Invoker: payer.UUID().String(), Currency: money.DefaultCurrency, Amount: 100_000, Source: "seed"})
	require.NoError(t, err)
	return svc, ms, rdb, payer, ctx
}

// #676: admit → Redis flush → capture still lands via caller-supplied payer
// coordinates (the durable fallback path).
func TestCaptureHold_RedisFlushFallback(t *testing.T) {
	svc, ms, rdb, payer, ctx := captureFallbackEnv(t)
	reqID := uuid.NewString()

	res, err := svc.Admit(ctx, billingservice.AdmitInput{
		CustomerID:      payer,
		Invoker:         "user:z",
		InvokerType:     "payer",
		Currency:        money.DefaultCurrency,
		EstimatedAmount: 500,
		Source:          "usage",
		SourceID:        reqID,
	})
	require.NoError(t, err)
	require.True(t, res.Allowed)

	// Redis loses everything between admit and capture.
	require.NoError(t, rdb.FlushDB(ctx).Err())

	// Without fallback coordinates the pointer miss is still a hard error.
	_, err = svc.CaptureHold(ctx, billingservice.CaptureHoldRequest{RequestID: reqID, Amount: 400})
	require.ErrorContains(t, err, "hold not found")

	// With fallback coordinates the rendered service is chargeable.
	trx, err := svc.CaptureHold(ctx, billingservice.CaptureHoldRequest{
		RequestID:   reqID,
		Amount:      400,
		CustomerID:  payer.UUID().String(),
		Currency:    money.DefaultCurrency,
		Invoker:     "user:z",
		AdmitSource: "usage",
	})
	require.NoError(t, err, "capture must land after a Redis flush when coords are supplied")
	require.NotNil(t, trx)

	bal, err := ms.GetBalanceForCustomer(ctx, payer, money.DefaultCurrency)
	require.NoError(t, err)
	require.Equal(t, int64(100_000-400), bal.Balance)
}

// #676: a fallback capture retry after a pointer-resolved capture dedupes on the
// same durable coordinates (no double charge).
func TestCaptureHold_FallbackRetryIsIdempotent(t *testing.T) {
	svc, ms, rdb, payer, ctx := captureFallbackEnv(t)
	reqID := uuid.NewString()

	res, err := svc.Admit(ctx, billingservice.AdmitInput{
		CustomerID:      payer,
		Invoker:         "user:z",
		InvokerType:     "payer",
		Currency:        money.DefaultCurrency,
		EstimatedAmount: 500,
		Source:          "usage",
		SourceID:        reqID,
	})
	require.NoError(t, err)
	require.True(t, res.Allowed)

	// First capture resolves the live pointer.
	trx1, err := svc.CaptureHold(ctx, billingservice.CaptureHoldRequest{RequestID: reqID, Amount: 400})
	require.NoError(t, err)

	// Redis flush, then an at-least-once retry via the fallback path.
	require.NoError(t, rdb.FlushDB(ctx).Err())
	trx2, err := svc.CaptureHold(ctx, billingservice.CaptureHoldRequest{
		RequestID:   reqID,
		Amount:      400,
		CustomerID:  payer.UUID().String(),
		Currency:    money.DefaultCurrency,
		Invoker:     "user:z",
		AdmitSource: "usage",
	})
	require.NoError(t, err)
	require.Equal(t, trx1.ID, trx2.ID, "retry must dedupe on the durable spend coordinates")

	bal, err := ms.GetBalanceForCustomer(ctx, payer, money.DefaultCurrency)
	require.NoError(t, err)
	require.Equal(t, int64(100_000-400), bal.Balance, "no double debit")
}
