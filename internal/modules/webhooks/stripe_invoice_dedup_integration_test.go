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
	"github.com/stretchr/testify/require"
)

// TestStripeInvoicePaymentAlreadyRecorded covers the Bug 2 cross-key dedup: the
// reconcile backfill records a subscription payment keyed by the CHARGE id (with
// stripe_invoice_id in metadata), while the 2026-04-22.preview invoice.paid
// webhook keys the SAME payment by the INVOICE id. The (merchant, rail,
// transaction_id) unique index cannot see across those keys, so without this
// helper a backfill-then-webhook ordering inserts a duplicate row.
func TestStripeInvoicePaymentAlreadyRecorded(t *testing.T) {
	dsn := dbtest.SharedPostgresDSN(t)

	ctx := dbtest.WithTestMerchant(context.Background())
	dbi := dbtest.OpenAppDB(t, dsn)
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
		Slug:             "dedup_product_" + uuid.New().String(),
		DisplayName:      "Dedup Product",
		Description:      &description,
		EntitlementsSpec: entitlementsSpecJSON,
		Status:           string(models.CatalogStatusActive),
		CreatedAt:        now,
		UpdatedAt:        now,
	})
	require.NoError(t, err)

	_, err = q.CreatePrice(ctx, gen.CreatePriceParams{
		MerchantID:       dbtest.TestMerchantID.UUID(),
		ID:               priceID,
		ProductID:        productID,
		Amount:           2900,
		Currency:         "usd",
		Status:           string(models.CatalogStatusActive),
		BillingCycleDays: &billingDays,
		CreatedAt:        now,
		UpdatedAt:        now,
	})
	require.NoError(t, err)

	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.payments WHERE customer_id = $1", tenantSubjectID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.prices WHERE id = $1", priceID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.products WHERE id = $1", productID)
	})

	paymentSvc := payments.NewPaymentService(dbi)
	svc := &StripeWebhookService{PaymentService: paymentSvc}

	// Reconcile-backfill row: keyed by CHARGE id, invoice id only in metadata.
	const chargeID = "ch_dedup_1"
	const invoiceID = "in_dedup_1"
	require.NoError(t, paymentSvc.Create(ctx, &models.Payment{
		ID:            uuid.New(),
		CustomerID:    tenantSubjectID,
		PriceID:       priceID,
		Rail:          models.RailStripe,
		TransactionID: chargeID,
		Amount:        2900,
		ListAmount:    2900,
		Currency:      "usd",
		Status:        payments.PaymentStatusCompletedValue,
		PurchasedAt:   now,
		CreatedAt:     now,
		Metadata: map[string]any{
			"source":            "stripe_reconcile_backfill",
			"stripe_charge_id":  chargeID,
			"stripe_invoice_id": invoiceID,
		},
	}))

	// Preview invoice.paid (no charge/payment_intent): must dedupe via the
	// stripe_invoice_id metadata of the backfill row.
	recorded, err := svc.stripeInvoicePaymentAlreadyRecorded(ctx, stripeInvoice{ID: invoiceID})
	require.NoError(t, err)
	require.True(t, recorded, "preview invoice must match backfill row via stripe_invoice_id metadata")

	// Non-preview invoice.paid that carries the charge: matches by transaction id.
	recorded, err = svc.stripeInvoicePaymentAlreadyRecorded(ctx, stripeInvoice{ID: invoiceID, Charge: chargeID})
	require.NoError(t, err)
	require.True(t, recorded, "invoice carrying the charge id must match by transaction id")

	// Unrelated invoice: no match.
	recorded, err = svc.stripeInvoicePaymentAlreadyRecorded(ctx, stripeInvoice{ID: "in_unrelated", Charge: "ch_unrelated"})
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
		TransactionID: "failed:in_dedup_2",
		Amount:        2900,
		ListAmount:    2900,
		Currency:      "usd",
		Status:        payments.PaymentStatusFailedValue,
		PurchasedAt:   now,
		CreatedAt:     now,
	}))
	recorded, err = svc.stripeInvoicePaymentAlreadyRecorded(ctx, stripeInvoice{ID: "in_dedup_2"})
	require.NoError(t, err)
	require.False(t, recorded, "a prior failed attempt must not suppress recording the success")
}
