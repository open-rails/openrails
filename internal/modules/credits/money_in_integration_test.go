//go:build integration

package credits_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/modules/credits"
	"github.com/open-rails/openrails/pkg/identity"
	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"
)

// moneyInEnv seeds a fresh credit type + owner with NO initial deposit (balance 0).
func moneyInEnv(t *testing.T) (*credits.CreditsService, *bun.DB, identity.OwnerOrgID, string, context.Context) {
	t.Helper()
	dsn := dbtest.SharedPostgresDSN(t)
	sqlDB := sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dsn)))
	t.Cleanup(func() { _ = sqlDB.Close() })
	bunDB := bun.NewDB(sqlDB, pgdialect.New())
	models.RegisterModels(bunDB)
	ctx := context.Background()
	require.NoError(t, bunDB.PingContext(ctx))

	var hasSettings bool
	require.NoError(t, bunDB.NewSelect().
		ColumnExpr("EXISTS (SELECT 1 FROM information_schema.tables WHERE table_schema='billing' AND table_name='credit_account_settings')").
		Scan(ctx, &hasSettings))
	if !hasSettings {
		t.Skip("billing.credit_account_settings missing; run migration 043")
	}

	dbi, err := db.NewWithBun(bunDB)
	require.NoError(t, err)

	now := time.Now().UTC().Truncate(time.Second)
	ctName := "test_moneyin_" + uuid.NewString()
	ctID := uuid.New()
	_, err = bunDB.NewInsert().Model(&models.CreditType{
		ID: ctID, Name: ctName, DisplayName: "Money-in Test", Unit: "cents",
		DecimalPlaces: 2, IsActive: true, CreatedAt: now,
	}).Exec(ctx)
	require.NoError(t, err)

	owner := identity.OwnerOrgIDFromString(uuid.NewString())
	ownerID := owner.UUID()
	t.Cleanup(func() {
		_, _ = bunDB.NewDelete().Model((*models.CreditSpendLimit)(nil)).Where("owner_id = ?", ownerID).Exec(ctx)
		_, _ = bunDB.NewDelete().Model((*models.CreditAccountSettings)(nil)).Where("owner_id = ?", ownerID).Exec(ctx)
		_, _ = bunDB.NewDelete().Model((*models.CreditBlock)(nil)).Where("owner_id = ?", ownerID).Exec(ctx)
		_, _ = bunDB.NewDelete().Model((*models.CreditTransaction)(nil)).Where("owner_id = ?", ownerID).Exec(ctx)
		_, _ = bunDB.NewDelete().Model((*models.UserCreditBalance)(nil)).Where("owner_id = ?", ownerID).Exec(ctx)
		_, _ = bunDB.NewDelete().Model((*models.CreditType)(nil)).Where("id = ?", ctID).Exec(ctx)
	})
	return credits.NewCreditsService(dbi), bunDB, owner, ctName, ctx
}

// --- fakes ---

type fakeCharger struct {
	charges    []credits.ChargeRequest
	declineAll bool
}

func (f *fakeCharger) ChargeSavedMethod(_ context.Context, req credits.ChargeRequest) (credits.ChargeResult, error) {
	f.charges = append(f.charges, req)
	if f.declineAll {
		return credits.ChargeResult{Declined: true}, nil
	}
	return credits.ChargeResult{TransactionID: "tx_" + req.IdempotencyKey}, nil
}

type fakeAlerter struct{ calls int }

func (f *fakeAlerter) LowBalanceAlert(_ context.Context, _ identity.OwnerOrgID, _ string, _, _ int64) error {
	f.calls++
	return nil
}

func latestBlock(t *testing.T, bunDB *bun.DB, ctx context.Context, ownerID uuid.UUID) *models.CreditBlock {
	t.Helper()
	b := new(models.CreditBlock)
	require.NoError(t, bunDB.NewSelect().Model(b).Where("owner_id = ?", ownerID).OrderExpr("created_at DESC").Limit(1).Scan(ctx))
	return b
}

// --- #240 expiry default ---

func TestDeposit_DefaultExpiry_NoSettingsRow(t *testing.T) {
	svc, bunDB, owner, ct, ctx := moneyInEnv(t)
	_, err := svc.Deposit(ctx, credits.CreditDepositParams{
		OwnerID: &owner, UserID: owner.UUID().String(), CreditType: ct, Amount: 1000,
		Source: "purchase", ApplyAccountExpiryDefault: true,
	})
	require.NoError(t, err)
	b := latestBlock(t, bunDB, ctx, owner.UUID())
	require.NotNil(t, b.ExpiresAt, "default 365d expiry should be applied")
	days := b.ExpiresAt.Sub(time.Now().UTC()).Hours() / 24
	require.InDelta(t, 365, days, 1.5)
}

func TestDeposit_NoFlag_Permanent(t *testing.T) {
	svc, bunDB, owner, ct, ctx := moneyInEnv(t)
	_, err := svc.Deposit(ctx, credits.CreditDepositParams{
		OwnerID: &owner, UserID: owner.UUID().String(), CreditType: ct, Amount: 1000, Source: "grant",
	})
	require.NoError(t, err)
	b := latestBlock(t, bunDB, ctx, owner.UUID())
	require.Nil(t, b.ExpiresAt, "no flag, no explicit expiry -> permanent")
}

