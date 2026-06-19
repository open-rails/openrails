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
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/integrations/fx"
	"github.com/open-rails/openrails/internal/modules/abuse"
	"github.com/open-rails/openrails/internal/modules/entitlements"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/pkg/identity"
	"github.com/open-rails/openrails/pkg/merchant"
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
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.merchant_configurations")
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.invoker_spend_limits WHERE customer_id = $1", payerID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.payer_spend_limits WHERE customer_id IS NULL OR customer_id = $1", payerID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.money_settings WHERE customer_id = $1", payerID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.usage_events WHERE customer_id = $1", payerID)
	})
	ms := money.NewMoneyService(dbi)
	_, err = ms.Deposit(ctx, money.DepositParams{CustomerID: &payer, Invoker: payer.UUID().String(), Currency: money.DefaultCurrency, Amount: 100_000_000, Source: "seed"})
	require.NoError(t, err)
	return svc, ms, payer, ctx
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

func TestMerchantConfiguration_TwoMerchantsKeepDistinctProfiles(t *testing.T) {
	svc, _, _, ctxA := wastedSvcEnv(t)
	pool := dbtest.OpenAppDB(t, dbtest.SharedPostgresDSN(t)).Pool()

	merchantB := merchant.ID(uuid.New())
	_, err := pool.Exec(context.Background(), `
		INSERT INTO openrails.merchants (id, slug, name, status)
		VALUES ($1, $2, $3, 'active')
		ON CONFLICT (slug) DO UPDATE SET name = EXCLUDED.name
	`, merchantB.UUID(), "profile-b", "Profile B")
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DELETE FROM openrails.merchant_configurations WHERE merchant_id = $1", merchantB.UUID())
		_, _ = pool.Exec(context.Background(), "DELETE FROM openrails.merchants WHERE id = $1", merchantB.UUID())
	})
	ctxB := merchant.WithID(context.Background(), merchantB)

	require.NoError(t, svc.SetMerchantConfiguration(ctxA, billingservice.MerchantConfiguration{
		Profile: &models.MerchantProfileConfiguration{
			DisplayName:  "Merchant A Billing",
			FromEmail:    "billing-a@example.com",
			SupportURL:   "https://a.example/support",
			SupportEmail: "support-a@example.com",
		},
	}))
	require.NoError(t, svc.SetMerchantConfiguration(ctxB, billingservice.MerchantConfiguration{
		Profile: &models.MerchantProfileConfiguration{
			DisplayName:  "Merchant B Billing",
			FromEmail:    "billing-b@example.com",
			SupportURL:   "https://b.example/support",
			SupportEmail: "support-b@example.com",
		},
	}))

	cfgA, foundA, err := svc.GetMerchantConfiguration(ctxA)
	require.NoError(t, err)
	require.True(t, foundA)
	cfgB, foundB, err := svc.GetMerchantConfiguration(ctxB)
	require.NoError(t, err)
	require.True(t, foundB)

	require.Equal(t, "Merchant A Billing", cfgA.Profile.DisplayName)
	require.Equal(t, "billing-a@example.com", cfgA.Profile.FromEmail)
	require.Equal(t, "https://a.example/support", cfgA.Profile.SupportURL)
	require.Equal(t, "support-a@example.com", cfgA.Profile.SupportEmail)
	require.Equal(t, "Merchant B Billing", cfgB.Profile.DisplayName)
	require.Equal(t, "billing-b@example.com", cfgB.Profile.FromEmail)
	require.Equal(t, "https://b.example/support", cfgB.Profile.SupportURL)
	require.Equal(t, "support-b@example.com", cfgB.Profile.SupportEmail)
}

func TestMerchantConfiguration_SetWindowsPreservesProfile(t *testing.T) {
	svc, _, _, ctx := wastedSvcEnv(t)

	require.NoError(t, svc.SetMerchantConfiguration(ctx, billingservice.MerchantConfiguration{
		Profile: &models.MerchantProfileConfiguration{
			DisplayName: "Profile survives",
			FromEmail:   "survives@example.com",
		},
	}))
	require.NoError(t, svc.SetMerchantConfiguration(ctx, billingservice.MerchantConfiguration{
		DelegatedInvokerWastedSpendWindows: []abuse.WastedWindow{
			{Key: "burst", Window: time.Minute, Limit: 123},
		},
	}))

	cfg, found, err := svc.GetMerchantConfiguration(ctx)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, "Profile survives", cfg.Profile.DisplayName)
	require.Equal(t, "survives@example.com", cfg.Profile.FromEmail)
	require.Len(t, cfg.DelegatedInvokerWastedSpendWindows, 1)
	require.EqualValues(t, 123, cfg.DelegatedInvokerWastedSpendWindows[0].Limit)
}

