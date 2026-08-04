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

// or#891 at the SDK boundary. The money-module proofs live in
// internal/modules/money/idempotency_key_integration_test.go; these drive the
// same contract through the embedded service facade a host actually calls,
// because that is the seam a host retries against.

func idemEnv(t *testing.T, seed int64) (*billingservice.Service, *money.MoneyService, *redis.Client, identity.CustomerID, context.Context) {
	t.Helper()
	ctx := context.Background()
	dsn := dbtest.SharedPostgresDSN(t)
	dbi := dbtest.OpenAppDB(t, dsn)
	pool := dbi.Pool()
	dbtest.EnsureTestMerchant(ctx, t, pool)
	ctx = dbtest.WithTestMerchant(ctx)

	// A DEDICATED Redis per test: these proofs FLUSH it to drive the capture
	// through its durable fallback coordinates.
	rc, err := tcredis.Run(ctx, "redis:7-alpine", dbtest.WithRedisLimits())
	require.NoError(t, err)
	t.Cleanup(func() { _ = rc.Terminate(context.Background()) })
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
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.usage_events WHERE customer_id = $1", payerID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.money_settings WHERE customer_id = $1", payerID)
	})
	ms := money.NewMoneyService(dbi)
	if seed > 0 {
		_, err = ms.Deposit(ctx, money.DepositParams{
			CustomerID: &payer, Invoker: payerID.String(), Currency: money.DefaultCurrency,
			Amount: seed, Source: "seed", SourceID: idemStrPtr("seed-" + payerID.String()),
		})
		require.NoError(t, err)
	}
	return svc, ms, rdb, payer, ctx
}

func idemStrPtr(s string) *string { return &s }

func idemBalance(t *testing.T, ms *money.MoneyService, ctx context.Context, payer identity.CustomerID) int64 {
	t.Helper()
	bal, err := ms.GetBalanceForCustomer(ctx, payer, money.DefaultCurrency)
	require.NoError(t, err)
	return bal.Balance
}

// Every money-moving entry point on the facade refuses a blank key. A money
// write without an idempotency key is an error, not a silent non-guarantee.
func TestOr891_ServiceRefusesMoneyWritesWithoutAKey(t *testing.T) {
	svc, ms, _, payer, ctx := idemEnv(t, 10_000_000)
	pid := payer.UUID()

	_, err := svc.DepositCredits(ctx, billingservice.DepositCreditsRequest{
		CustomerID: &payer, Invoker: pid.String(), Currency: money.DefaultCurrency,
		Amount: 1_000, Source: "admin",
	})
	require.ErrorContains(t, err, "source_id required")

	_, err = svc.WithdrawCredits(ctx, billingservice.WithdrawCreditsRequest{
		CustomerID: &payer, Invoker: pid.String(), Currency: money.DefaultCurrency,
		Amount: 1_000, Source: "admin",
	})
	require.ErrorContains(t, err, "source_id required")

	err = svc.RecordUsage(ctx, billingservice.RecordUsageInput{
		CustomerID: payer, Currency: money.DefaultCurrency, EventType: "invoke",
		Amount: 1_000, Source: "invoke",
	})
	require.ErrorContains(t, err, "source_id")

	_, err = svc.ReportWastedSpend(ctx, billingservice.WastedSpendInput{
		CustomerID: payer, Invoker: pid.String(), InvokerType: "payer",
		Currency: money.DefaultCurrency, Amount: 1_000, Source: "invoke",
	})
	require.ErrorContains(t, err, "source_id required")

	require.Equal(t, int64(10_000_000), idemBalance(t, ms, ctx, payer),
		"no refused write may move money")
}

