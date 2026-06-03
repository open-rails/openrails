//go:build integration

package service_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"
	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"

	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/modules/credits"
	"github.com/open-rails/openrails/internal/modules/entitlements"
	"github.com/open-rails/openrails/pkg/identity"
	billingservice "github.com/open-rails/openrails/pkg/service"
)

func authzEnv(t *testing.T) (*billingservice.Service, *credits.CreditsService, identity.OwnerOrgID, string, context.Context) {
	t.Helper()
	dsn := dbtest.SharedPostgresDSN(t)
	sqlDB := sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dsn)))
	t.Cleanup(func() { _ = sqlDB.Close() })
	bunDB := bun.NewDB(sqlDB, pgdialect.New())
	models.RegisterModels(bunDB)
	ctx := context.Background()
	require.NoError(t, bunDB.PingContext(ctx))

	var ok bool
	require.NoError(t, bunDB.NewSelect().
		ColumnExpr("EXISTS (SELECT 1 FROM information_schema.tables WHERE table_schema='billing' AND table_name='credit_account_settings')").
		Scan(ctx, &ok))
	if !ok {
		t.Skip("billing.credit_account_settings missing; run migration 043")
	}

	dbi, err := db.NewWithBun(bunDB)
	require.NoError(t, err)
	rt := &app.Runtime{
		CreditsService:     credits.NewCreditsService(dbi),
		EntitlementService: entitlements.NewEntitlementService(dbi),
		Clock:              clockwork.NewRealClock(),
	}
	svc, err := billingservice.New(rt)
	require.NoError(t, err)

	now := time.Now().UTC().Truncate(time.Second)
	ctName := "svc_authz_" + uuid.NewString()
	ctID := uuid.New()
	_, err = bunDB.NewInsert().Model(&models.CreditType{
		ID: ctID, Name: ctName, DisplayName: "Authz Test", Unit: "cents", DecimalPlaces: 2, IsActive: true, CreatedAt: now,
	}).Exec(ctx)
	require.NoError(t, err)

	owner := identity.OwnerOrgIDFromString(uuid.NewString())
	ownerID := owner.UUID()
	t.Cleanup(func() {
		_, _ = bunDB.NewDelete().Model((*models.CreditAccountSettings)(nil)).Where("owner_id = ?", ownerID).Exec(ctx)
		_, _ = bunDB.NewDelete().Model((*models.CreditBlock)(nil)).Where("owner_id = ?", ownerID).Exec(ctx)
		_, _ = bunDB.NewDelete().Model((*models.CreditTransaction)(nil)).Where("owner_id = ?", ownerID).Exec(ctx)
		_, _ = bunDB.NewDelete().Model((*models.UserCreditBalance)(nil)).Where("owner_id = ?", ownerID).Exec(ctx)
		_, _ = bunDB.NewDelete().Model((*models.CreditType)(nil)).Where("id = ?", ctID).Exec(ctx)
	})
	return svc, credits.NewCreditsService(dbi), owner, ctName, ctx
}

func TestAuthorizeSpend_PrepaidBalanceGate(t *testing.T) {
	svc, cs, owner, ct, ctx := authzEnv(t)
	_, err := cs.Deposit(ctx, credits.CreditDepositParams{OwnerID: &owner, UserID: owner.UUID().String(), CreditType: ct, Amount: 1000, Source: "seed"})
	require.NoError(t, err)

	ok, err := svc.AuthorizeSpend(ctx, billingservice.AuthorizeSpendRequest{OwnerID: &owner, CreditType: ct, Invoker: "user:a", EstimateCents: 500})
	require.NoError(t, err)
	require.True(t, ok.Allowed)
	require.Equal(t, int64(1000), ok.AvailableCents)

	deny, err := svc.AuthorizeSpend(ctx, billingservice.AuthorizeSpendRequest{OwnerID: &owner, CreditType: ct, Invoker: "user:a", EstimateCents: 1500})
	require.NoError(t, err)
	require.False(t, deny.Allowed)
	require.Equal(t, billingservice.DenyInsufficientBalance, deny.DenyCode)
}

func TestAuthorizeSpend_DailyCapDeny(t *testing.T) {
	svc, cs, owner, ct, ctx := authzEnv(t)
	_, err := cs.Deposit(ctx, credits.CreditDepositParams{OwnerID: &owner, UserID: owner.UUID().String(), CreditType: ct, Amount: 100000, Source: "seed"})
	require.NoError(t, err)
	cap := int64(1000)
	require.NoError(t, svc.SetCreditAccountSettings(ctx, owner, ct, credits.AccountSettingsInput{MaxSpendPerDayCents: &cap}))
	_, err = cs.Withdraw(ctx, credits.CreditWithdrawParams{OwnerID: &owner, UserID: "user:a", CreditType: ct, Amount: 800, Source: "usage"})
	require.NoError(t, err)

	deny, err := svc.AuthorizeSpend(ctx, billingservice.AuthorizeSpendRequest{OwnerID: &owner, CreditType: ct, Invoker: "user:a", EstimateCents: 300})
	require.NoError(t, err)
	require.False(t, deny.Allowed)
	require.Equal(t, credits.DenyDailyCap, deny.DenyCode)
	require.Greater(t, deny.RetryAfterSeconds, int64(0))

	ok, err := svc.AuthorizeSpend(ctx, billingservice.AuthorizeSpendRequest{OwnerID: &owner, CreditType: ct, Invoker: "user:a", EstimateCents: 100})
	require.NoError(t, err)
	require.True(t, ok.Allowed)
}

func TestGetCreditAccount_Snapshot(t *testing.T) {
	svc, cs, owner, ct, ctx := authzEnv(t)
	_, err := cs.Deposit(ctx, credits.CreditDepositParams{OwnerID: &owner, UserID: owner.UUID().String(), CreditType: ct, Amount: 5000, Source: "seed"})
	require.NoError(t, err)
	_, err = cs.Hold(ctx, &owner, "user:a", ct, 1500, "usage", "h1", time.Now().Add(time.Hour).UTC())
	require.NoError(t, err)

	snap, err := svc.GetCreditAccount(ctx, owner, ct)
	require.NoError(t, err)
	require.Equal(t, int64(5000), snap.BalanceCents)
	require.Equal(t, int64(1500), snap.HeldCents)
	require.Equal(t, int64(3500), snap.AvailableCents)
	require.Equal(t, credits.BillingModePrepaid, snap.BillingMode)
}