// #499: with merchant config SMALLER than the hardcoded default
// ($1/15m vs $5/15m), an invoker that exceeds the CONFIGURED window is denied at
// admit — proving the merchant-configured window (not DefaultInvokerWastedWindows)
// is the enforced budget.
func TestMerchantConfiguration_ConfiguredDelegatedInvokerWindowDenies(t *testing.T) {
	svc, _, payer, ctx := wastedSvcEnv(t)

	// Configure the merchant default: $1 / 15 min (tighter than the $5 default).
	require.NoError(t, svc.SetMerchantConfiguration(ctx, billingservice.MerchantConfiguration{
		DelegatedInvokerWastedSpendWindows: []abuse.WastedWindow{
			{Key: "burst", Window: 15 * time.Minute, Limit: 1_000_000},
		},
	}))

	// $2 wasted by the invoker — over the configured $1 window, UNDER the $5 default.
	_, err := svc.ReportWastedSpend(ctx, billingservice.WastedSpendInput{
		CustomerID: payer, Invoker: "user:configured", Currency: money.DefaultCurrency, Amount: 2_000_000,
		Source: "test", SourceID: "configured", Reason: "test",
	})
	require.NoError(t, err)

	res, err := svc.Admit(ctx, billingservice.AdmitInput{
		CustomerID: payer, Invoker: "user:configured", InvokerType: string(identity.InvokerTypeDelegated), Tier: "free", Resource: "r",
		Currency: money.DefaultCurrency, EstimatedAmount: 100,
		Source: "usage", SourceID: "blocked-configured",
	})
	require.NoError(t, err)
	require.False(t, res.Allowed, "invoker over the CONFIGURED $1 window must be denied")
	require.Equal(t, "abuse", res.BlockedBy)
}

func TestMerchantConfiguration_EURWasteCountsAgainstUSDInvokerCutoff(t *testing.T) {
	svc, _, payer, ctx := wastedSvcEnv(t)

	require.NoError(t, svc.SetMerchantConfiguration(ctx, billingservice.MerchantConfiguration{
		DelegatedInvokerWastedSpendWindows: []abuse.WastedWindow{
			{Key: "burst", Window: 15 * time.Minute, Limit: 1_000_000, Currency: "USD"},
		},
	}))

	res, err := svc.ReportWastedSpend(ctx, billingservice.WastedSpendInput{
		CustomerID: payer, Invoker: "user:fx-cutoff", Currency: "EUR", Amount: 600_000,
		Source: "test", SourceID: "fx-cutoff", Reason: "test",
	})
	require.NoError(t, err)
	require.Equal(t, "USD", res.PolicyCurrency)
	require.EqualValues(t, 1_200_000, res.PolicyRecordedAmount)

	admit, err := svc.Admit(ctx, billingservice.AdmitInput{
		CustomerID: payer, Invoker: "user:fx-cutoff", InvokerType: string(identity.InvokerTypeDelegated), Tier: "free", Resource: "r",
		Currency: money.DefaultCurrency, EstimatedAmount: 100,
		Source: "usage", SourceID: "blocked-fx-cutoff",
	})
	require.NoError(t, err)
	require.False(t, admit.Allowed)
	require.Equal(t, "abuse", admit.BlockedBy)
	require.Equal(t, "USD", admit.PolicyCurrency)
}

// #499: with no merchant config stored, the resolver falls back to
// DefaultInvokerWastedWindows ($5/15m). $2 wasted is UNDER $5 -> still allowed,
// proving the default is the fallback and the $1 config above was load-bearing.
func TestMerchantConfiguration_UnsetFallsBackToDefault(t *testing.T) {
	svc, _, payer, ctx := wastedSvcEnv(t)
	grantDelegatedSpend(t, svc, ctx, payer, "user:default")

	// No SetMerchantConfiguration call -> hardcoded $5/15m default applies.
	_, err := svc.ReportWastedSpend(ctx, billingservice.WastedSpendInput{
		CustomerID: payer, Invoker: "user:default", Currency: money.DefaultCurrency, Amount: 2_000_000,
		Source: "test", SourceID: "default", Reason: "test",
	})
	require.NoError(t, err)

	res, err := svc.Admit(ctx, billingservice.AdmitInput{
		CustomerID: payer, Invoker: "user:default", InvokerType: string(identity.InvokerTypeDelegated), Tier: "free", Resource: "r",
		Currency: money.DefaultCurrency, EstimatedAmount: 100,
		Source: "usage", SourceID: "ok-default",
	})
	require.NoError(t, err)
	require.True(t, res.Allowed, "$2 wasted is under the $5 default backstop -> allowed")
}

