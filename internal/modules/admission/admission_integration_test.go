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

// admitEnv provisions an Admitter + money service + one payer.
func admitEnv(t *testing.T) (*admission.Admitter, *money.MoneyService, *admission.TierPolicyStore, identity.CustomerID, string, context.Context, *abuse.BlocklistService) {
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
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.budget_inflight_holds WHERE customer_id = $1", payerID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.money_settings WHERE customer_id = $1", payerID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.money_blocks WHERE customer_id = $1", payerID)
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

	cs := money.NewMoneyService(dbi)
	store := admission.NewTierPolicyStore(dbi)
	bl := abuse.NewBlocklistService(dbi)
	bsvc := budgets.NewService(dbi)
	adm := admission.NewAdmitter(ratelimit.NewLimiter(rdb), cs, store, bl, bsvc)
	return adm, cs, store, payer, money.DefaultCurrency, ctx, bl
}

func TestAdmit_ThroughputDeny(t *testing.T) {
	adm, _, store, payer, _, ctx, _ := admitEnv(t)
	require.NoError(t, store.UpsertTierPolicy(ctx, payer, "free",
		[]models.ThroughputWindow{{Unit: "request", WindowSeconds: 60, Max: 2}}))

	req := admission.AdmitRequest{CustomerID: payer, Invoker: "user:a", Tier: "free", Resource: "gpt-4o",
		Amounts: map[string]int64{"request": 1}, EstimatedAmount: 0}
	for i := 0; i < 2; i++ {
		d, err := adm.Admit(ctx, req)
		require.NoError(t, err)
		require.True(t, d.Allowed, "admit %d", i+1)
	}
	d, err := adm.Admit(ctx, req)
	require.NoError(t, err)
	require.False(t, d.Allowed)
	require.Equal(t, "throughput", d.BlockedBy)
	require.Equal(t, "request", d.BlockedUnit)
	require.Greater(t, d.RetryAfter, time.Duration(0))
}

func TestAdmit_MoneyDeny(t *testing.T) {
	adm, _, store, payer, _, ctx, _ := admitEnv(t)
	require.NoError(t, store.UpsertTierPolicy(ctx, payer, "free",
		[]models.ThroughputWindow{{Unit: "request", WindowSeconds: 60, Max: 100}}))
	// no deposit -> balance 0, prepaid
	d, err := adm.Admit(ctx, admission.AdmitRequest{
		CustomerID: payer, Invoker: "user:a", Tier: "free", Resource: "gpt-4o",
		Amounts: map[string]int64{"request": 1}, EstimatedAmount: 500,
		Source: "usage", SourceID: "m1", ExpiresAt: time.Now().Add(time.Hour),
	})
	require.NoError(t, err)
	require.False(t, d.Allowed)
	require.Equal(t, "money", d.BlockedBy)
	require.Equal(t, money.DenyInsufficientBalance, d.DenyCode)
}

func TestAdmit_Allow(t *testing.T) {
	adm, cs, store, payer, _, ctx, _ := admitEnv(t)
	require.NoError(t, store.UpsertTierPolicy(ctx, payer, "free",
		[]models.ThroughputWindow{{Unit: "request", WindowSeconds: 60, Max: 100}}))
	_, err := cs.Deposit(ctx, money.DepositParams{CustomerID: &payer, Invoker: payer.UUID().String(), Amount: 10_000, Source: "seed"})
	require.NoError(t, err)

	d, err := adm.Admit(ctx, admission.AdmitRequest{
		CustomerID: payer, Invoker: "user:a", Tier: "free", Resource: "gpt-4o",
		Amounts: map[string]int64{"request": 1, "token": 50}, EstimatedAmount: 500,
		Source: "usage", SourceID: "ok1", ExpiresAt: time.Now().Add(time.Hour),
	})
	require.NoError(t, err)
	require.True(t, d.Allowed)
	require.NotNil(t, d.Hold, "allowed admit reserves a money hold")
	require.NotEmpty(t, d.Windows)
}