// A capture retried with a CORRECTED amount is refused, not answered with the
// first charge. That shape let a changed-amount retry keep the original number.
func TestOr891_ServiceCaptureRefusesAChangedAmount(t *testing.T) {
	svc, ms, rdb, payer, ctx := idemEnv(t, 10_000_000)
	pid := payer.UUID()
	reqID := uuid.NewString()

	res, err := svc.Admit(ctx, billingservice.AdmitInput{
		CustomerID: payer, Invoker: "user:z", InvokerType: "payer",
		Currency: money.DefaultCurrency, EstimatedAmount: 5_000,
		Source: "admit", SourceID: reqID,
	})
	require.NoError(t, err)
	require.True(t, res.Allowed)
	require.NoError(t, rdb.FlushDB(ctx).Err())

	before := idemBalance(t, ms, ctx, payer)
	_, err = svc.CaptureHold(ctx, billingservice.CaptureHoldRequest{
		RequestID: reqID, Amount: 400, CustomerID: pid.String(),
		Currency: money.DefaultCurrency, Invoker: "user:z", AdmitSource: "admit",
	})
	require.NoError(t, err)

	_, err = svc.CaptureHold(ctx, billingservice.CaptureHoldRequest{
		RequestID: reqID, Amount: 4_000, CustomerID: pid.String(),
		Currency: money.DefaultCurrency, Invoker: "user:z", AdmitSource: "admit",
	})
	require.ErrorIs(t, err, money.ErrIdempotencyKeyReused,
		"a changed-amount capture retry must be refused")

	// The identical retry is still an idempotent replay.
	_, err = svc.CaptureHold(ctx, billingservice.CaptureHoldRequest{
		RequestID: reqID, Amount: 400, CustomerID: pid.String(),
		Currency: money.DefaultCurrency, Invoker: "user:z", AdmitSource: "admit",
	})
	require.NoError(t, err)
	require.Equal(t, int64(400), before-idemBalance(t, ms, ctx, payer))
}

// A metered usage event retried with a corrected amount is refused too.
func TestOr891_ServiceRecordUsageRefusesAChangedAmount(t *testing.T) {
	svc, ms, _, payer, ctx := idemEnv(t, 10_000_000)
	key := uuid.NewString()
	before := idemBalance(t, ms, ctx, payer)

	require.NoError(t, svc.RecordUsage(ctx, billingservice.RecordUsageInput{
		CustomerID: payer, Currency: money.DefaultCurrency, EventType: "invoke",
		Amount: 700, Source: "invoke", SourceID: key,
	}))
	err := svc.RecordUsage(ctx, billingservice.RecordUsageInput{
		CustomerID: payer, Currency: money.DefaultCurrency, EventType: "invoke",
		Amount: 7_000, Source: "invoke", SourceID: key,
	})
	require.ErrorIs(t, err, money.ErrIdempotencyKeyReused)
	require.Equal(t, int64(700), before-idemBalance(t, ms, ctx, payer))
}

// The key is required but NOT self-enforcing: a key minted per attempt passes
// every check and guarantees nothing. This pins the property the doc comments
// now state, so a future reader does not re-derive "the key is enforced,
// therefore retries are safe".
func TestOr891_AMintedKeyIsNotAnIdempotencyKey(t *testing.T) {
	svc, ms, _, payer, ctx := idemEnv(t, 10_000_000)
	pid := payer.UUID()

	before := idemBalance(t, ms, ctx, payer)
	for i := 0; i < 2; i++ {
		k := uuid.New() // a fresh key per attempt — what a retried handler did
		_, err := svc.DepositCredits(ctx, billingservice.DepositCreditsRequest{
			CustomerID: &payer, Invoker: pid.String(), Currency: money.DefaultCurrency,
			Amount: 2_500_000, Source: "admin", SourceID: &k,
		})
		require.NoError(t, err)
	}
	require.Equal(t, int64(5_000_000), idemBalance(t, ms, ctx, payer)-before,
		"a per-attempt key double-credits: enforcement cannot supply reproducibility")

	// The same deposit under a REPRODUCIBLE key credits once, however often it
	// is retried.
	derived := uuid.NewSHA1(uuid.NameSpaceOID, []byte("admin-deposit:"+pid.String()+":batch-7"))
	before = idemBalance(t, ms, ctx, payer)
	for i := 0; i < 3; i++ {
		_, err := svc.DepositCredits(ctx, billingservice.DepositCreditsRequest{
			CustomerID: &payer, Invoker: pid.String(), Currency: money.DefaultCurrency,
			Amount: 2_500_000, Source: "admin", SourceID: &derived,
		})
		require.NoError(t, err)
	}
	require.Equal(t, int64(2_500_000), idemBalance(t, ms, ctx, payer)-before)
}