func TestMerchantConfiguration_EmptyConfigFallsBackToDefault(t *testing.T) {
	svc, _, payer, ctx := wastedSvcEnv(t)
	grantDelegatedSpend(t, svc, ctx, payer, "user:empty-config")

	require.NoError(t, svc.SetMerchantConfiguration(ctx, billingservice.MerchantConfiguration{}))
	_, err := svc.ReportWastedSpend(ctx, billingservice.WastedSpendInput{
		CustomerID: payer, Invoker: "user:empty-config", Currency: money.DefaultCurrency, Amount: 2_000_000,
		Source: "test", SourceID: "empty-config", Reason: "test",
	})
	require.NoError(t, err)

	res, err := svc.Admit(ctx, billingservice.AdmitInput{
		CustomerID: payer, Invoker: "user:empty-config", InvokerType: string(identity.InvokerTypeDelegated), Tier: "free", Resource: "r",
		Currency: money.DefaultCurrency, EstimatedAmount: 100,
		Source: "usage", SourceID: "ok-empty-config",
	})
	require.NoError(t, err)
	require.True(t, res.Allowed, "$2 wasted is under the $5 default backstop -> allowed")
}

func TestMerchantConfiguration_InvalidJSONShapeReturnsError(t *testing.T) {
	svc, _, payer, ctx := wastedSvcEnv(t)
	tid, err := merchant.Require(ctx)
	require.NoError(t, err)
	pool := dbtest.OpenAppDB(t, dbtest.SharedPostgresDSN(t)).Pool()
	_, err = pool.Exec(ctx, `
		INSERT INTO openrails.merchant_configurations (merchant_id, config)
		VALUES ($1, '{"delegated_invoker_wasted_spend_windows":[{"key":"bad","window_seconds":"oops","limit":1}]}'::jsonb)
		ON CONFLICT (merchant_id) DO UPDATE SET config = EXCLUDED.config
	`, tid.UUID())
	require.NoError(t, err)

	_, _, err = svc.AbuseUsage(ctx, payer, "user:bad-config", money.DefaultCurrency, "")
	require.Error(t, err)
	require.Contains(t, err.Error(), "merchant config: decode config")
}

// #499: the delegated-invoker wasted-spend config is merchant-wide, while Redis usage for
// a delegated-user invoker is namespaced by payer to avoid cross-platform
// collisions for common delegated IDs like "user:123".
func TestMerchantConfiguration_MerchantWideDelegatedInvokerWindowPayerScopedUsage(t *testing.T) {
	svc, ms, payerA, ctx := wastedSvcEnv(t)
	payerB := identity.CustomerIDFromString(uuid.NewString())
	payerBID := payerB.UUID()
	pool := dbtest.OpenAppDB(t, dbtest.SharedPostgresDSN(t)).Pool()
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.invoker_spend_limits WHERE customer_id = $1", payerBID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.money_settings WHERE customer_id = $1", payerBID)
	})
	_, err := ms.Deposit(ctx, money.DepositParams{CustomerID: &payerB, Invoker: payerB.UUID().String(), Currency: money.DefaultCurrency, Amount: 100_000_000, Source: "seed"})
	require.NoError(t, err)

	require.NoError(t, svc.SetMerchantConfiguration(ctx, billingservice.MerchantConfiguration{
		DelegatedInvokerWastedSpendWindows: []abuse.WastedWindow{
			{Key: "burst", Window: 15 * time.Minute, Limit: 1_000_000},
		},
	}))

	const invoker = "user:same-delegated-id"
	grantDelegatedSpend(t, svc, ctx, payerB, invoker)
	_, err = svc.ReportWastedSpend(ctx, billingservice.WastedSpendInput{
		CustomerID: payerA, Invoker: invoker, Currency: money.DefaultCurrency, Amount: 2_000_000,
		Source: "test", SourceID: "payer-a", Reason: "test",
	})
	require.NoError(t, err)

	resA, err := svc.Admit(ctx, billingservice.AdmitInput{
		CustomerID: payerA, Invoker: invoker, InvokerType: string(identity.InvokerTypeDelegated), Tier: "free", Resource: "r",
		Currency: money.DefaultCurrency, EstimatedAmount: 100,
		Source: "usage", SourceID: "blocked-payer-a",
	})
	require.NoError(t, err)
	require.False(t, resA.Allowed, "payer A's invoker is over the merchant-wide $1 policy")

	resB, err := svc.Admit(ctx, billingservice.AdmitInput{
		CustomerID: payerB, Invoker: invoker, InvokerType: string(identity.InvokerTypeDelegated), Tier: "free", Resource: "r",
		Currency: money.DefaultCurrency, EstimatedAmount: 100,
		Source: "usage", SourceID: "allowed-payer-b",
	})
	require.NoError(t, err)
	require.True(t, resB.Allowed, "same invoker label under payer B must not inherit payer A's Redis usage")

	_, err = svc.ReportWastedSpend(ctx, billingservice.WastedSpendInput{
		CustomerID: payerB, Invoker: invoker, Currency: money.DefaultCurrency, Amount: 2_000_000,
		Source: "test", SourceID: "payer-b", Reason: "test",
	})
	require.NoError(t, err)
	resB, err = svc.Admit(ctx, billingservice.AdmitInput{
		CustomerID: payerB, Invoker: invoker, InvokerType: string(identity.InvokerTypeDelegated), Tier: "free", Resource: "r",
		Currency: money.DefaultCurrency, EstimatedAmount: 100,
		Source: "usage", SourceID: "blocked-payer-b",
	})
	require.NoError(t, err)
	require.False(t, resB.Allowed, "the same merchant-wide policy still applies to payer B's invoker")
}

