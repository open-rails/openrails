//go:build integration

package admission_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/integrations/fx"
	"github.com/open-rails/openrails/internal/modules/admission"
	"github.com/open-rails/openrails/internal/modules/budgets"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/pkg/identity"
	"github.com/stretchr/testify/require"
)

// admitEnv provisions an Admitter + money service + one payer.
func admitEnv(t *testing.T) (*admission.Admitter, *money.MoneyService, *admission.TierPolicyStore, *admission.BudgetPolicyStore, identity.CustomerID, string, context.Context) {
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
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.money_settings WHERE customer_id = $1", payerID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.money_transactions WHERE customer_id = $1", payerID)
	})

	cs := money.NewMoneyService(dbi)
	store := admission.NewTierPolicyStore(dbi)
	bpStore := admission.NewBudgetPolicyStore(dbi)
	bsvc := budgets.NewService(dbi)
	adm := admission.NewAdmitter(cs, store, bsvc).WithBudgetScopes(bpStore)
	return adm, cs, store, bpStore, payer, money.DefaultCurrency, ctx
}

func grantInvokerBudget(t *testing.T, ctx context.Context, store *admission.BudgetPolicyStore, payer identity.CustomerID, invoker string, windows []models.BudgetWindowPolicy) {
	t.Helper()
	require.NoError(t, store.Upsert(ctx, payer, admission.BudgetScopePolicy{
		Scope: budgets.ScopeInvoker, Owner: "subject", ScopeKey: invoker, Windows: windows,
	}))
}

func TestAdmit_USDChargeCountsAgainstEURBudget(t *testing.T) {
	adm, cs, store, bpStore, payer, _, ctx := admitEnv(t)
	adm = adm.WithFXProvider(fx.NewMockProvider(map[string]float64{"eur": 1.25}))
	require.NoError(t, store.UpsertTierPolicyFull(ctx, payer, "free", models.ThroughputPolicy{
		PolicyCurrency: "EUR",
	}))
	grantInvokerBudget(t, ctx, bpStore, payer, "user:fx", []models.BudgetWindowPolicy{{Key: "daily", WindowSeconds: 86_400, Limit: 800_000, Currency: "EUR"}})
	_, err := cs.Deposit(ctx, money.DepositParams{CustomerID: &payer, Invoker: payer.UUID().String(), Currency: money.DefaultCurrency, Amount: 10_000_000, Source: "seed"})
	require.NoError(t, err)

	d, err := adm.Admit(ctx, admission.AdmitRequest{
		CustomerID: payer, Currency: money.DefaultCurrency, Invoker: "user:fx", Tier: "free", Resource: "gpt-4o",
		EstimatedAmount: 1_000_000,
		Source:          "usage", SourceID: "fx1", ExpiresAt: time.Now().Add(time.Hour),
	})
	require.NoError(t, err)
	require.True(t, d.Allowed)
	require.Equal(t, "EUR", d.PolicyCurrency)
	require.EqualValues(t, 800_000, d.PolicyAmount)

	d, err = adm.Admit(ctx, admission.AdmitRequest{
		CustomerID: payer, Currency: money.DefaultCurrency, Invoker: "user:fx", Tier: "free", Resource: "gpt-4o",
		EstimatedAmount: 1,
		Source:          "usage", SourceID: "fx2", ExpiresAt: time.Now().Add(time.Hour),
	})
	require.NoError(t, err)
	require.False(t, d.Allowed)
	require.Equal(t, "budget", d.BlockedBy)
	require.Equal(t, "EUR", d.PolicyCurrency)
	require.EqualValues(t, 1, d.PolicyAmount)
}

func TestAdmit_MissingRedisFXRateFailsClosed(t *testing.T) {
	adm, cs, store, bpStore, payer, _, ctx := admitEnv(t)
	adm = adm.WithFXProvider(fx.NewMockProvider(map[string]float64{}))
	require.NoError(t, store.UpsertTierPolicyFull(ctx, payer, "free", models.ThroughputPolicy{
		PolicyCurrency: "EUR",
	}))
	grantInvokerBudget(t, ctx, bpStore, payer, "user:missing-fx", []models.BudgetWindowPolicy{{Key: "daily", WindowSeconds: 86_400, Limit: 800_000, Currency: "EUR"}})
	_, err := cs.Deposit(ctx, money.DepositParams{CustomerID: &payer, Invoker: payer.UUID().String(), Currency: money.DefaultCurrency, Amount: 10_000_000, Source: "seed"})
	require.NoError(t, err)

	_, err = adm.Admit(ctx, admission.AdmitRequest{
		CustomerID: payer, Currency: money.DefaultCurrency, Invoker: "user:missing-fx", Tier: "free", Resource: "gpt-4o",
		EstimatedAmount: 1_000_000,
		Source:          "usage", SourceID: "missing-fx", ExpiresAt: time.Now().Add(time.Hour),
	})
	require.Error(t, err)
	require.ErrorContains(t, err, "currency")
}