func TestAdmit_BlocklistDeny(t *testing.T) {
	adm, _, store, payer, _, ctx, bl := admitEnv(t)
	require.NoError(t, store.UpsertTierPolicy(ctx, payer, "free",
		[]models.ThroughputWindow{{Unit: "request", WindowSeconds: 60, Max: 100}}))
	fp := "fp_" + uuid.NewString()
	require.NoError(t, bl.Add(ctx, nil, "card_fingerprint", fp, "test"))

	d, err := adm.Admit(ctx, admission.AdmitRequest{
		CustomerID: payer, Invoker: "user:a", Tier: "free", Resource: "gpt-4o",
		Amounts:     map[string]int64{"request": 1},
		BlockChecks: []admission.BlockCheck{{Kind: "card_fingerprint", Value: fp}},
	})
	require.NoError(t, err)
	require.False(t, d.Allowed)
	require.Equal(t, "blocked", d.BlockedBy)
	require.Equal(t, "card_fingerprint", d.BlockedUnit)
}

func TestAdmit_EndpointGating(t *testing.T) {
	adm, _, store, payer, _, ctx, _ := admitEnv(t)
	require.NoError(t, store.UpsertTierPolicyFull(ctx, payer, "free", models.ThroughputPolicy{
		Windows:           []models.ThroughputWindow{{Unit: "request", WindowSeconds: 60, Max: 100}},
		EntitledResources: []string{"dall-e-3"},
	}))

	d, err := adm.Admit(ctx, admission.AdmitRequest{CustomerID: payer, Invoker: "user:a", Tier: "free",
		Resource: "gpt-4o", Amounts: map[string]int64{"request": 1}})
	require.NoError(t, err)
	require.False(t, d.Allowed)
	require.Equal(t, "resource", d.BlockedBy) // deny axis renamed endpoint->resource (#332)

	d, err = adm.Admit(ctx, admission.AdmitRequest{CustomerID: payer, Invoker: "user:a", Tier: "free",
		Resource: "dall-e-3", Amounts: map[string]int64{"request": 1}})
	require.NoError(t, err)
	require.True(t, d.Allowed)
}

func TestAdmit_BudgetDeny(t *testing.T) {
	adm, cs, store, payer, _, ctx, _ := admitEnv(t)
	require.NoError(t, store.UpsertTierPolicyFull(ctx, payer, "free", models.ThroughputPolicy{
		Windows:       []models.ThroughputWindow{{Unit: "request", WindowSeconds: 60, Max: 1000}},
		BudgetWindows: []models.BudgetWindowPolicy{{Key: "1h", WindowSeconds: 3600, Limit: 500}},
	}))
	_, err := cs.Deposit(ctx, money.DepositParams{CustomerID: &payer, Invoker: payer.UUID().String(), Amount: 100_000, Source: "seed"})
	require.NoError(t, err)

	// Budget windows reserve the estimate 1:1 in currency internal units (#337/#463).
	// First request: 400 against the 500/hour budget -> allowed.
	d, err := adm.Admit(ctx, admission.AdmitRequest{CustomerID: payer, Invoker: "user:a", Tier: "free", Resource: "gpt-4o",
		Amounts: map[string]int64{"request": 1}, EstimatedAmount: 400, Source: "usage", SourceID: "b1", ExpiresAt: time.Now().Add(time.Hour)})
	require.NoError(t, err)
	require.True(t, d.Allowed)

	// Second request (200) pushes the window to 600 > 500 -> budget deny.
	d, err = adm.Admit(ctx, admission.AdmitRequest{CustomerID: payer, Invoker: "user:a", Tier: "free", Resource: "gpt-4o",
		Amounts: map[string]int64{"request": 1}, EstimatedAmount: 200, Source: "usage", SourceID: "b2", ExpiresAt: time.Now().Add(time.Hour)})
	require.NoError(t, err)
	require.False(t, d.Allowed)
	require.Equal(t, "budget", d.BlockedBy)
}

func TestAdmit_UnverifiedArrearsDeny(t *testing.T) {
	adm, cs, store, payer, cur, ctx, _ := admitEnv(t)
	require.NoError(t, store.UpsertTierPolicy(ctx, payer, "free",
		[]models.ThroughputWindow{{Unit: "request", WindowSeconds: 60, Max: 1000}}))
	bm := money.BillingModeArrears
	_, err := cs.UpsertAccountSettings(ctx, payer, money.DefaultCurrency, money.AccountSettingsInput{BillingMode: &bm})
	require.NoError(t, err)

	// arrears (credit line) + unverified payment method -> deny.
	d, err := adm.Admit(ctx, admission.AdmitRequest{CustomerID: payer, Invoker: "user:a", Tier: "free", Resource: "gpt-4o",
		Amounts: map[string]int64{"request": 1}, EstimatedAmount: 100, Source: "usage", SourceID: "u1", ExpiresAt: time.Now().Add(time.Hour)})
	require.NoError(t, err)
	require.False(t, d.Allowed)
	require.Equal(t, "unverified", d.BlockedBy)

	// verify -> allowed (arrears with unlimited line).
	require.NoError(t, cs.SetPaymentMethodVerified(ctx, payer, cur, true))
	d, err = adm.Admit(ctx, admission.AdmitRequest{CustomerID: payer, Invoker: "user:a", Tier: "free", Resource: "gpt-4o",
		Amounts: map[string]int64{"request": 1}, EstimatedAmount: 100, Source: "usage", SourceID: "u2", ExpiresAt: time.Now().Add(time.Hour)})
	require.NoError(t, err)
	require.True(t, d.Allowed)
}

