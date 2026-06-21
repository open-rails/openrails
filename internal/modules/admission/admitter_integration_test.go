//go:build integration

package admission_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/modules/admission"
	"github.com/open-rails/openrails/internal/modules/admission/spendgate"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/pkg/identity"
)

// admitEnv wires the spendgate-backed admitter (#513) over real Postgres
// (tier/budget policy + money balance) + real Redis (the atomic gate). Returns the
// admitter, the budget-policy store (to grant delegated caps), a freshly funded
// payer, and a merchant-scoped context.
func admitEnv(t *testing.T, deposit int64) (*admission.Admitter, *admission.InvokerSpendLimitStore, identity.CustomerID, context.Context, *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()

	dbi := dbtest.OpenAppDB(t, dbtest.SharedPostgresDSN(t))
	pool := dbi.Pool()
	dbtest.EnsureTestMerchant(ctx, t, pool)
	ctx = dbtest.WithTestMerchant(ctx)

	payer := identity.CustomerIDFromString(uuid.NewString())
	payerID := payer.UUID()
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.invoker_spend_limits WHERE customer_id = $1", payerID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.money_settings WHERE customer_id = $1", payerID)
	})

	cs := money.NewMoneyService(dbi)
	if deposit > 0 {
		_, err := cs.Deposit(ctx, money.DepositParams{CustomerID: &payer, Invoker: payerID.String(), Currency: money.DefaultCurrency, Amount: deposit, Source: "seed"})
		require.NoError(t, err)
	}

	rdb, _ := dbtest.SharedRedisClient(t)

	bpStore := admission.NewInvokerSpendLimitStore(dbi)
	loader := admission.NewSpendgatePolicyLoader(admission.NewPayerSpendLimitStore(dbi), bpStore, nil)
	adm := admission.NewAdmitter(cs, spendgate.New(rdb), loader)
	return adm, bpStore, payer, ctx, pool
}

func payerReq(payer identity.CustomerID, reqID string, amount int64) admission.AdmitRequest {
	return admission.AdmitRequest{
		CustomerID: payer, Invoker: payer.UUID().String(), InvokerType: string(identity.InvokerTypePayer),
		Currency: money.DefaultCurrency, EstimatedAmount: amount,
		Source: "usage", SourceID: reqID, ExpiresAt: time.Now().Add(time.Hour),
	}
}

func delegatedReq(payer identity.CustomerID, invoker, reqID string, amount int64) admission.AdmitRequest {
	return admission.AdmitRequest{
		CustomerID: payer, Invoker: invoker, InvokerType: string(identity.InvokerTypeDelegated),
		Currency: money.DefaultCurrency, EstimatedAmount: amount,
		Source: "usage", SourceID: reqID, ExpiresAt: time.Now().Add(time.Hour),
	}
}

func TestAdmitter_PrepaidBalanceGate(t *testing.T) {
	adm, _, payer, ctx, _ := admitEnv(t, 1000)

	d, err := adm.Admit(ctx, payerReq(payer, uuid.NewString(), 600))
	require.NoError(t, err)
	require.True(t, d.Allowed)

	// 600 already held; only 400 spendable left.
	d, err = adm.Admit(ctx, payerReq(payer, uuid.NewString(), 600))
	require.NoError(t, err)
	require.False(t, d.Allowed)
	require.Equal(t, "money", d.BlockedBy)
	require.Equal(t, money.DenyInsufficientBalance, d.DenyCode)
}

func TestAdmitter_ArrearsCreditLineFromAdmissionCapacity(t *testing.T) {
	adm, _, payer, ctx, _ := admitEnv(t, 0)
	svc := money.NewMoneyService(dbtest.OpenAppDB(t, dbtest.SharedPostgresDSN(t)))
	require.NoError(t, svc.SetCreditLimit(ctx, payer, money.DefaultCurrency, 500))

	d, err := adm.Admit(ctx, payerReq(payer, uuid.NewString(), 400))
	require.NoError(t, err)
	require.True(t, d.Allowed)

	// The first admit reserved 400 in Redis. With a 500 credit line and zero balance,
	// only 100 remains available for the next in-flight request.
	d, err = adm.Admit(ctx, payerReq(payer, uuid.NewString(), 200))
	require.NoError(t, err)
	require.False(t, d.Allowed)
	require.Equal(t, "money", d.BlockedBy)
	require.Equal(t, money.DenyInsufficientCredit, d.DenyCode)
}