func TestAdmit_CustomCreditUnitCannotBeFXConverted(t *testing.T) {
	adm, _, store, bpStore, payer, _, ctx := admitEnv(t)
	adm = adm.WithFXProvider(fx.NewMockProvider(map[string]float64{"eur": 1.25}))
	require.NoError(t, store.UpsertTierPolicyFull(ctx, payer, "free", models.ThroughputPolicy{
		PolicyCurrency: "EUR",
	}))
	grantInvokerBudget(t, ctx, bpStore, payer, "user:custom-fx", []models.BudgetWindowPolicy{{Key: "daily", WindowSeconds: 86_400, Limit: 800_000, Currency: "EUR"}})

	_, err := adm.Admit(ctx, admission.AdmitRequest{
		CustomerID: payer, Currency: "tenant/custom", Invoker: "user:custom-fx", Tier: "free", Resource: "gpt-4o",
		EstimatedAmount: 1,
		Source:          "usage", SourceID: "custom-fx", ExpiresAt: time.Now().Add(time.Hour),
	})
	require.Error(t, err)
}

func TestAdmit_SameCurrencyPolicyWorksWithoutFXProvider(t *testing.T) {
	adm, cs, store, bpStore, payer, _, ctx := admitEnv(t)
	require.NoError(t, store.UpsertTierPolicyFull(ctx, payer, "free", models.ThroughputPolicy{
		PolicyCurrency: money.DefaultCurrency,
	}))
	grantInvokerBudget(t, ctx, bpStore, payer, "user:same-fx", []models.BudgetWindowPolicy{{Key: "daily", WindowSeconds: 86_400, Limit: 1_000_000, Currency: money.DefaultCurrency}})
	_, err := cs.Deposit(ctx, money.DepositParams{CustomerID: &payer, Invoker: payer.UUID().String(), Currency: money.DefaultCurrency, Amount: 10_000_000, Source: "seed"})
	require.NoError(t, err)

	d, err := adm.Admit(ctx, admission.AdmitRequest{
		CustomerID: payer, Currency: money.DefaultCurrency, Invoker: "user:same-fx", Tier: "free", Resource: "gpt-4o",
		EstimatedAmount: 100,
		Source:          "usage", SourceID: "same-fx", ExpiresAt: time.Now().Add(time.Hour),
	})
	require.NoError(t, err)
	require.True(t, d.Allowed)
	require.Equal(t, money.DefaultCurrency, d.PolicyCurrency)
	require.EqualValues(t, 100, d.PolicyAmount)
}

func TestAdmit_MoneyDeny(t *testing.T) {
	adm, _, store, _, payer, _, ctx := admitEnv(t)
	require.NoError(t, store.UpsertTierPolicyFull(ctx, payer, "free", models.ThroughputPolicy{}))
	// no deposit -> balance 0, prepaid
	d, err := adm.Admit(ctx, admission.AdmitRequest{
		CustomerID: payer, Currency: money.DefaultCurrency, Invoker: "user:a", Tier: "free", Resource: "gpt-4o",
		InvokerType: string(identity.InvokerTypePayer), EstimatedAmount: 500,
		Source: "usage", SourceID: "m1", ExpiresAt: time.Now().Add(time.Hour),
	})
	require.NoError(t, err)
	require.False(t, d.Allowed)
	require.Equal(t, "money", d.BlockedBy)
	require.Equal(t, money.DenyInsufficientBalance, d.DenyCode)
}

func TestAdmit_Allow(t *testing.T) {
	adm, cs, store, _, payer, _, ctx := admitEnv(t)
	require.NoError(t, store.UpsertTierPolicyFull(ctx, payer, "free", models.ThroughputPolicy{}))
	_, err := cs.Deposit(ctx, money.DepositParams{CustomerID: &payer, Invoker: payer.UUID().String(), Currency: money.DefaultCurrency, Amount: 10_000, Source: "seed"})
	require.NoError(t, err)

	d, err := adm.Admit(ctx, admission.AdmitRequest{
		CustomerID: payer, Currency: money.DefaultCurrency, Invoker: "user:a", Tier: "free", Resource: "gpt-4o",
		InvokerType: string(identity.InvokerTypePayer), EstimatedAmount: 500,
		Source: "usage", SourceID: "ok1", ExpiresAt: time.Now().Add(time.Hour),
	})
	require.NoError(t, err)
	require.True(t, d.Allowed)
	require.Greater(t, d.AccountCapacityAmount, int64(0), "allowed admit returns account capacity for the Redis hold")
}

