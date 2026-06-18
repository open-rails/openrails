//go:build integration

package service_test

import (
	"context"
	"testing"

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

func TestGetCreditAccount_Snapshot(t *testing.T) {
	svc, ms, payer, ctx := authzEnv(t)
	_, err := ms.Deposit(ctx, money.DepositParams{CustomerID: &payer, Invoker: payer.UUID().String(), Currency: money.DefaultCurrency, Amount: 5000, Source: "seed"})
	require.NoError(t, err)

	snap, err := svc.GetCreditAccount(ctx, payer, money.DefaultCurrency)
	require.NoError(t, err)
	require.Equal(t, int64(5000), snap.BalanceAmount)
	require.Equal(t, int64(0), snap.HeldAmount)
	require.Equal(t, int64(5000), snap.AvailableAmount)
	require.Equal(t, money.BillingModePrepaid, snap.BillingMode)
}
