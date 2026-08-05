//go:build integration

package webhooks

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/stretchr/testify/require"
)

// TestStripeInvoicePaymentAlreadyRecorded covers the cross-key dedup, ported to
// the #684 fetch-and-converge creation leg: the reconcile backfill records a
// subscription payment keyed by the CHARGE id (with stripe_invoice_id in
// metadata), while a fetched latest invoice without charge/payment_intent keys
// the SAME payment by the INVOICE id. The (merchant, rail, transaction_id)
// unique index cannot see across those keys, so without this helper a
// backfill-then-converge ordering inserts a duplicate row.
func TestStripeInvoicePaymentAlreadyRecorded(t *testing.T) {

	ctx := dbtest.WithTestMerchant(context.Background())
	dbi := dbtest.OpenMerchantDB(t, dbtest.TestMerchantID.UUID())
	pool := dbi.Pool()
	q := gen.New(pool)

	now := time.Date(2026, 6, 4, 0, 0, 0, 0, time.UTC)
	userID := uuid.New().String()
	tenantSubjectID := dbtest.EnsureCustomerIDPgx(ctx, t, pool, userID)
	productID := uuid.New()
	priceID := uuid.New()
	billingDays := int32(30)

	entitlementsSpecJSON := []byte(`{"premium":null}`)
	description := "Test"
	_, err := q.CreateProduct(ctx, gen.CreateProductParams{
		MerchantID:       dbtest.TestMerchantID.UUID(),
		ID:               productID,
		Key:              "dedup_product_" + uuid.New().String(),
		DisplayName:      "Dedup Product",
		Description:      &description,
		EntitlementsSpec: entitlementsSpecJSON,
		Archived:         false,
		CreatedAt:        now,
		UpdatedAt:        now,
	})
	require.NoError(t, err)

	_, err = q.CreatePrice(ctx, gen.CreatePriceParams{
		MerchantID:          dbtest.TestMerchantID.UUID(),
		ID:                  priceID,
		ProductID:           productID,
		Amount:              2900,
		Currency:            "USD",
		Archived:            false,
		AccessDurationHours: &billingDays,
		AutoRenew:           true,
		CreatedAt:           now,
		UpdatedAt:           now,
	})
	require.NoError(t, err)

	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.payments WHERE customer_id = $1", tenantSubjectID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.prices WHERE id = $1", priceID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.products WHERE id = $1", productID)
	})

	paymentSvc := payments.NewPaymentService(dbi)
	svc := &StripeConvergeService{PaymentService: paymentSvc}
	pspID := dbtest.EnsureTestPSP(ctx, t, pool, dbtest.TestMerchantID.UUID(), string(models.RailStripe))

	// Reconcile-backfill row: keyed by CHARGE id, invoice id only in metadata.
	const chargeID = "ch_dedup_1"
	const invoiceID = "in_dedup_1"
	require.NoError(t, paymentSvc.Create(ctx, &models.Payment{
		ID:            uuid.New(),
		CustomerID:    tenantSubjectID,
		PriceID:       priceID,
		Rail:          models.RailStripe,
		PspID:         &pspID,
		TransactionID: chargeID,
		Amount:        2900,
		ListAmount:    2900,
		Currency:      "USD",
		Status:        payments.PaymentStatusCompletedValue,
		MoneyMovement: models.MoneyMovementRail,
		PurchasedAt:   now,
		CreatedAt:     now,
		Metadata: map[string]any{
			"source":            "stripe_reconcile_backfill",
			"stripe_charge_id":  chargeID,
			"stripe_invoice_id": invoiceID,
		},
	}))

	// Fetched invoice without charge/payment_intent (transaction id falls back
	// to the invoice id): must dedupe via the backfill row's stripe_invoice_id.
	recorded, err := svc.fetchedInvoicePaymentAlreadyRecorded(ctx, subscriptions.StripeLivenessRecord{
		LatestInvoiceID: invoiceID, LatestInvoiceTransactionID: invoiceID,
	})
	require.NoError(t, err)
	require.True(t, recorded, "fetched invoice must match backfill row via stripe_invoice_id metadata")

	// Fetched invoice carrying the charge: matches by transaction id.
	recorded, err = svc.fetchedInvoicePaymentAlreadyRecorded(ctx, subscriptions.StripeLivenessRecord{
		LatestInvoiceID: invoiceID, LatestInvoiceTransactionID: chargeID,
	})
	require.NoError(t, err)
	require.True(t, recorded, "fetched invoice carrying the charge id must match by transaction id")

	// Unrelated invoice: no match.
	recorded, err = svc.fetchedInvoicePaymentAlreadyRecorded(ctx, subscriptions.StripeLivenessRecord{
		LatestInvoiceID: "in_unrelated", LatestInvoiceTransactionID: "ch_unrelated",
	})
	require.NoError(t, err)
	require.False(t, recorded, "unrelated invoice must not match")

	// A prior FAILED attempt for an invoice must not suppress recording the
	// eventual success: failed rows are "failed:"-prefixed with no invoice
	// metadata, so they never match.
	require.NoError(t, paymentSvc.Create(ctx, &models.Payment{
		ID:            uuid.New(),
		CustomerID:    tenantSubjectID,
		PriceID:       priceID,
		Rail:          models.RailStripe,
		PspID:         &pspID,
		TransactionID: "failed:in_dedup_2",
		Amount:        2900,
		ListAmount:    2900,
		Currency:      "USD",
		Status:        payments.PaymentStatusFailedValue,
		PurchasedAt:   now,
		CreatedAt:     now,
	}))
	recorded, err = svc.fetchedInvoicePaymentAlreadyRecorded(ctx, subscriptions.StripeLivenessRecord{
		LatestInvoiceID: "in_dedup_2", LatestInvoiceTransactionID: "in_dedup_2",
	})
	require.NoError(t, err)
	require.False(t, recorded, "a prior failed attempt must not suppress recording the success")
}
