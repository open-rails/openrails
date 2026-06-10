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
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/modules/credits"
	"github.com/open-rails/openrails/internal/modules/entitlements"
	"github.com/open-rails/openrails/pkg/identity"
	billingservice "github.com/open-rails/openrails/pkg/service"
)

func authzEnv(t *testing.T) (*billingservice.Service, *credits.CreditsService, identity.TenantSubjectID, string, context.Context) {
	t.Helper()
	dsn := dbtest.SharedPostgresDSN(t)
	ctx := context.Background()

	dbi := dbtest.OpenAppDB(t, dsn)
	pool := dbi.Pool()

	var ok bool
	require.NoError(t, pool.QueryRow(ctx,
		"SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_schema='billing' AND table_name='credit_account_settings')",
	).Scan(&ok))
	if !ok {
		t.Skip("billing.credit_account_settings missing; run migration 043")
	}

	rt := &app.Runtime{
		DB:                 dbi,
		CreditsService:     credits.NewCreditsService(dbi),
		EntitlementService: entitlements.NewEntitlementService(dbi),
		Clock:              clockwork.NewRealClock(),
	}
	svc, err := billingservice.New(rt)
	require.NoError(t, err)

	now := time.Now().UTC().Truncate(time.Second)
	ctName := "svc_authz_" + uuid.NewString()
	ctID := uuid.New()
	_, err = gen.New(pool).CreateCreditType(ctx, gen.CreateCreditTypeParams{
		ID: ctID, Name: ctName, DisplayName: "Authz Test", Unit: "cents", DecimalPlaces: 2, IsActive: true, CreatedAt: now,
	})
	require.NoError(t, err)

	payer := identity.TenantSubjectIDFromString(uuid.NewString())
	payerID := payer.UUID()
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM billing.credit_account_settings WHERE tenant_subject_id = $1", payerID)
		_, _ = pool.Exec(ctx, "DELETE FROM billing.credit_blocks WHERE tenant_subject_id = $1", payerID)
		_, _ = pool.Exec(ctx, "DELETE FROM billing.credit_transactions WHERE tenant_subject_id = $1", payerID)
		_, _ = pool.Exec(ctx, "DELETE FROM billing.credit_balances WHERE tenant_subject_id = $1", payerID)
		_, _ = pool.Exec(ctx, "DELETE FROM billing.credit_types WHERE id = $1", ctID)
	})
	return svc, credits.NewCreditsService(dbi), payer, ctName, ctx
}

// testPool opens a fresh app DB handle on the shared DSN and returns its pgx
// pool — used by tests for fixture cleanup and assertion reads (closed on test
// cleanup).
func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	return dbtest.OpenAppDB(t, dbtest.SharedPostgresDSN(t)).Pool()
}

func TestAuthorizeSpend_PrepaidBalanceGate(t *testing.T) {
	svc, cs, payer, ct, ctx := authzEnv(t)
	_, err := cs.Deposit(ctx, credits.CreditDepositParams{TenantSubjectID: &payer, Actor: payer.UUID().String(), CreditType: ct, Amount: 1000, Source: "seed"})
	require.NoError(t, err)

	ok, err := svc.AuthorizeSpend(ctx, billingservice.AuthorizeSpendRequest{TenantSubjectID: payer, CreditType: ct, Actor: "user:a", EstimateMicros: 500})
	require.NoError(t, err)
	require.True(t, ok.Allowed)
	require.Equal(t, int64(1000), ok.AvailableMicros)

	deny, err := svc.AuthorizeSpend(ctx, billingservice.AuthorizeSpendRequest{TenantSubjectID: payer, CreditType: ct, Actor: "user:a", EstimateMicros: 1500})
	require.NoError(t, err)
	require.False(t, deny.Allowed)
	require.Equal(t, billingservice.DenyInsufficientBalance, deny.DenyCode)
}

func TestAuthorizeSpend_DailyCapDeny(t *testing.T) {
	svc, cs, payer, ct, ctx := authzEnv(t)
	_, err := cs.Deposit(ctx, credits.CreditDepositParams{TenantSubjectID: &payer, Actor: payer.UUID().String(), CreditType: ct, Amount: 100000, Source: "seed"})
	require.NoError(t, err)
	cap := int64(1000)
	require.NoError(t, svc.SetCreditAccountSettings(ctx, payer, ct, credits.AccountSettingsInput{MaxSpendPerDayMicros: &cap}))
	_, err = cs.Withdraw(ctx, credits.CreditWithdrawParams{TenantSubjectID: &payer, Actor: "user:a", CreditType: ct, Amount: 800, Source: "usage"})
	require.NoError(t, err)

	deny, err := svc.AuthorizeSpend(ctx, billingservice.AuthorizeSpendRequest{TenantSubjectID: payer, CreditType: ct, Actor: "user:a", EstimateMicros: 300})
	require.NoError(t, err)
	require.False(t, deny.Allowed)
	require.Equal(t, credits.DenyDailyCap, deny.DenyCode)
	require.Greater(t, deny.RetryAfterSeconds, int64(0))

	ok, err := svc.AuthorizeSpend(ctx, billingservice.AuthorizeSpendRequest{TenantSubjectID: payer, CreditType: ct, Actor: "user:a", EstimateMicros: 100})
	require.NoError(t, err)
	require.True(t, ok.Allowed)
}

func TestGetCreditAccount_Snapshot(t *testing.T) {
	svc, cs, payer, ct, ctx := authzEnv(t)
	_, err := cs.Deposit(ctx, credits.CreditDepositParams{TenantSubjectID: &payer, Actor: payer.UUID().String(), CreditType: ct, Amount: 5000, Source: "seed"})
	require.NoError(t, err)
	_, err = cs.Hold(ctx, &payer, "user:a", ct, 1500, "usage", "h1", time.Now().Add(time.Hour).UTC())
	require.NoError(t, err)

	snap, err := svc.GetCreditAccount(ctx, payer, ct)
	require.NoError(t, err)
	require.Equal(t, int64(5000), snap.BalanceMicros)
	require.Equal(t, int64(1500), snap.HeldMicros)
	require.Equal(t, int64(3500), snap.AvailableMicros)
	require.Equal(t, credits.BillingModePrepaid, snap.BillingMode)
}
