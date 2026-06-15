//go:build integration

package service_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jonboulle/clockwork"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/modules/entitlements"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/pkg/identity"
	billingservice "github.com/open-rails/openrails/pkg/service"
)

func authzEnv(t *testing.T) (*billingservice.Service, *money.MoneyService, identity.CustomerID, context.Context) {
	t.Helper()
	dsn := dbtest.SharedPostgresDSN(t)
	ctx := context.Background()

	dbi := dbtest.OpenAppDB(t, dsn)
	pool := dbi.Pool()

	var ok bool
	require.NoError(t, pool.QueryRow(ctx,
		"SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_schema='billing' AND table_name='money_account_settings')",
	).Scan(&ok))
	if !ok {
		t.Skip("openrails.money_account_settings missing; run migrations")
	}

	rt := &app.Runtime{
		DB:                 dbi,
		MoneyService:       money.NewMoneyService(dbi),
		EntitlementService: entitlements.NewEntitlementService(dbi),
		Clock:              clockwork.NewRealClock(),
	}
	svc, err := billingservice.New(rt)
	require.NoError(t, err)

	// Money is unit-less — no credit-type row to seed (#472).
	payer := identity.CustomerIDFromString(uuid.NewString())
	payerID := payer.UUID()
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.money_account_settings WHERE customer_id = $1", payerID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.money_blocks WHERE customer_id = $1", payerID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.money_transactions WHERE customer_id = $1", payerID)
	})
	return svc, money.NewMoneyService(dbi), payer, ctx
}

// testPool opens a fresh app DB handle on the shared DSN and returns its pgx
// pool — used by tests for fixture cleanup and assertion reads (closed on test
// cleanup).
func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	return dbtest.OpenAppDB(t, dbtest.SharedPostgresDSN(t)).Pool()
}

func TestAuthorizeSpend_PrepaidBalanceGate(t *testing.T) {
	svc, ms, payer, ctx := authzEnv(t)
	_, err := ms.Deposit(ctx, money.DepositParams{CustomerID: &payer, Invoker: payer.UUID().String(), Currency: money.DefaultCurrency, Amount: 1000, Source: "seed"})
	require.NoError(t, err)

	ok, err := svc.AuthorizeSpend(ctx, billingservice.AuthorizeSpendRequest{CustomerID: payer, Invoker: "user:a", EstimatedAmount: 500})
	require.NoError(t, err)
	require.True(t, ok.Allowed)
	require.Equal(t, int64(1000), ok.AvailableAmount)

	deny, err := svc.AuthorizeSpend(ctx, billingservice.AuthorizeSpendRequest{CustomerID: payer, Invoker: "user:a", EstimatedAmount: 1500})
	require.NoError(t, err)
	require.False(t, deny.Allowed)
	require.Equal(t, billingservice.DenyInsufficientBalance, deny.DenyCode)
}

func TestAuthorizeSpend_DailyCapDeny(t *testing.T) {
	svc, ms, payer, ctx := authzEnv(t)
	_, err := ms.Deposit(ctx, money.DepositParams{CustomerID: &payer, Invoker: payer.UUID().String(), Currency: money.DefaultCurrency, Amount: 100000, Source: "seed"})
	require.NoError(t, err)
	cap := int64(1000)
	require.NoError(t, svc.SetCreditAccountSettings(ctx, payer, money.DefaultCurrency, money.AccountSettingsInput{MaxSpendPerDay: &cap}))
	_, err = ms.Withdraw(ctx, money.WithdrawParams{CustomerID: &payer, Invoker: "user:a", Currency: money.DefaultCurrency, Amount: 800, Source: "usage"})
	require.NoError(t, err)

	deny, err := svc.AuthorizeSpend(ctx, billingservice.AuthorizeSpendRequest{CustomerID: payer, Invoker: "user:a", EstimatedAmount: 300})
	require.NoError(t, err)
	require.False(t, deny.Allowed)
	require.Equal(t, money.DenyDailyCap, deny.DenyCode)
	require.Greater(t, deny.RetryAfterSeconds, int64(0))

	ok, err := svc.AuthorizeSpend(ctx, billingservice.AuthorizeSpendRequest{CustomerID: payer, Invoker: "user:a", EstimatedAmount: 100})
	require.NoError(t, err)
	require.True(t, ok.Allowed)
}

func TestGetCreditAccount_Snapshot(t *testing.T) {
	svc, ms, payer, ctx := authzEnv(t)
	_, err := ms.Deposit(ctx, money.DepositParams{CustomerID: &payer, Invoker: payer.UUID().String(), Currency: money.DefaultCurrency, Amount: 5000, Source: "seed"})
	require.NoError(t, err)
	_, err = ms.Hold(ctx, &payer, "user:a", money.DefaultCurrency, 1500, "usage", "h1", time.Now().Add(time.Hour).UTC())
	require.NoError(t, err)

	snap, err := svc.GetCreditAccount(ctx, payer, money.DefaultCurrency)
	require.NoError(t, err)
	require.Equal(t, int64(5000), snap.BalanceAmount)
	require.Equal(t, int64(1500), snap.HeldAmount)
	require.Equal(t, int64(3500), snap.AvailableAmount)
	require.Equal(t, money.BillingModePrepaid, snap.BillingMode)
}
