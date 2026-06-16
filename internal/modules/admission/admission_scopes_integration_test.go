//go:build integration

package admission_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/modules/admission"
	"github.com/open-rails/openrails/internal/modules/budgets"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/pkg/identity"
	"github.com/stretchr/testify/require"
)

// scopeEnv builds an admitter WITH hierarchical budget scopes (#473) wired,
// plus the budget-policy store + tier-policy store, a fresh payer, and a
// generously funded USD money balance so the money axis never blocks the
// budget-scope tests. Money has no credit_type dimension (#472).
func scopeEnv(t *testing.T) (*admission.Admitter, *admission.BudgetPolicyStore, *admission.TierPolicyStore, identity.CustomerID, context.Context) {
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
	})

	cs := money.NewMoneyService(dbi)
	// Fund the payer generously so money never blocks (budgets are the gate here).
	_, err := cs.Deposit(ctx, money.DepositParams{CustomerID: &payer, Invoker: payer.UUID().String(), Currency: money.DefaultCurrency, Amount: 1_000_000_000, Source: "seed"})
	require.NoError(t, err)

	tierStore := admission.NewTierPolicyStore(dbi)
	bpStore := admission.NewBudgetPolicyStore(dbi)
	bsvc := budgets.NewService(dbi)
	adm := admission.NewAdmitter(cs, tierStore, bsvc).WithBudgetScopes(bpStore)
	return adm, bpStore, tierStore, payer, ctx
}

func mustReq(payer identity.CustomerID, invoker, srcID string, roles []uuid.UUID, amount int64) admission.AdmitRequest {
	return admission.AdmitRequest{
		CustomerID: payer, Currency: money.DefaultCurrency, Invoker: invoker, Tier: "free", Resource: "gpt-4o",
		Roles:           roles,
		EstimatedAmount: amount, Source: "usage", SourceID: srcID, ExpiresAt: time.Now().Add(time.Hour),
	}
}

// budgetPolicyWindows builds a single 1h fixed-window scope policy capped at
// limit amount.
func budgetPolicyWindows(limit int64) []models.BudgetWindowPolicy {
	return []models.BudgetWindowPolicy{{Key: "1h", WindowSeconds: 3600, Limit: limit}}
}

// TestScope_PlatformCapDeniesEvenWhenRoleAndInvokerUnder: the platform (subject)
// cap denies even when the role pool and invoker cap have room.
// TestScope_InvokerCapIndependent: each invoker's (subject, invoker) cap is
// independent — one invoker hitting its cap does not block a different invoker.
func TestScope_InvokerCapIndependent(t *testing.T) {
	adm, bpStore, tier, payer, ctx := scopeEnv(t)
	require.NoError(t, tier.UpsertTierPolicyFull(ctx, payer, "free", models.ThroughputPolicy{}))
	grantInvokerBudget(t, ctx, bpStore, payer, "user:a", budgetPolicyWindows(400))
	grantInvokerBudget(t, ctx, bpStore, payer, "user:b", budgetPolicyWindows(400))

	// user:a exhausts its own 400 cap.
	d, err := adm.Admit(ctx, mustReq(payer, "user:a", "i1", nil, 400))
	require.NoError(t, err)
	require.True(t, d.Allowed)
	d, err = adm.Admit(ctx, mustReq(payer, "user:a", "i2", nil, 100))
	require.NoError(t, err)
	require.False(t, d.Allowed, "user:a is over its own cap")

	// user:b unaffected — independent (subject, invoker) bucket.
	d, err = adm.Admit(ctx, mustReq(payer, "user:b", "i3", nil, 400))
	require.NoError(t, err)
	require.True(t, d.Allowed, "user:b has its own independent cap")
}

func TestScope_DelegatedSpendRequiresMatchingGrant(t *testing.T) {
	adm, _, tier, payer, ctx := scopeEnv(t)
	require.NoError(t, tier.UpsertTierPolicyFull(ctx, payer, "free", models.ThroughputPolicy{}))

	d, err := adm.Admit(ctx, mustReq(payer, "user:no-grant", "missing-grant", nil, 100))
	require.NoError(t, err)
	require.False(t, d.Allowed)
	require.Equal(t, "budget", d.BlockedBy)
	require.Equal(t, admission.DenyDelegatedSpendNotAllowed, d.DenyCode)
}

func TestScope_RoleGrantAllowsAndCapsDelegatedSpend(t *testing.T) {
	adm, bpStore, tier, payer, ctx := scopeEnv(t)
	roleID := uuid.New()
	require.NoError(t, tier.UpsertTierPolicyFull(ctx, payer, "free", models.ThroughputPolicy{}))
	require.NoError(t, bpStore.Upsert(ctx, payer, admission.BudgetScopePolicy{
		Scope: budgets.ScopeRole, Owner: "subject", ScopeKey: roleID.String(),
		Windows: budgetPolicyWindows(400),
	}))

	d, err := adm.Admit(ctx, mustReq(payer, "user:role", "role-1", []uuid.UUID{roleID}, 300))
	require.NoError(t, err)
	require.True(t, d.Allowed)
	require.Len(t, d.BudgetScopes, 1)
	require.Equal(t, budgets.ScopeRole, d.BudgetScopes[0].Scope)

	d, err = adm.Admit(ctx, mustReq(payer, "user:role", "role-2", []uuid.UUID{roleID}, 200))
	require.NoError(t, err)
	require.False(t, d.Allowed)
	require.Equal(t, "budget", d.BlockedBy)

	d, err = adm.Admit(ctx, mustReq(payer, "user:missing-role", "role-3", nil, 100))
	require.NoError(t, err)
	require.False(t, d.Allowed)
	require.Equal(t, admission.DenyDelegatedSpendNotAllowed, d.DenyCode)
}

func TestScope_InvokerTierGrantAllowsAndCapsDelegatedSpend(t *testing.T) {
	adm, bpStore, tier, payer, ctx := scopeEnv(t)
	require.NoError(t, tier.UpsertTierPolicyFull(ctx, payer, "tier_1", models.ThroughputPolicy{}))
	require.NoError(t, bpStore.Upsert(ctx, payer, admission.BudgetScopePolicy{
		Scope: budgets.ScopeInvokerTier, Owner: "subject", ScopeKey: "tier_1",
		Windows: budgetPolicyWindows(400),
	}))

	req := mustReq(payer, "user:tier-a", "tier-1", nil, 300)
	req.Tier = "tier_1"
	d, err := adm.Admit(ctx, req)
	require.NoError(t, err)
	require.True(t, d.Allowed)
	require.Len(t, d.BudgetScopes, 1)
	require.Equal(t, budgets.ScopeInvokerTier, d.BudgetScopes[0].Scope)

	req = mustReq(payer, "user:tier-a", "tier-2", nil, 200)
	req.Tier = "tier_1"
	d, err = adm.Admit(ctx, req)
	require.NoError(t, err)
	require.False(t, d.Allowed)
	require.Equal(t, "budget", d.BlockedBy)

	req = mustReq(payer, "user:tier-b", "tier-3", nil, 400)
	req.Tier = "tier_1"
	d, err = adm.Admit(ctx, req)
	require.NoError(t, err)
	require.True(t, d.Allowed, "tier grants meter each invoker independently")

	req = mustReq(payer, "user:tier-c", "tier-4", nil, 100)
	req.Tier = "tier_2"
	d, err = adm.Admit(ctx, req)
	require.NoError(t, err)
	require.False(t, d.Allowed)
	require.Equal(t, admission.DenyDelegatedSpendNotAllowed, d.DenyCode)
}