func TestDeposit_ConfiguredExpiryDays(t *testing.T) {
	svc, bunDB, owner, ct, ctx := moneyInEnv(t)
	days := 30
	_, err := svc.UpsertAccountSettings(ctx, owner, ct, credits.AccountSettingsInput{DefaultCreditExpiryDays: &days})
	require.NoError(t, err)
	_, err = svc.Deposit(ctx, credits.CreditDepositParams{
		OwnerID: &owner, UserID: owner.UUID().String(), CreditType: ct, Amount: 1000,
		Source: "purchase", ApplyAccountExpiryDefault: true,
	})
	require.NoError(t, err)
	b := latestBlock(t, bunDB, ctx, owner.UUID())
	require.NotNil(t, b.ExpiresAt)
	require.InDelta(t, 30, b.ExpiresAt.Sub(time.Now().UTC()).Hours()/24, 1.5)
}

// --- #240 low-balance alerts ---

func TestRunLowBalanceAlerts(t *testing.T) {
	svc, _, owner, ct, ctx := moneyInEnv(t)
	thr := int64(1000)
	_, err := svc.UpsertAccountSettings(ctx, owner, ct, credits.AccountSettingsInput{LowBalanceThreshold: &thr})
	require.NoError(t, err)
	// available 500 < 1000
	_, err = svc.Deposit(ctx, credits.CreditDepositParams{OwnerID: &owner, UserID: owner.UUID().String(), CreditType: ct, Amount: 500, Source: "seed"})
	require.NoError(t, err)

	al := &fakeAlerter{}
	n, err := svc.RunLowBalanceAlerts(ctx, al, time.Hour)
	require.NoError(t, err)
	require.Equal(t, 1, n)
	require.Equal(t, 1, al.calls)

	// Re-run within cooldown -> deduped.
	n2, err := svc.RunLowBalanceAlerts(ctx, al, time.Hour)
	require.NoError(t, err)
	require.Equal(t, 0, n2)
	require.Equal(t, 1, al.calls)
}

// --- #239 auto-top-up ---

func TestRunAutoTopups_ChargesAndDeposits(t *testing.T) {
	svc, _, owner, ct, ctx := moneyInEnv(t)
	thr, amt := int64(1000), int64(5000)
	pm := uuid.New()
	enabled := true
	_, err := svc.UpsertAccountSettings(ctx, owner, ct, credits.AccountSettingsInput{
		LowBalanceThreshold: &thr, AutoTopupEnabled: &enabled, AutoTopupAmountCents: &amt, AutoTopupPaymentMethod: &pm,
	})
	require.NoError(t, err)
	_, err = svc.Deposit(ctx, credits.CreditDepositParams{OwnerID: &owner, UserID: owner.UUID().String(), CreditType: ct, Amount: 500, Source: "seed"})
	require.NoError(t, err)

	ch := &fakeCharger{}
	n, err := svc.RunAutoTopups(ctx, ch, time.Hour)
	require.NoError(t, err)
	require.Equal(t, 1, n)
	require.Len(t, ch.charges, 1)
	require.Equal(t, int64(5000), ch.charges[0].AmountCents)
	require.Equal(t, pm, ch.charges[0].PaymentMethodID)

	bal, err := svc.GetBalanceForOwner(ctx, owner, ct)
	require.NoError(t, err)
	require.Equal(t, int64(5500), bal.Balance) // 500 + 5000

	// Re-run within cooldown: no second charge.
	n2, err := svc.RunAutoTopups(ctx, ch, time.Hour)
	require.NoError(t, err)
	require.Equal(t, 0, n2)
	require.Len(t, ch.charges, 1)
}

func TestRunAutoTopups_Declined(t *testing.T) {
	svc, _, owner, ct, ctx := moneyInEnv(t)
	thr, amt := int64(1000), int64(5000)
	pm := uuid.New()
	enabled := true
	_, err := svc.UpsertAccountSettings(ctx, owner, ct, credits.AccountSettingsInput{
		LowBalanceThreshold: &thr, AutoTopupEnabled: &enabled, AutoTopupAmountCents: &amt, AutoTopupPaymentMethod: &pm,
	})
	require.NoError(t, err)
	_, err = svc.Deposit(ctx, credits.CreditDepositParams{OwnerID: &owner, UserID: owner.UUID().String(), CreditType: ct, Amount: 500, Source: "seed"})
	require.NoError(t, err)

	ch := &fakeCharger{declineAll: true}
	n, err := svc.RunAutoTopups(ctx, ch, time.Hour)
	require.NoError(t, err)
	require.Equal(t, 0, n)
	require.Len(t, ch.charges, 1, "charge attempted")
	bal, err := svc.GetBalanceForOwner(ctx, owner, ct)
	require.NoError(t, err)
	require.Equal(t, int64(500), bal.Balance, "declined -> no deposit")
}