func TestAdmit_BudgetDeny(t *testing.T) {
	adm, cs, store, bpStore, payer, _, ctx := admitEnv(t)
	require.NoError(t, store.UpsertTierPolicyFull(ctx, payer, "free", models.ThroughputPolicy{}))
	grantInvokerBudget(t, ctx, bpStore, payer, "user:a", []models.BudgetWindowPolicy{{Key: "1h", WindowSeconds: 3600, Limit: 500}})
	_, err := cs.Deposit(ctx, money.DepositParams{CustomerID: &payer, Invoker: payer.UUID().String(), Currency: money.DefaultCurrency, Amount: 100_000, Source: "seed"})
	require.NoError(t, err)

	// Budget windows reserve the estimate 1:1 in currency internal units (#337/#463).
	// First request: 400 against the 500/hour budget -> allowed.
	d, err := adm.Admit(ctx, admission.AdmitRequest{CustomerID: payer, Currency: money.DefaultCurrency, Invoker: "user:a", Tier: "free", Resource: "gpt-4o",
		EstimatedAmount: 400, Source: "usage", SourceID: "b1", ExpiresAt: time.Now().Add(time.Hour)})
	require.NoError(t, err)
	require.True(t, d.Allowed)

	// Second request (200) pushes the window to 600 > 500 -> budget deny.
	d, err = adm.Admit(ctx, admission.AdmitRequest{CustomerID: payer, Currency: money.DefaultCurrency, Invoker: "user:a", Tier: "free", Resource: "gpt-4o",
		EstimatedAmount: 200, Source: "usage", SourceID: "b2", ExpiresAt: time.Now().Add(time.Hour)})
	require.NoError(t, err)
	require.False(t, d.Allowed)
	require.Equal(t, "budget", d.BlockedBy)
}

// TestAdmit_BudgetReservedEqualsEstimate locks unit parity between the money
// ledger and the budget windows (same currency internal precision, #337/#463): an admit
// with EstimatedAmount=X must reserve exactly X against every budget window.
// Regression test for the (estimate+9)/10 residue (the pre-#337
// internal-units-to-millicents conversion) that under-reserved budgets 10x.
func TestAdmit_BudgetReservedEqualsEstimate(t *testing.T) {
	adm, cs, store, bpStore, payer, _, ctx := admitEnv(t)
	_, err := cs.Deposit(ctx, money.DepositParams{CustomerID: &payer, Invoker: payer.UUID().String(), Currency: money.DefaultCurrency, Amount: 10_000_000, Source: "seed"}) // $10
	require.NoError(t, err)
	require.NoError(t, store.UpsertTierPolicyFull(ctx, payer, "paid", models.ThroughputPolicy{}))
	grantInvokerBudget(t, ctx, bpStore, payer, "invoker-parity", []models.BudgetWindowPolicy{{Key: "5h", WindowSeconds: 5 * 3600, Limit: 5_000_000, Cadence: "session"}})

	const estimate = int64(3_000_000) // $3
	dec, err := adm.Admit(ctx, admission.AdmitRequest{
		CustomerID: payer, Currency: money.DefaultCurrency, Invoker: "invoker-parity", Tier: "paid",
		EstimatedAmount: estimate,
		Source:          "gen", SourceID: "req-parity-1", ExpiresAt: time.Now().Add(time.Hour),
	})
	require.NoError(t, err)
	require.True(t, dec.Allowed, "expected allow: $3 within the $5 window")
	require.NotEmpty(t, dec.BudgetWindows)
	require.Equal(t, estimate, dec.BudgetWindows[0].Reserved,
		"budget must reserve the estimate 1:1 in currency internal units — a 10x divergence means a stale unit-conversion path came back")

	// A second $3 must now be denied: 3+3 > 5. With the old /10 bug this
	// passed trivially (0.6 reserved against a 5_000_000 limit).
	dec2, err := adm.Admit(ctx, admission.AdmitRequest{
		CustomerID: payer, Currency: money.DefaultCurrency, Invoker: "invoker-parity", Tier: "paid",
		EstimatedAmount: estimate,
		Source:          "gen", SourceID: "req-parity-2", ExpiresAt: time.Now().Add(time.Hour),
	})
	require.NoError(t, err)
	require.False(t, dec2.Allowed, "second $3 must exceed the $5 window")
	require.Equal(t, "budget", dec2.BlockedBy)
}
