package checkout

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
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/internal/modules/entitlements"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"
)

func TestRegisterPurchase_DuplicateTransactionDoesNotExtendEntitlements(t *testing.T) {
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

	dbi, err := db.NewWithBun(bunDB)
	require.NoError(t, err)

	now := time.Now().UTC().Truncate(time.Second)
	userID := uuid.New().String()
	productID := uuid.New()
	priceID := uuid.New()
	durationDays := 30

	product := &models.Product{
		ID:          productID,
		Slug:        "test_duplicate_purchase_" + uuid.New().String(),
		DisplayName: "Duplicate Purchase Test",
		Description: "Test",
		EntitlementsSpec: map[string]*int{
			"premium_duplicate_purchase": &durationDays,
		},
		IsActive:  true,
		CreatedAt: now,
		UpdatedAt: now,
	}
	price := &models.Price{
		ID:          priceID,
		ProductID:   productID,
		DisplayName: "One-time Test Price",
		IsActive:    true,
		Amount:      1000,
		Currency:    "USD",
		CreatedAt:   now,
		UpdatedAt:   now,
	}

	_, err = bunDB.NewInsert().Model(product).Exec(ctx)
	require.NoError(t, err)
	_, err = bunDB.NewInsert().Model(price).Exec(ctx)
	require.NoError(t, err)

	t.Cleanup(func() {
		_, _ = bunDB.NewDelete().Model((*models.Entitlement)(nil)).Where("user_id = ?", userID).Exec(ctx)
		_, _ = bunDB.NewDelete().Model((*models.Payment)(nil)).Where("user_id = ?", userID).Exec(ctx)
		_, _ = bunDB.NewDelete().Model((*models.Price)(nil)).Where("id = ?", priceID).Exec(ctx)
		_, _ = bunDB.NewDelete().Model((*models.Product)(nil)).Where("id = ?", productID).Exec(ctx)
	})

	fakeClock := clockwork.NewFakeClockAt(now)
	productSvc := catalog.NewProductService(dbi)
	purchaseSvc := NewCheckoutPurchaseService(
		catalog.NewPriceService(dbi),
		productSvc,
		payments.NewPaymentService(dbi, fakeClock),
		entitlements.NewEntitlementService(dbi, fakeClock),
		nil,
		fakeClock,
	)

	txnID := "duplicate_purchase_" + uuid.New().String()
	first, err := purchaseSvc.RegisterPurchase(ctx, &payments.RegisterPurchaseRequest{
		UserID:        userID,
		PriceID:       priceID,
		Processor:     string(models.ProcessorStripe),
		TransactionID: txnID,
		Amount:        price.Amount,
		Currency:      price.Currency,
	})
	require.NoError(t, err)

	var firstEnt models.Entitlement
	require.NoError(t, bunDB.NewSelect().
		Model(&firstEnt).
		Where("user_id = ?", userID).
		Where("entitlement = ?", "premium_duplicate_purchase").
		Where("source_id = ?", first.PaymentID).
		Where("revoked_at IS NULL").
		Where("deleted_at IS NULL").
		Limit(1).
		Scan(ctx))
	require.NotNil(t, firstEnt.EndAt)
	firstEnd := firstEnt.EndAt.UTC()

	newDurationDays := 60
	_, err = productSvc.UpdateDefinition(ctx, productID, catalog.ProductDefinitionUpdateParams{
		EntitlementsSpec: map[string]*int{
			"premium_duplicate_purchase": &newDurationDays,
			"bonus_duplicate_purchase":   &newDurationDays,
		},
		SetEntitlements: true,
	})
	require.NoError(t, err)

	second, err := purchaseSvc.RegisterPurchase(ctx, &payments.RegisterPurchaseRequest{
		UserID:        userID,
		PriceID:       priceID,
		Processor:     string(models.ProcessorStripe),
		TransactionID: txnID,
		Amount:        price.Amount,
		Currency:      price.Currency,
	})
	require.NoError(t, err)
	require.Equal(t, first.PaymentID, second.PaymentID)

	var ents []models.Entitlement
	require.NoError(t, bunDB.NewSelect().
		Model(&ents).
		Where("user_id = ?", userID).
		Where("revoked_at IS NULL").
		Where("deleted_at IS NULL").
		OrderExpr("entitlement ASC").
		Scan(ctx))
	require.Len(t, ents, 1)
	require.Equal(t, "premium_duplicate_purchase", ents[0].Entitlement)
	require.NotNil(t, ents[0].EndAt)
	require.Equal(t, firstEnd, ents[0].EndAt.UTC())
}
