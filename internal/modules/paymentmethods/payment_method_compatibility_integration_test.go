//go:build integration

package paymentmethods

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
)

func TestListCompatibleByUserIDFiltersBeforePagination(t *testing.T) {
	ctx := dbtest.WithTestMerchant(context.Background())
	pool := dbtest.SharedPGXPool(t)
	merchantID := dbtest.TestMerchantID.UUID()
	database, err := db.NewWithPGXPool(pool, "")
	require.NoError(t, err)

	userID := uuid.NewString()
	customerID := dbtest.EnsureCustomerIDPgx(ctx, t, pool, userID)
	q := gen.New(pool)
	environment := "test"
	matchingPSP, err := q.UpsertPSP(ctx, gen.UpsertPSPParams{
		MerchantID:  merchantID,
		Rail:        string(models.RailNMI),
		Environment: &environment,
		AccountID:   "compatible-" + uuid.NewString(),
	})
	require.NoError(t, err)
	otherPSP, err := q.UpsertPSP(ctx, gen.UpsertPSPParams{
		MerchantID:  merchantID,
		Rail:        string(models.RailNMI),
		Environment: &environment,
		AccountID:   "incompatible-" + uuid.NewString(),
	})
	require.NoError(t, err)

	now := time.Now().UTC().Truncate(time.Second)
	repo := NewPaymentMethodRepo(database)
	newMethod := func(rail models.Rail, pspID *uuid.UUID, createdAt time.Time) *models.PaymentMethod {
		method := &models.PaymentMethod{
			ID:              uuid.New(),
			CustomerID:      customerID,
			Rail:            rail,
			PspID:           pspID,
			RailCustomerRef: "customer-" + uuid.NewString(),
			RailMethodRef:   "method-" + uuid.NewString(),
			CreatedAt:       createdAt,
			UpdatedAt:       createdAt,
		}
		require.NoError(t, repo.Create(ctx, method))
		return method
	}

	matchingNewest := newMethod(models.RailNMI, &matchingPSP.ID, now.Add(5*time.Second))
	wrongRail := newMethod(models.RailCCBill, &matchingPSP.ID, now.Add(4*time.Second))
	wrongPSP := newMethod(models.RailNMI, &otherPSP.ID, now.Add(3*time.Second))
	legacy := newMethod(models.RailNMI, nil, now.Add(2*time.Second))
	matchingOldest := newMethod(models.RailNMI, &matchingPSP.ID, now.Add(time.Second))

	productID := uuid.New()
	priceID := uuid.New()
	subscriptionID := uuid.New()
	productKey := "compatible-product-" + uuid.NewString()
	_, err = pool.Exec(ctx,
		`INSERT INTO openrails.products (id, merchant_id, key, display_name) VALUES ($1, $2, $3, $4)`,
		productID, merchantID, productKey, "Compatible Product")
	require.NoError(t, err)
	_, err = pool.Exec(ctx,
		`INSERT INTO openrails.prices (id, product_id, merchant_id, amount, currency) VALUES ($1, $2, $3, $4, $5)`,
		priceID, productID, merchantID, 1000, "USD")
	require.NoError(t, err)
	_, err = pool.Exec(ctx,
		`INSERT INTO openrails.subscriptions
		   (id, product_id, price_id, payment_method_id, rail, psp_id, merchant_id, customer_id)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		subscriptionID, productID, priceID, legacy.ID, string(models.RailNMI), matchingPSP.ID, merchantID, customerID)
	require.NoError(t, err)

	methodIDs := []uuid.UUID{matchingNewest.ID, wrongRail.ID, wrongPSP.ID, legacy.ID, matchingOldest.ID}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM openrails.subscriptions WHERE id = $1`, subscriptionID)
		_, _ = pool.Exec(ctx, `DELETE FROM openrails.payment_methods WHERE id = ANY($1)`, methodIDs)
		_, _ = pool.Exec(ctx, `DELETE FROM openrails.prices WHERE id = $1`, priceID)
		_, _ = pool.Exec(ctx, `DELETE FROM openrails.products WHERE id = $1`, productID)
		_, _ = pool.Exec(ctx, `DELETE FROM openrails.psps WHERE id = ANY($1)`, []uuid.UUID{matchingPSP.ID, otherPSP.ID})
	})

	service := NewPaymentMethodService(database)
	methods, total, err := service.ListCompatibleByUserID(
		ctx,
		userID,
		models.Rail(" NMI "),
		&matchingPSP.ID,
		1,
		1,
	)
	require.NoError(t, err)
	require.Equal(t, int64(3), total)
	require.Len(t, methods, 1)
	require.Equal(t, legacy.ID, methods[0].ID)
	require.Len(t, methods[0].Subscriptions, 1)
	require.Equal(t, subscriptionID, methods[0].Subscriptions[0].ID)
	require.NotNil(t, methods[0].Subscriptions[0].Product)
	require.Equal(t, "Compatible Product", methods[0].Subscriptions[0].Product.DisplayName)

	methods, total, err = service.ListCompatibleByUserID(
		ctx,
		userID,
		models.RailNMI,
		nil,
		10,
		0,
	)
	require.NoError(t, err)
	require.Equal(t, int64(4), total)
	require.Equal(t, []uuid.UUID{matchingNewest.ID, wrongPSP.ID, legacy.ID, matchingOldest.ID}, paymentMethodIDs(methods))
}

func paymentMethodIDs(methods []*models.PaymentMethod) []uuid.UUID {
	ids := make([]uuid.UUID, 0, len(methods))
	for _, method := range methods {
		ids = append(ids, method.ID)
	}
	return ids
}
