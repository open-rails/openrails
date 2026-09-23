//go:build integration

package embed_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	riverhelpers "github.com/open-rails/helpers/river"
	"github.com/riverqueue/river"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/embed"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/pkg/pricing"
)

// A host that owns River (or#914) inserts the engine's invoice job through the
// public args and the shared fleet works it: a usage-only payer (zero-amount
// catalog usage, no ledger row) is rated and invoiced by the period sweep,
// read back through the shared Client, and a second run changes nothing.
func TestInvoiceSweepArgs_HostOwnedRiverRunsThePeriodSweep(t *testing.T) {
	ctx := context.Background()
	dsn := dbtest.SharedPostgresDSN(t)
	pool, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	dbtest.EnsureTestMerchant(ctx, t, pool)

	rt, err := embed.New(ctx, embed.Options{
		Config: &config.Config{Encryption: &config.EncryptionConfig{MasterKey: "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8="}, TestMode: config.CredentialPostureSandbox, MerchantConfigHTTP: true, AllowCatalogUpdates: true, SecretBackend: config.SecretBackendDB, DB: &config.DBConfig{URL: dsn}},
		River:  embed.RiverFromHost(),
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = rt.Close(context.Background()) })
	jobs, err := riverhelpers.New(ctx, pool, &river.Config{Queues: map[string]river.QueueConfig{embed.QueueBilling: {MaxWorkers: 1}}}, rt.RiverJobs())
	require.NoError(t, err)
	require.NoError(t, jobs.Start(ctx))
	t.Cleanup(func() { _ = jobs.Stop(context.Background()) })

	client, err := rt.Client(openrails.WithMerchantID(dbtest.TestMerchantID))
	require.NoError(t, err)

	// Calendar-month periods so the previous period is exact; restore after.
	settings, err := client.GetMerchantSettings(ctx)
	require.NoError(t, err)
	previous := *settings
	t.Cleanup(func() { _ = client.SetMerchantSettings(context.Background(), previous) })
	settings.InvoiceBillingBoundary = "calendar_month"
	require.NoError(t, client.SetMerchantSettings(ctx, *settings))

	suffix := uuid.NewString()[:8]
	meter := "payment-volume-" + suffix
	eventType := "platform.payment_volume." + suffix
	productID, err := client.Products.Ensure(ctx, &openrails.ProductCreateParams{Key: "volume-" + suffix, DisplayName: "Payment volume"})
	require.NoError(t, err)
	require.NoError(t, client.EnsureUsageMeter(ctx, openrails.UsageMeterSpec{
		Key: meter, EventType: eventType, ValueProperty: "amount_micros", Aggregation: "sum", Unit: "currency_micros",
	}))
	_, err = client.SetDefaultUsageRateCard(ctx, meter, openrails.DefaultUsageRateCardRequest{
		ProductID: (sdkProductID(t, productID.ID)).String(),
		Price: pricing.RatePrice{Model: pricing.ModelPerUnit, Currency: "USD",
			PerUnit: &pricing.PerUnitPrice{UnitAmount: 100, DivideBy: 10_000}},
	})
	require.NoError(t, err)

	now := time.Now().UTC()
	periodTo := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	periodFrom := periodTo.AddDate(0, -1, 0)
	payer := uuid.New()
	const settled = int64(17_000_000)
	for _, occurred := range []time.Time{periodFrom.Add(3 * 24 * time.Hour), periodTo.Add(-time.Hour)} {
		occurred := occurred
		require.NoError(t, client.RecordUsage(ctx, openrails.UsageReport{
			CustomerID: (openrails.CustomerID(payer)).String(), Invoker: payer.String(), Currency: "USD", EventType: eventType,
			Dimensions: map[string]int64{"amount_micros": settled},
			Source:     "host-settlement", SourceID: uuid.NewString(), OccurredAt: &occurred,
		}))
	}
	// Ledger reads go through a merchant-pinned connection: the tables force RLS.
	ledger := dbtest.SharedMerchantPool(t, dbtest.TestMerchantID.UUID())
	var ledgerRows int
	require.NoError(t, ledger.QueryRow(ctx, `SELECT count(*) FROM billing.ledger_transfers WHERE customer_id = $1`, payer).Scan(&ledgerRows))
	require.Zero(t, ledgerRows, "precondition: the payer has no ledger row before rating")

	sweep := func() {
		t.Helper()
		res, err := jobs.Insert(ctx, embed.InvoiceSweepArgs{FinalizePreviousMonth: true}, nil)
		require.NoError(t, err)
		require.Equal(t, embed.QueueBilling, res.Job.Queue)
		require.Eventually(t, func() bool {
			var state string
			return pool.QueryRow(ctx, `SELECT state FROM river_job WHERE id = $1`, res.Job.ID).Scan(&state) == nil && state == "completed"
		}, 60*time.Second, 200*time.Millisecond, "the host-inserted invoice sweep must be worked by the shared fleet")
	}
	listInvoices := func() []openrails.MerchantInvoiceDTO {
		t.Helper()
		invoices, total, err := client.ListMerchantInvoices(ctx, openrails.MerchantInvoiceFilter{CustomerID: (openrails.CustomerID(payer)).String()}, 10, 0)
		require.NoError(t, err)
		require.EqualValues(t, len(invoices), total)
		return invoices
	}
	require.Empty(t, listInvoices())

	sweep()
	invoices := listInvoices()
	require.Len(t, invoices, 1, "the period sweep must invoice a usage-only payer")
	inv := invoices[0]
	// 2 x 17,000,000 micros x 100 / 10,000 = 340,000 micros, exact.
	const fee = int64(340_000)
	require.Equal(t, "open", inv.Status)
	require.Equal(t, fee, inv.AmountDue)
	require.Equal(t, fee, inv.TotalAmount)
	require.True(t, inv.PeriodFrom.Equal(periodFrom) && inv.PeriodTo.Equal(periodTo), "period %v..%v", inv.PeriodFrom, inv.PeriodTo)
	var rated *openrails.InvoiceLineItemDTO
	for i := range inv.LineItems {
		if inv.LineItems[i].EventType == "metered:"+meter {
			rated = &inv.LineItems[i]
		}
	}
	require.NotNil(t, rated, "rated fee line item in %+v", inv.LineItems)
	require.Equal(t, fee, rated.Amount)

	sweep()
	again := listInvoices()
	require.Len(t, again, 1)
	require.Equal(t, inv.ID, again[0].ID)
	require.Equal(t, fee, again[0].AmountDue)
	var accrued int64
	require.NoError(t, ledger.QueryRow(ctx, `SELECT COALESCE(SUM(amount), 0) FROM billing.ledger_transfers
		WHERE customer_id = $1 AND transfer_type = 'owed_accrual'`, payer).Scan(&accrued))
	require.Equal(t, fee, accrued, "exactly-once rating across host-inserted sweeps")
}
