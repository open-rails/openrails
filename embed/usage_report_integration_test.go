//go:build integration

package embed_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/embed"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/integrationharness"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/pkg/identity"
	"github.com/open-rails/openrails/pkg/pricing"
	"github.com/stretchr/testify/require"
)

// TestRecordUsage_UnifiedClient_RatesIntoInvoice proves #797 end to end on
// BOTH transports: host-reported usage lands in openrails.usage_events through
// the unified client (idempotent on source+source_id), and the pkg/service
// FinalizeInvoice export rates it through a gauge rate card into an invoice.
func TestRecordUsage_UnifiedClient_RatesIntoInvoice(t *testing.T) {
	ctx := context.Background()
	currency := money.DefaultCurrency

	h := integrationharness.New(t, ctx)
	embedded := h.StartEmbeddedHost(currency)
	standalone := h.StartStandalone(currency)
	pool := h.Pool()

	embeddedClient := embedded.Runtime().Client(embed.WithCurrency(currency))
	standaloneClient := standalone.Client()

	merchantID := dbtest.TestMerchantID.UUID()
	meterKey := "gb-seconds-" + uuid.NewString()
	productID := uuid.New()
	payerID := uuid.New()
	payer := identity.CustomerID(payerID)
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.usage_events WHERE customer_id = $1", payerID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.invoice_items WHERE customer_id = $1", payerID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.invoices WHERE customer_id = $1", payerID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.catalog_rate_cards WHERE merchant_id = $1 AND product_id = $2", merchantID, productID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.catalog_meters WHERE key = $1", meterKey)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.products WHERE id = $1", productID)
	})

	// Arrears so rated usage becomes an open receivable.
	svc := embedded.Runtime().Service()
	mode := money.BillingModeArrears
	require.NoError(t, svc.SetCreditAccountSettings(dbtest.WithTestMerchant(ctx), payer, currency,
		money.AccountSettingsInput{BillingMode: &mode}))

	from := time.Now().Add(-time.Hour)
	to := time.Now().Add(time.Hour)
	occurred := time.Now().UTC().Truncate(time.Second)

	// Two gauge segment events via the EMBEDDED unified client; the first is
	// replayed and must not double-record.
	report := openrails.UsageReport{
		CustomerID:     payerID.String(),
		Invoker:        "usage-report-test",
		Currency:       currency,
		EventType:      meterKey,
		Dimensions:     map[string]int64{meterKey: 1800},
		Amount:         0,
		Source:         "usage-report-test",
		SourceID:       uuid.NewString(),
		OccurredAtUnix: occurred.Unix(),
	}
	require.NoError(t, embeddedClient.RecordUsage(ctx, report))
	require.NoError(t, embeddedClient.RecordUsage(ctx, report), "idempotent replay must succeed")
	second := report
	second.SourceID = uuid.NewString()
	second.Dimensions = map[string]int64{meterKey: 5400}
	require.NoError(t, embeddedClient.RecordUsage(ctx, second))

	// One more via the STANDALONE client (same wire, real HTTP + AuthKit).
	third := report
	third.SourceID = uuid.NewString()
	third.Dimensions = map[string]int64{meterKey: 3600}
	require.NoError(t, standaloneClient.RecordUsage(ctx, third))

	var eventCount int
	var totalUnits int64
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT count(*), COALESCE(SUM((dimensions ->> $2)::bigint), 0)
		FROM openrails.usage_events
		WHERE customer_id = $1 AND event_type = $2`, payerID, meterKey).Scan(&eventCount, &totalUnits))
	require.Equal(t, 3, eventCount, "replay must not create a fourth event")
	require.Equal(t, int64(1800+5400+3600), totalUnits)

	// Gauge rate card: 500_000 micros per 3600 unit-seconds.
	rateMicros, divideBy := int64(500_000), int64(3600)
	_, err := pool.Exec(ctx, `INSERT INTO openrails.products (id, key, display_name, merchant_id) VALUES ($1, $2, $3, $4)`,
		productID, "usage-report-"+uuid.NewString(), "Usage Report Product", merchantID)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `INSERT INTO openrails.catalog_meters (merchant_id, key, kind) VALUES ($1, $2, 'gauge')`,
		merchantID, meterKey)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `
INSERT INTO openrails.catalog_rate_cards (merchant_id, product_id, ordinal, meter_key, payment_term, price)
VALUES ($1, $2, 1, $3, 'in_arrears', jsonb_build_object(
    'model', 'per_unit',
    'currency', 'usd',
    'per_unit', jsonb_build_object('unit_amount', $4::bigint, 'divide_by', $5::bigint)))`,
		merchantID, productID, meterKey, rateMicros, divideBy)
	require.NoError(t, err)

	expected, err := pricing.ChargeModel{Kind: pricing.ModelPerUnit, UnitAmount: rateMicros, DivideBy: divideBy}.Rate(totalUnits)
	require.NoError(t, err)
	require.Greater(t, expected, int64(0))

	inv, err := svc.FinalizeInvoice(dbtest.WithTestMerchant(ctx), payer, currency, from, to)
	require.NoError(t, err)
	require.Equal(t, "open", inv.Status)
	require.Equal(t, expected, inv.AmountDue, "invoice must equal reported unit-seconds x rate")

	// Re-finalize the same window: idempotent (per-period watermark).
	inv2, err := svc.FinalizeInvoice(dbtest.WithTestMerchant(ctx), payer, currency, from, to)
	require.NoError(t, err)
	require.Equal(t, inv.ID, inv2.ID)
	require.Equal(t, expected, inv2.AmountDue)
}
