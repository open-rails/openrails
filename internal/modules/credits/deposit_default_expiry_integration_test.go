//go:build integration

package credits_test

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/credits"
	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"
)

// TestDeposit_AppliesDefaultExpiry verifies issue #240: a deposit with no
// explicit ExpiresAt receives the configured default expiry (365-day global
// fallback unless the credit type carries its own default), stamped on both the
// transaction and the FIFO block.
func TestDeposit_AppliesDefaultExpiry(t *testing.T) {
	dsn := os.Getenv("OPENRAILS_TEST_DB_URL")
	if dsn == "" {
		t.Skip("set OPENRAILS_TEST_DB_URL to run integration tests")
	}

	sqlDB := sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dsn)))
	t.Cleanup(func() { _ = sqlDB.Close() })
	bunDB := bun.NewDB(sqlDB, pgdialect.New())
	models.RegisterModels(bunDB)

	ctx := context.Background()
	require.NoError(t, bunDB.PingContext(ctx))

	var hasCol bool
	require.NoError(t, bunDB.NewSelect().
		ColumnExpr("EXISTS (SELECT 1 FROM information_schema.columns WHERE table_schema='billing' AND table_name='credit_types' AND column_name='default_credit_expiry_days')").
		Scan(ctx, &hasCol))
	if !hasCol {
		t.Skip("migration 052 not applied; run migrations before integration tests")
	}

	dbi, err := db.NewWithBun(bunDB)
	require.NoError(t, err)

	clock := clockwork.NewFakeClockAt(time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC))
	now := clock.Now().UTC()

	// --- credit type WITHOUT a per-type default: relies on the global 365d default.
	globalTypeName := "test_default_expiry_global_" + uuid.NewString()
	globalTypeID := uuid.New()
	_, err = bunDB.NewInsert().Model(&models.CreditType{
		ID: globalTypeID, Name: globalTypeName, DisplayName: "Global", Unit: "units",
		DecimalPlaces: 0, IsActive: true, CreatedAt: now,
	}).Exec(ctx)
	require.NoError(t, err)

	// --- credit type WITH a 30-day per-type default: overrides the global default.
	perTypeName := "test_default_expiry_pertype_" + uuid.NewString()
	perTypeID := uuid.New()
	thirty := 30
	_, err = bunDB.NewInsert().Model(&models.CreditType{
		ID: perTypeID, Name: perTypeName, DisplayName: "PerType", Unit: "units",
		DecimalPlaces: 0, IsActive: true, DefaultCreditExpiryDays: &thirty, CreatedAt: now,
	}).Exec(ctx)
	require.NoError(t, err)

	userID := uuid.NewString()
	t.Cleanup(func() {
		_, _ = bunDB.NewDelete().Model((*models.CreditBlock)(nil)).Where("user_id = ?", userID).Exec(ctx)
		_, _ = bunDB.NewDelete().Model((*models.CreditTransaction)(nil)).Where("user_id = ?", userID).Exec(ctx)
		_, _ = bunDB.NewDelete().Model((*models.UserCreditBalance)(nil)).Where("user_id = ?", userID).Exec(ctx)
		_, _ = bunDB.NewDelete().Model((*models.CreditType)(nil)).Where("id IN (?, ?)", globalTypeID, perTypeID).Exec(ctx)
	})

	svc := credits.NewCreditsService(dbi, clock)
	svc.SetDefaultExpiryDays(365) // mirrors config.Config.DefaultCreditExpiryDays() default

	// 1) Deposit with NO explicit expiry on the global-default type -> now + 365d.
	trx, err := svc.Deposit(ctx, credits.CreditDepositParams{
		UserID: userID, CreditType: globalTypeName, Amount: 1000, Source: "purchase",
	})
	require.NoError(t, err)
	require.NotNil(t, trx.ExpiresAt, "deposit should get a default expiry")
	require.Equal(t, now.Add(365*24*time.Hour), trx.ExpiresAt.UTC())

	block := new(models.CreditBlock)
	require.NoError(t, bunDB.NewSelect().Model(block).Where("source_transaction_id = ?", trx.ID).Scan(ctx))
	require.NotNil(t, block.ExpiresAt)
	require.Equal(t, now.Add(365*24*time.Hour), block.ExpiresAt.UTC())

	// 2) Per-type default (30d) overrides the global default.
	trx2, err := svc.Deposit(ctx, credits.CreditDepositParams{
		UserID: userID, CreditType: perTypeName, Amount: 500, Source: "purchase",
	})
	require.NoError(t, err)
	require.NotNil(t, trx2.ExpiresAt)
	require.Equal(t, now.Add(30*24*time.Hour), trx2.ExpiresAt.UTC())

	// 3) Explicit expiry always wins over any default.
	explicit := now.Add(72 * time.Hour).UTC()
	trx3, err := svc.Deposit(ctx, credits.CreditDepositParams{
		UserID: userID, CreditType: globalTypeName, Amount: 200, Source: "promo",
		ExpiresAt: &explicit,
	})
	require.NoError(t, err)
	require.NotNil(t, trx3.ExpiresAt)
	require.Equal(t, explicit, trx3.ExpiresAt.UTC())
}
