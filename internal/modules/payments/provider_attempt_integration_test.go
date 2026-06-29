//go:build integration

package payments

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/stretchr/testify/require"
)

// The NMI subscription checkout records its real charge as a separate payment
// row, so its attempt anchor is completed "in place" (metadata + status); it
// must not linger as a forever-pending payment.
func TestCompleteProviderAttemptInPlace_ResolvesStatus(t *testing.T) {
	dsn := dbtest.SharedPostgresDSN(t)

	ctx := dbtest.WithTestMerchant(context.Background())
	dbi := dbtest.OpenAppDB(t, dsn)
	pool := dbi.Pool()
	q := gen.New(pool)
	svc := NewPaymentService(dbi)

	now := time.Now().UTC().Truncate(time.Second)
	userID := uuid.New().String()
	tenantSubjectID := dbtest.EnsureCustomerIDPgx(ctx, t, pool, userID)
	productID := uuid.New()
	priceID := uuid.New()

	description := "Provider attempt test product"
	_, err := q.CreateProduct(ctx, gen.CreateProductParams{
		ID:          productID,
		MerchantID:  dbtest.TestMerchantID.UUID(),
		Key:         "provider_attempt_" + uuid.New().String(),
		DisplayName: "Provider Attempt Test",
		Description: &description,
		Status:      string(models.CatalogStatusActive),
		CreatedAt:   now,
		UpdatedAt:   now,
	})
	require.NoError(t, err)
	billingCycleDays := int32(30)
	_, err = q.CreatePrice(ctx, gen.CreatePriceParams{
		ID:                  priceID,
		MerchantID:          dbtest.TestMerchantID.UUID(),
		ProductID:           productID,
		Amount:              2300,
		Currency:            "USD",
		Status:              string(models.CatalogStatusActive),
		AccessDurationHours: &billingCycleDays,
		AutoRenew:           true,
		CreatedAt:           now,
		UpdatedAt:           now,
	})
	require.NoError(t, err)

	t.Cleanup(func() {
		cctx := context.Background()
		_, _ = pool.Exec(cctx, "DELETE FROM openrails.payments WHERE customer_id = $1", tenantSubjectID)
		_, _ = pool.Exec(cctx, "DELETE FROM openrails.prices WHERE id = $1", priceID)
		_, _ = pool.Exec(cctx, "DELETE FROM openrails.products WHERE id = $1", productID)
	})

	reserve := func(orderID string) *models.Payment {
		attempt, err := svc.ReserveProviderAttempt(ctx, &models.Payment{
			CustomerID:    tenantSubjectID,
			PriceID:       priceID,
			Rail:          models.RailNMI,
			TransactionID: "nmi_sub_attempt:" + orderID,
			Amount:        2300,
			ListAmount:    2300,
			Currency:      "USD",
			Status:        PaymentStatusPendingValue,
			Metadata:      map[string]any{"nmi_subscription_order_id": orderID, "nmi_attempt_status": "pending"},
		})
		require.NoError(t, err)
		require.Equal(t, PaymentStatusPendingValue, attempt.Status)
		return attempt
	}

	// Success path: completing in place resolves the status column too.
	success := reserve("order_success_" + uuid.New().String())
	completed, err := svc.CompleteProviderAttemptInPlace(ctx, success.ID, map[string]any{"nmi_attempt_status": "completed"})
	require.NoError(t, err)
	require.Equal(t, PaymentStatusCompletedValue, completed.Status)
	// The synthetic transaction reference stays (the real charge is its own
	// payment row), which keeps the row excluded from refunds.
	require.Equal(t, success.TransactionID, completed.TransactionID)
	require.Error(t, svc.ValidateRefund(ctx, completed, 100))

	// Completing the same attempt twice must not silently succeed.
	_, err = svc.CompleteProviderAttemptInPlace(ctx, success.ID, map[string]any{"nmi_attempt_status": "completed"})
	require.Error(t, err)

	// Failure path keeps marking the attempt failed.
	failure := reserve("order_failure_" + uuid.New().String())
	require.NoError(t, svc.MarkFailed(ctx, failure.ID))
	failed, err := svc.GetByID(ctx, failure.ID)
	require.NoError(t, err)
	require.Equal(t, PaymentStatusFailedValue, failed.Status)
}