func TestAdmit_SuspendedDeny(t *testing.T) {
	adm, cs, store, payer, cur, ctx, _ := admitEnv(t)
	require.NoError(t, store.UpsertTierPolicy(ctx, payer, "free",
		[]models.ThroughputWindow{{Unit: "request", WindowSeconds: 60, Max: 100}}))
	require.NoError(t, cs.Suspend(ctx, payer, cur, "past_due"))

	// The suspension axis runs only on the money path (EstimatedAmount > 0, #299).
	d, err := adm.Admit(ctx, admission.AdmitRequest{CustomerID: payer, Invoker: "user:a", Tier: "free",
		Resource: "gpt-4o", Amounts: map[string]int64{"request": 1},
		EstimatedAmount: 100, Source: "usage", SourceID: "s1", ExpiresAt: time.Now().Add(time.Hour)})
	require.NoError(t, err)
	require.False(t, d.Allowed)
	require.Equal(t, "suspended", d.BlockedBy)
}

// TestAdmit_BudgetReservedEqualsEstimate locks unit parity between the money
// ledger and the budget windows (same currency internal precision, #337/#463): an admit
// with EstimatedAmount=X must reserve exactly X against every budget window.
// Regression test for the (estimate+9)/10 residue (the pre-#337
// internal-units-to-millicents conversion) that under-reserved budgets 10x.
func TestAdmit_BudgetReservedEqualsEstimate(t *testing.T) {
	adm, cs, store, payer, _, ctx, _ := admitEnv(t)
	_, err := cs.Deposit(ctx, money.DepositParams{CustomerID: &payer, Invoker: payer.UUID().String(), Amount: 10_000_000, Source: "seed"}) // $10
	require.NoError(t, err)
	require.NoError(t, store.UpsertTierPolicyFull(ctx, payer, "paid", models.ThroughputPolicy{
		Windows: []models.ThroughputWindow{{Unit: "request", WindowSeconds: 3600, Max: 1000}},
		BudgetWindows: []models.BudgetWindowPolicy{
			{Key: "5h", WindowSeconds: 5 * 3600, Limit: 5_000_000, Cadence: "session"}, // $5
		},
	}))

	const estimate = int64(3_000_000) // $3
	dec, err := adm.Admit(ctx, admission.AdmitRequest{
		CustomerID: payer, Invoker: "invoker-parity", Tier: "paid",
		Amounts: map[string]int64{"request": 1}, EstimatedAmount: estimate,
		Source: "gen", SourceID: "req-parity-1", ExpiresAt: time.Now().Add(time.Hour),
	})
	require.NoError(t, err)
	require.True(t, dec.Allowed, "expected allow: $3 within the $5 window")
	require.NotEmpty(t, dec.BudgetWindows)
	require.Equal(t, estimate, dec.BudgetWindows[0].Reserved,
		"budget must reserve the estimate 1:1 in currency internal units — a 10x divergence means a stale unit-conversion path came back")

	// A second $3 must now be denied: 3+3 > 5. With the old /10 bug this
	// passed trivially (0.6 reserved against a 5_000_000 limit).
	dec2, err := adm.Admit(ctx, admission.AdmitRequest{
		CustomerID: payer, Invoker: "invoker-parity", Tier: "paid",
		Amounts: map[string]int64{"request": 1}, EstimatedAmount: estimate,
		Source: "gen", SourceID: "req-parity-2", ExpiresAt: time.Now().Add(time.Hour),
	})
	require.NoError(t, err)
	require.False(t, dec2.Allowed, "second $3 must exceed the $5 window")
	require.Equal(t, "budget", dec2.BlockedBy)
}