func TestWastedSpendDirectPayer_GraceThenChargeIdempotently(t *testing.T) {
	svc, _, payer, ctx := wastedSvcEnv(t)
	pool := dbtest.OpenAppDB(t, dbtest.SharedPostgresDSN(t)).Pool()
	require.NoError(t, svc.SetPayerSpendLimits(ctx, identity.CustomerID{}, billingservice.PayerSpendLimitInput{
		Tier: "free",
		BadSpendWindows: []billingservice.TierBudgetWindowInput{
			{Key: "burst", WindowSeconds: int64((15 * time.Minute) / time.Second), Limit: 1_000_000},
		},
	}))

	res, err := svc.ReportWastedSpend(ctx, billingservice.WastedSpendInput{
		CustomerID: payer, Invoker: payer.UUID().String(), InvokerType: string(identity.InvokerTypePayer),
		Currency: money.DefaultCurrency, Amount: 500_000, Source: "waste", SourceID: "under-grace", Reason: "test",
	})
	require.NoError(t, err)
	require.Equal(t, money.DefaultCurrency, res.Currency)
	require.Equal(t, int64(500_000), res.ForgivenAmount)
	require.Zero(t, res.ChargedAmount)
	requireUsageAmount(t, pool, ctx, payer, "under-grace", 0)

	res, err = svc.ReportWastedSpend(ctx, billingservice.WastedSpendInput{
		CustomerID: payer, Invoker: payer.UUID().String(), InvokerType: string(identity.InvokerTypePayer),
		Currency: money.DefaultCurrency, Amount: 1_500_000, Source: "waste", SourceID: "over-grace", Reason: "test",
	})
	require.NoError(t, err)
	require.Equal(t, money.DefaultCurrency, res.Currency)
	require.Equal(t, int64(500_000), res.ForgivenAmount)
	require.Equal(t, int64(1_000_000), res.ChargedAmount)
	requireUsageAmount(t, pool, ctx, payer, "over-grace", 1_000_000)

	res, err = svc.ReportWastedSpend(ctx, billingservice.WastedSpendInput{
		CustomerID: payer, Invoker: payer.UUID().String(), InvokerType: string(identity.InvokerTypePayer),
		Currency: money.DefaultCurrency, Amount: 1_500_000, Source: "waste", SourceID: "over-grace", Reason: "retry",
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
	require.NoError(t, svc.SetPayerSpendLimits(ctx, identity.CustomerID{}, billingservice.PayerSpendLimitInput{
		Tier: "free",
		BadSpendWindows: []billingservice.TierBudgetWindowInput{
			{Key: "burst", WindowSeconds: int64((15 * time.Minute) / time.Second), Limit: 10_000_000},
		},
	}))
	require.NoError(t, svc.SetMerchantConfiguration(ctx, billingservice.MerchantConfiguration{
		DelegatedInvokerWastedSpendWindows: []abuse.WastedWindow{
			{Key: "burst", Window: 15 * time.Minute, Limit: 1_000_000},
		},
	}))

	_, err := svc.ReportWastedSpend(ctx, billingservice.WastedSpendInput{
		CustomerID: payer, Invoker: "service-token:payer-owned", InvokerType: string(identity.InvokerTypePayer),
		Currency: money.DefaultCurrency, Amount: 2_000_000, Source: "waste", SourceID: "direct-over-flat", Reason: "test",
	})
	require.NoError(t, err)

	res, err := svc.Admit(ctx, billingservice.AdmitInput{
		CustomerID: payer, Invoker: "service-token:payer-owned", InvokerType: string(identity.InvokerTypePayer),
		Tier: "free", Resource: "r",
		Currency: money.DefaultCurrency, EstimatedAmount: 100,
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
