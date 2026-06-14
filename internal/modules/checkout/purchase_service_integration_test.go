//go:build integration

package checkout

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/internal/modules/entitlements"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/stretchr/testify/require"
)

func TestRegisterPurchase_DuplicateTransactionDoesNotExtendEntitlements(t *testing.T) {
	dsn := dbtest.SharedPostgresDSN(t)

	ctx := context.Background()
	dbi := dbtest.OpenAppDB(t, dsn)
	pool := dbi.Pool()

	now := time.Now().UTC().Truncate(time.Second)
	userID := uuid.New().String()
	tenantSubjectID := dbtest.EnsureMerchantSubjectIDPgx(ctx, t, pool, userID)
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
		Status:    models.CatalogStatusActive,
		CreatedAt: now,
		UpdatedAt: now,
	}
	price := &models.Price{
		ID:        priceID,
		ProductID: productID,
		Status:    models.CatalogStatusActive,
		Amount:    1000,
		Currency:  "USD",
		CreatedAt: now,
		UpdatedAt: now,
	}

	insertProductAndPrice(ctx, t, pool, product, price)

	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.entitlements WHERE merchant_subject_id = $1", tenantSubjectID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.payments WHERE merchant_subject_id = $1", tenantSubjectID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.prices WHERE id = $1", priceID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.products WHERE id = $1", productID)
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

	var firstEndAt *time.Time
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT end_at FROM openrails.entitlements
		 WHERE merchant_subject_id = $1 AND entitlement = $2 AND source_id = $3
		   AND revoked_at IS NULL AND deleted_at IS NULL
		 LIMIT 1`,
		tenantSubjectID, "premium_duplicate_purchase", first.PaymentID,
	).Scan(&firstEndAt))
	require.NotNil(t, firstEndAt)
	firstEnd := firstEndAt.UTC()

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

	type entRow struct {
		Entitlement string
		EndAt       *time.Time
	}
	var ents []entRow
	rows, err := pool.Query(ctx,
		`SELECT entitlement, end_at FROM openrails.entitlements
		 WHERE merchant_subject_id = $1 AND revoked_at IS NULL AND deleted_at IS NULL
		 ORDER BY entitlement ASC`,
		tenantSubjectID)
	require.NoError(t, err)
	for rows.Next() {
		var r entRow
		require.NoError(t, rows.Scan(&r.Entitlement, &r.EndAt))
		ents = append(ents, r)
	}
	rows.Close()
	require.NoError(t, rows.Err())
	require.Len(t, ents, 1)
	require.Equal(t, "premium_duplicate_purchase", ents[0].Entitlement)
	require.NotNil(t, ents[0].EndAt)
	require.Equal(t, firstEnd, ents[0].EndAt.UTC())
}

// TestArchivedPriceStillBillsExistingSubscription is the load-bearing
// grandfather regression for issue #210. An archived price MUST keep billing
// existing subscribers indefinitely (renewal/rebill loads by ID and bills the
// stored amount, status-agnostic), while NEW purchases of the same price are
// rejected.
func TestArchivedPriceStillBillsExistingSubscription(t *testing.T) {
	dsn := dbtest.SharedPostgresDSN(t)

	ctx := context.Background()
	dbi := dbtest.OpenAppDB(t, dsn)
	pool := dbi.Pool()

	now := time.Now().UTC().Truncate(time.Second)
	userID := uuid.New().String()
	tenantSubjectID := dbtest.EnsureMerchantSubjectIDPgx(ctx, t, pool, userID)
	productID := uuid.New()
	priceID := uuid.New()
	durationDays := 30

	product := &models.Product{
		ID:          productID,
		Slug:        "test_grandfather_" + uuid.New().String(),
		DisplayName: "Grandfather Test",
		Description: "Test",
		EntitlementsSpec: map[string]*int{
			"premium_grandfather": &durationDays,
		},
		Status:    models.CatalogStatusActive,
		CreatedAt: now,
		UpdatedAt: now,
	}
	price := &models.Price{
		ID:        priceID,
		ProductID: productID,
		Status:    models.CatalogStatusActive,
		Amount:    1500,
		Currency:  "USD",
		CreatedAt: now,
		UpdatedAt: now,
	}

	insertProductAndPrice(ctx, t, pool, product, price)

	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.entitlements WHERE merchant_subject_id = $1", tenantSubjectID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.payments WHERE merchant_subject_id = $1", tenantSubjectID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.prices WHERE id = $1", priceID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.products WHERE id = $1", productID)
	})

	fakeClock := clockwork.NewFakeClockAt(now)
	priceSvc := catalog.NewPriceService(dbi)
	productSvc := catalog.NewProductService(dbi)
	purchaseSvc := NewCheckoutPurchaseService(
		priceSvc,
		productSvc,
		payments.NewPaymentService(dbi, fakeClock),
		entitlements.NewEntitlementService(dbi, fakeClock),
		nil,
		fakeClock,
	)

	// Initial purchase while the price is active (establishes the subscriber).
	_, err := purchaseSvc.RegisterPurchase(ctx, &payments.RegisterPurchaseRequest{
		UserID:        userID,
		PriceID:       priceID,
		Processor:     string(models.ProcessorStripe),
		TransactionID: "grandfather_initial_" + uuid.New().String(),
		Amount:        price.Amount,
		Currency:      price.Currency,
	})
	require.NoError(t, err)

	// Archive the price (operator retires the plan).
	require.NoError(t, priceSvc.Deactivate(ctx, priceID))

	archived, err := priceSvc.GetByID(ctx, priceID)
	require.NoError(t, err)
	require.Equal(t, models.CatalogStatusArchived, archived.Status, "Deactivate must archive, not draft")
	require.False(t, archived.IsPurchasable(), "archived price must not be purchasable")
	require.True(t, archived.IsBillable(), "archived price must remain billable")
	require.Equal(t, int64(1500), archived.Amount, "renewal must still see the stored amount")

	// Renewal/rebill path: load-by-id is status-agnostic and bills the stored
	// amount. This MUST still succeed for the grandfathered subscriber.
	renewal, err := purchaseSvc.RegisterPurchase(ctx, &payments.RegisterPurchaseRequest{
		UserID:        userID,
		PriceID:       priceID,
		Processor:     string(models.ProcessorStripe),
		TransactionID: "grandfather_renewal_" + uuid.New().String(),
		Amount:        archived.Amount,
		Currency:      archived.Currency,
	})
	require.NoError(t, err, "archived price must keep billing existing subscriptions")
	require.NotEqual(t, uuid.Nil, renewal.PaymentID)

	// New-purchase gate: a brand-new customer cannot buy the archived price.
	elig, err := purchaseSvc.CheckPurchaseEligibility(ctx, uuid.New().String(), priceID)
	require.NoError(t, err)
	require.Equal(t, EligibilityBlocked, elig.Status, "new purchase of archived price must be blocked")
}

// insertProductAndPrice writes the catalog fixture rows through the generated
// queries (the model structs are plain data holders here).
func insertProductAndPrice(ctx context.Context, t *testing.T, qx gen.DBTX, product *models.Product, price *models.Price) {
	t.Helper()
	q := gen.New(qx)

	var entSpec []byte
	if product.EntitlementsSpec != nil {
		var err error
		entSpec, err = json.Marshal(product.EntitlementsSpec)
		require.NoError(t, err)
	}
	_, err := q.CreateProduct(ctx, gen.CreateProductParams{
		ID:               product.ID,
		Slug:             product.Slug,
		DisplayName:      product.DisplayName,
		Description:      &product.Description,
		EntitlementsSpec: entSpec,
		Status:           string(product.Status),
		CreatedAt:        product.CreatedAt,
		UpdatedAt:        product.UpdatedAt,
	})
	require.NoError(t, err)

	var cycleDays *int32
	if price.BillingCycleDays != nil {
		d := int32(*price.BillingCycleDays)
		cycleDays = &d
	}
	_, err = q.CreatePrice(ctx, gen.CreatePriceParams{
		ID:               price.ID,
		ProductID:        price.ProductID,
		Amount:           price.Amount,
		Currency:         price.Currency,
		Status:           string(price.Status),
		BillingCycleDays: cycleDays,
		CreatedAt:        price.CreatedAt,
		UpdatedAt:        price.UpdatedAt,
	})
	require.NoError(t, err)
}