func TestAdmitter_DelegatedWithoutGrantDenied(t *testing.T) {
	adm, _, payer, ctx, _ := admitEnv(t, 1_000_000)

	d, err := adm.Admit(ctx, delegatedReq(payer, "user:nogrant", uuid.NewString(), 100))
	require.NoError(t, err)
	require.False(t, d.Allowed)
	require.Equal(t, "budget", d.BlockedBy)
	require.Equal(t, admission.DenyDelegatedSpendNotAllowed, d.DenyCode)
}

func TestAdmitter_DelegatedInvokerWindowEnforced(t *testing.T) {
	adm, bpStore, payer, ctx, _ := admitEnv(t, 1_000_000_000) // balance never gates
	invoker := "user:alice"

	// Grant the invoker a $1000-per-5h fixed window (the delegated spend cap).
	require.NoError(t, bpStore.Upsert(ctx, payer, admission.InvokerSpendLimit{
		Scope: "invoker", ScopeKey: invoker,
		Windows: []models.BudgetWindowPolicy{{Key: "5h", WindowSeconds: 5 * 3600, Limit: 1000}},
	}))

	d, err := adm.Admit(ctx, delegatedReq(payer, invoker, uuid.NewString(), 600))
	require.NoError(t, err)
	require.True(t, d.Allowed, "first 600 fits the 1000 window")

	d, err = adm.Admit(ctx, delegatedReq(payer, invoker, uuid.NewString(), 600))
	require.NoError(t, err)
	require.False(t, d.Allowed, "600+600 breaches the 1000 invoker window")
	require.Equal(t, "budget", d.BlockedBy)
	require.Equal(t, admission.DenyBudgetExceeded, d.DenyCode)

	// A DIFFERENT invoker under the same payer is unconstrained by alice's window —
	// but has no grant of its own, so it is denied delegated-spend (proving per-invoker
	// isolation of the window, not a shared payer counter).
	d, err = adm.Admit(ctx, delegatedReq(payer, "user:bob", uuid.NewString(), 600))
	require.NoError(t, err)
	require.False(t, d.Allowed)
	require.Equal(t, admission.DenyDelegatedSpendNotAllowed, d.DenyCode)
}

func TestAdmitter_StaggeredPerPayerWindows(t *testing.T) {
	// Two payers, identical trust-tier policy, first-charge at DIFFERENT times → their
	// fixed windows are anchored independently (staggered), never a global reset.
	// Here we assert isolation: payer A maxing its window does NOT affect payer B.
	adm, bpStore, payerA, ctx, pool := admitEnv(t, 1_000_000_000)
	payerB := identity.CustomerIDFromString(uuid.NewString())
	_, err := money.NewMoneyService(dbtest.OpenAppDB(t, dbtest.SharedPostgresDSN(t))).
		Deposit(ctx, money.DepositParams{CustomerID: &payerB, Invoker: payerB.UUID().String(), Currency: money.DefaultCurrency, Amount: 1_000_000_000, Source: "seed"})
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.invoker_spend_limits WHERE customer_id = $1", payerB.UUID())
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.money_settings WHERE customer_id = $1", payerB.UUID())
	})
	inv := "user:shared-name"
	for _, p := range []identity.CustomerID{payerA, payerB} {
		require.NoError(t, bpStore.Upsert(ctx, p, admission.InvokerSpendLimit{
			Scope: "invoker", ScopeKey: inv,
			Windows: []models.BudgetWindowPolicy{{Key: "7d", WindowSeconds: 7 * 24 * 3600, Limit: 1000}},
		}))
	}

	// Payer A spends to its cap.
	d, err := adm.Admit(ctx, delegatedReq(payerA, inv, uuid.NewString(), 1000))
	require.NoError(t, err)
	require.True(t, d.Allowed)
	d, err = adm.Admit(ctx, delegatedReq(payerA, inv, uuid.NewString(), 1))
	require.NoError(t, err)
	require.False(t, d.Allowed, "payer A is at its 7d cap")

	// Payer B's identical window is independent (its own counter key) → unaffected.
	d, err = adm.Admit(ctx, delegatedReq(payerB, inv, uuid.NewString(), 1000))
	require.NoError(t, err)
	require.True(t, d.Allowed, "payer B's window is anchored independently of payer A")
}
