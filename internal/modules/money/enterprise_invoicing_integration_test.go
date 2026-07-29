//go:build integration

// #798 enterprise arrears invoicing: negotiated per-payer rate cards,
// net-N terms + invoice document snapshot, send_invoice collection skip,
// past-due marking, out-of-band settlement, sweep + pending charges.
package money_test

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/pkg/identity"
	"github.com/open-rails/openrails/pkg/pricing"
	"github.com/stretchr/testify/require"
)

const gbMonthDivisor = int64(1024 * 30 * 24 * 3600) // MiB-seconds per GB-month

func TestEnterpriseInvoicing_PayerRateCardOverride(t *testing.T) {
	svc, pool, payer, cur, ctx := moneyInEnv(t)
	merchantID := dbtest.TestMerchantID.UUID()
	negotiated := identity.CustomerIDFromString(uuid.NewString())
	productID := uuid.New()
	meter := "storage-gb-" + uuid.NewString()[:8]
	eventType := "storage.repo_gb." + uuid.NewString()[:8]
	t.Cleanup(func() {
		for _, p := range []uuid.UUID{payer.UUID(), negotiated.UUID()} {
			_, _ = pool.Exec(ctx, "DELETE FROM openrails.usage_events WHERE customer_id = $1", p)
			_, _ = pool.Exec(ctx, "DELETE FROM openrails.invoice_items WHERE customer_id = $1", p)
			_, _ = pool.Exec(ctx, "DELETE FROM openrails.invoices WHERE customer_id = $1", p)
			_, _ = pool.Exec(ctx, "DELETE FROM openrails.metered_rating_watermarks WHERE customer_id = $1", p)
			_, _ = pool.Exec(ctx, "DELETE FROM openrails.money_settings WHERE customer_id = $1", p)
		}
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.catalog_rate_cards WHERE merchant_id = $1 AND meter_key = $2", merchantID, meter)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.catalog_meters WHERE merchant_id = $1 AND key = $2", merchantID, meter)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.products WHERE id = $1", productID)
	})

	_, err := pool.Exec(ctx, `INSERT INTO openrails.products (id, key, display_name, merchant_id) VALUES ($1, $2, 'Storage', $3)`,
		productID, "storage-"+uuid.NewString()[:8], merchantID)
	require.NoError(t, err)

	// Host-owned meter + merchant-default card ($0.02/GB-month) via the #798 seams.
	require.NoError(t, svc.EnsureUsageMeter(ctx, money.UsageMeterSpec{
		Key: meter, EventType: eventType, ValueProperty: "mib_seconds", Aggregation: "sum", Unit: "mib_second",
	}))
	require.NoError(t, svc.SetUsageRateCard(ctx, money.UsageRateCardInput{
		ProductID: &productID,
		MeterKey:  meter,
		Price: pricing.RatePrice{Model: "per_unit", Currency: "USD",
			PerUnit: &pricing.PerUnitPrice{UnitAmount: 20_000, DivideBy: gbMonthDivisor}},
	}))
	// Negotiated override for the second payer: half price.
	require.NoError(t, svc.SetUsageRateCard(ctx, money.UsageRateCardInput{
		Payer:    &negotiated,
		MeterKey: meter,
		Price: pricing.RatePrice{Model: "per_unit", Currency: "USD",
			PerUnit: &pricing.PerUnitPrice{UnitAmount: 10_000, DivideBy: gbMonthDivisor}},
	}))

	// Identical usage for both payers: 5 GB-months of MiB-seconds.
	quantity := 5 * gbMonthDivisor
	from := time.Now().Add(-time.Hour)
	to := time.Now().Add(time.Hour)
	for _, p := range []identity.CustomerID{payer, negotiated} {
		pp := p
		_, err = svc.UpsertAccountSettings(ctx, pp, money.DefaultCurrency, money.AccountSettingsInput{
			BillingMode: strptr(money.BillingModeArrears),
		})
		require.NoError(t, err)
		_, err = svc.RecordUsage(ctx, money.RecordUsageParams{
			Payer: &pp, Invoker: pp.UUID().String(), Currency: cur, EventType: eventType,
			Dimensions: map[string]int64{"mib_seconds": quantity},
			Amount:     0, Source: "th798-test", SourceID: uuid.NewString(), OccurredAt: time.Now(),
		})
		require.NoError(t, err)
	}

	invDefault, err := svc.FinalizeInvoice(ctx, payer, cur, from, to)
	require.NoError(t, err)
	invNegotiated, err := svc.FinalizeInvoice(ctx, negotiated, cur, from, to)
	require.NoError(t, err)

	// 5 GB-month x $0.02 = $0.10 = 100_000 micros; negotiated = half.
	require.Equal(t, int64(100_000), invDefault.AmountDue, "default rate card rates the default payer")
	require.Equal(t, int64(50_000), invNegotiated.AmountDue, "negotiated per-payer card replaces the default")

	// The statement itemizes the rated charge per meter source (#798).
	foundRated := false
	for _, li := range invNegotiated.LineItems {
		if li.EventType == "metered:"+meter {
			require.Equal(t, int64(50_000), li.Amount)
			foundRated = true
		}
	}
	require.True(t, foundRated, "rated per-category line item present")

	// Dropping the override restores the default for future periods.
	require.NoError(t, svc.DeletePayerRateCard(ctx, negotiated, meter))
}

func TestEnterpriseInvoicing_NetTermsDocumentSnapshotAndDunning(t *testing.T) {
	svc, pool, payer, cur, ctx := moneyInEnv(t)
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.invoice_payments WHERE customer_id = $1", payer.UUID())
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.invoice_items WHERE customer_id = $1", payer.UUID())
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.invoices WHERE customer_id = $1", payer.UUID())
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.customer_invoice_profiles WHERE customer_id = $1", payer.UUID())
	})

	// Terms account: net-30 manual remittance with full document fields.
	require.NoError(t, svc.SetCustomerInvoiceProfile(ctx, payer, money.CustomerInvoiceProfile{
		NetTermsDays:     30,
		CollectionMethod: money.CollectionSendInvoice,
		PONumber:         "PO-778899",
		Tax:              map[string]any{"tax_id": "DE811223344", "scheme": "eu_vat"},
		BillingContacts:  []models.InvoiceContact{{Name: "AP Desk", Email: "ap@acme.example"}},
		Memo:             "net-30 terms per MSA",
	}))
	got, err := svc.GetCustomerInvoiceProfile(ctx, payer)
	require.NoError(t, err)
	require.Equal(t, 30, got.NetTermsDays)
	require.Equal(t, money.CollectionSendInvoice, got.CollectionMethod)

	// Saved payment method on file — the send_invoice skip below must be the
	// collection_method, not a missing method.
	pm := seedPaymentMethodWithRailCustomerRef(t, pool, ctx, payer, string(models.RailStripe), "pm_terms_"+uuid.NewString()[:8])
	_, err = svc.UpsertAccountSettings(ctx, payer, money.DefaultCurrency, money.AccountSettingsInput{
		BillingMode: strptr(money.BillingModeArrears), AutoTopupPaymentMethod: &pm,
	})
	require.NoError(t, err)

	_, err = svc.AccrueOwed(ctx, payer, money.DefaultCurrency, "usage", "th798-terms-"+uuid.NewString()[:8], 250_000)
	require.NoError(t, err)

	from := time.Now().Add(-time.Hour)
	to := time.Now().Add(time.Hour)
	inv, err := svc.FinalizeInvoice(ctx, payer, cur, from, to)
	require.NoError(t, err)

	// Net-30 due date + document snapshot on the receivable.
	require.Equal(t, "open", inv.Status)
	require.Equal(t, money.CollectionSendInvoice, inv.CollectionMethod)
	require.NotNil(t, inv.DueAt)
	require.NotNil(t, inv.IssuedAt)
	require.InDelta(t, float64(30*24*time.Hour), float64(inv.DueAt.Sub(*inv.IssuedAt)), float64(time.Minute))
	require.NotNil(t, inv.PONumber)
	require.Equal(t, "PO-778899", *inv.PONumber)
	require.Equal(t, "DE811223344", inv.Tax["tax_id"])
	require.Len(t, inv.BillingContacts, 1)
	require.Equal(t, "ap@acme.example", inv.BillingContacts[0].Email)
	require.NotNil(t, inv.Memo)

	// Collection NEVER charges a send_invoice receivable.
	charger := &fakeCharger{}
	n, err := svc.ChargeOutstanding(ctx, charger, 0)
	require.NoError(t, err)
	require.Zero(t, n)
	require.Empty(t, charger.charges, "manual-remittance invoice must not be auto-charged")

	// Not yet due: past-due marking is a no-op for this invoice.
	flipped, err := svc.MarkInvoicesPastDue(ctx, time.Now())
	require.NoError(t, err)
	cur1, err := svc.GetInvoiceByID(ctx, payer, inv.ID)
	require.NoError(t, err)
	require.Equal(t, "open", cur1.Status)

	// Past the due date the receivable flips to past_due (the dunning signal).
	_, err = pool.Exec(ctx, "UPDATE openrails.invoices SET due_at = now() - interval '1 day' WHERE id = $1", inv.ID)
	require.NoError(t, err)
	flipped, err = svc.MarkInvoicesPastDue(ctx, time.Now())
	require.NoError(t, err)
	require.GreaterOrEqual(t, flipped, 1)
	cur2, err := svc.GetInvoiceByID(ctx, payer, inv.ID)
	require.NoError(t, err)
	require.Equal(t, "past_due", cur2.Status)

	// Manual remittance settles it: exposure returns to zero.
	paid, err := svc.RecordOutOfBandInvoicePayment(ctx, payer, inv.ID, 250_000, "wire-th798-"+uuid.NewString()[:8])
	require.NoError(t, err)
	require.Equal(t, "paid", paid.Status)
	require.Equal(t, int64(0), paid.AmountDue)
	outstanding, err := svc.GetOutstandingOwed(ctx, payer, cur)
	require.NoError(t, err)
	require.Zero(t, outstanding)
}

func TestEnterpriseInvoicing_EnsureCustomerInvoiceProfileDoesNotOverwrite(t *testing.T) {
	svc, pool, payer, _, ctx := moneyInEnv(t)
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.customer_invoice_profiles WHERE customer_id = $1", payer.UUID())
	})

	created, err := svc.EnsureCustomerInvoiceProfile(ctx, payer, money.CustomerInvoiceProfile{
		CollectionMethod: money.CollectionChargeAutomatically,
	})
	require.NoError(t, err)
	require.True(t, created)

	require.NoError(t, svc.SetCustomerInvoiceProfile(ctx, payer, money.CustomerInvoiceProfile{
		NetTermsDays:     30,
		CollectionMethod: money.CollectionSendInvoice,
		Memo:             "operator terms",
	}))

	created, err = svc.EnsureCustomerInvoiceProfile(ctx, payer, money.CustomerInvoiceProfile{
		CollectionMethod: money.CollectionChargeAutomatically,
	})
	require.NoError(t, err)
	require.False(t, created)

	got, err := svc.GetCustomerInvoiceProfile(ctx, payer)
	require.NoError(t, err)
	require.Equal(t, 30, got.NetTermsDays)
	require.Equal(t, money.CollectionSendInvoice, got.CollectionMethod)
	require.Equal(t, "operator terms", got.Memo)

	_, err = pool.Exec(ctx, "DELETE FROM openrails.customer_invoice_profiles WHERE customer_id = $1", payer.UUID())
	require.NoError(t, err)

	start := make(chan struct{})
	errs := make(chan error, 2)
	go func() {
		<-start
		_, err := svc.EnsureCustomerInvoiceProfile(ctx, payer, money.CustomerInvoiceProfile{
			CollectionMethod: money.CollectionChargeAutomatically,
		})
		errs <- err
	}()
	go func() {
		<-start
		errs <- svc.SetCustomerInvoiceProfile(ctx, payer, money.CustomerInvoiceProfile{
			NetTermsDays:     45,
			CollectionMethod: money.CollectionSendInvoice,
			Memo:             "concurrent operator terms",
		})
	}()
	close(start)
	require.NoError(t, <-errs)
	require.NoError(t, <-errs)

	got, err = svc.GetCustomerInvoiceProfile(ctx, payer)
	require.NoError(t, err)
	require.Equal(t, 45, got.NetTermsDays)
	require.Equal(t, money.CollectionSendInvoice, got.CollectionMethod)
	require.Equal(t, "concurrent operator terms", got.Memo)

	_, err = pool.Exec(ctx, "DELETE FROM openrails.customer_invoice_profiles WHERE customer_id = $1", payer.UUID())
	require.NoError(t, err)
	type ensureResult struct {
		created bool
		err     error
	}
	results := make(chan ensureResult, 2)
	start = make(chan struct{})
	for range 2 {
		go func() {
			<-start
			created, err := svc.EnsureCustomerInvoiceProfile(ctx, payer, money.CustomerInvoiceProfile{
				CollectionMethod: money.CollectionChargeAutomatically,
			})
			results <- ensureResult{created: created, err: err}
		}()
	}
	close(start)
	createdCount := 0
	for range 2 {
		result := <-results
		require.NoError(t, result.err)
		if result.created {
			createdCount++
		}
	}
	require.Equal(t, 1, createdCount)
}

func TestEnterpriseInvoicing_CustomerInvoiceProfileRejectsCrossMerchantPayer(t *testing.T) {
	svc, _, _, _, ctx := moneyInEnv(t)
	foreignMerchantID := uuid.New()
	foreignPayer := identity.CustomerIDFromString(uuid.NewString())
	// The fixture is deliberately ANOTHER merchant's — the whole point is that the
	// service refuses it — so seeding it is one of the few genuinely privileged
	// cases. No merchant-pinned connection can write another merchant's rows.
	pool := dbtest.SharedSuperuserPGXPool(t)
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.customers WHERE id = $1", foreignPayer.UUID())
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.merchants WHERE id = $1", foreignMerchantID)
	})

	_, err := pool.Exec(ctx,
		"INSERT INTO openrails.merchants (id, slug) VALUES ($1, $2)",
		foreignMerchantID, "foreign-profile-"+uuid.NewString()[:8],
	)
	require.NoError(t, err)
	_, err = pool.Exec(ctx,
		"INSERT INTO openrails.customers (id, merchant_id, subject) VALUES ($1, $2, $3)",
		foreignPayer.UUID(), foreignMerchantID, foreignPayer.UUID().String(),
	)
	require.NoError(t, err)

	profile := money.CustomerInvoiceProfile{CollectionMethod: money.CollectionChargeAutomatically}
	created, err := svc.EnsureCustomerInvoiceProfile(ctx, foreignPayer, profile)
	require.ErrorContains(t, err, "payer belongs to another merchant")
	require.False(t, created)
	require.ErrorContains(t, svc.SetCustomerInvoiceProfile(ctx, foreignPayer, profile), "payer belongs to another merchant")
}

// TestEnterpriseInvoicing_PastWindowFinalizeAttachesAccruals pins the #798
// fix: finalizing a window that ENDED in the past (the previous-month close)
// must rate + ATTACH that window's usage — accruals are stamped with their
// rating period, not the sweep time.
func TestEnterpriseInvoicing_PastWindowFinalizeAttachesAccruals(t *testing.T) {
	svc, pool, payer, cur, ctx := moneyInEnv(t)
	merchantID := dbtest.TestMerchantID.UUID()
	productID := uuid.New()
	meter := "past-gb-" + uuid.NewString()[:8]
	eventType := "storage.past_gb." + uuid.NewString()[:8]
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.usage_events WHERE customer_id = $1", payer.UUID())
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.invoice_items WHERE customer_id = $1", payer.UUID())
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.invoices WHERE customer_id = $1", payer.UUID())
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.metered_rating_watermarks WHERE customer_id = $1", payer.UUID())
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.catalog_rate_cards WHERE merchant_id = $1 AND meter_key = $2", merchantID, meter)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.catalog_meters WHERE merchant_id = $1 AND key = $2", merchantID, meter)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.products WHERE id = $1", productID)
	})
	_, err := pool.Exec(ctx, `INSERT INTO openrails.products (id, key, display_name, merchant_id) VALUES ($1, $2, 'Past', $3)`,
		productID, "past-"+uuid.NewString()[:8], merchantID)
	require.NoError(t, err)
	require.NoError(t, svc.EnsureUsageMeter(ctx, money.UsageMeterSpec{
		Key: meter, EventType: eventType, ValueProperty: "mib_seconds", Aggregation: "sum",
	}))
	require.NoError(t, svc.SetUsageRateCard(ctx, money.UsageRateCardInput{
		ProductID: &productID, MeterKey: meter,
		Price: pricing.RatePrice{Model: "per_unit", Currency: "USD",
			PerUnit: &pricing.PerUnitPrice{UnitAmount: 20_000, DivideBy: gbMonthDivisor}},
	}))
	_, err = svc.UpsertAccountSettings(ctx, payer, money.DefaultCurrency, money.AccountSettingsInput{
		BillingMode: strptr(money.BillingModeArrears),
	})
	require.NoError(t, err)

	// Usage occurred INSIDE a window that closed 10 days ago.
	from := time.Now().AddDate(0, 0, -40)
	to := time.Now().AddDate(0, 0, -10)
	_, err = svc.RecordUsage(ctx, money.RecordUsageParams{
		Payer: &payer, Invoker: payer.UUID().String(), Currency: cur, EventType: eventType,
		Dimensions: map[string]int64{"mib_seconds": 3 * gbMonthDivisor},
		Amount:     0, Source: "th798-past", SourceID: uuid.NewString(), OccurredAt: time.Now().AddDate(0, 0, -20),
	})
	require.NoError(t, err)

	inv, err := svc.FinalizeInvoice(ctx, payer, cur, from, to)
	require.NoError(t, err)
	require.Equal(t, int64(60_000), inv.AmountDue, "past-window finalize attaches the window's rated usage")
	outstanding, err := svc.GetOutstandingOwed(ctx, payer, cur)
	require.NoError(t, err)
	require.Equal(t, int64(60_000), outstanding, "no pending leakage outside the invoice")
}

func TestEnterpriseInvoicing_SweepUsageFeedsPendingChargesAndExposure(t *testing.T) {
	svc, pool, payer, cur, ctx := moneyInEnv(t)
	merchantID := dbtest.TestMerchantID.UUID()
	productID := uuid.New()
	meter := "media-gb-" + uuid.NewString()[:8]
	eventType := "storage.user_media_gb." + uuid.NewString()[:8]
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.usage_events WHERE customer_id = $1", payer.UUID())
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.invoice_items WHERE customer_id = $1", payer.UUID())
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.metered_rating_watermarks WHERE customer_id = $1", payer.UUID())
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.catalog_rate_cards WHERE merchant_id = $1 AND meter_key = $2", merchantID, meter)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.catalog_meters WHERE merchant_id = $1 AND key = $2", merchantID, meter)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.products WHERE id = $1", productID)
	})

	_, err := pool.Exec(ctx, `INSERT INTO openrails.products (id, key, display_name, merchant_id) VALUES ($1, $2, 'Media', $3)`,
		productID, "media-"+uuid.NewString()[:8], merchantID)
	require.NoError(t, err)
	require.NoError(t, svc.EnsureUsageMeter(ctx, money.UsageMeterSpec{
		Key: meter, EventType: eventType, ValueProperty: "mib_seconds", Aggregation: "sum",
	}))
	require.NoError(t, svc.SetUsageRateCard(ctx, money.UsageRateCardInput{
		ProductID: &productID, MeterKey: meter,
		Price: pricing.RatePrice{Model: "per_unit", Currency: "USD",
			PerUnit: &pricing.PerUnitPrice{UnitAmount: 20_000, DivideBy: gbMonthDivisor}},
	}))
	_, err = svc.UpsertAccountSettings(ctx, payer, money.DefaultCurrency, money.AccountSettingsInput{
		BillingMode: strptr(money.BillingModeArrears),
	})
	require.NoError(t, err)

	_, err = svc.RecordUsage(ctx, money.RecordUsageParams{
		Payer: &payer, Invoker: payer.UUID().String(), Currency: cur, EventType: eventType,
		Dimensions: map[string]int64{"mib_seconds": 2 * gbMonthDivisor},
		Amount:     0, Source: "th798-sweep", SourceID: uuid.NewString(), OccurredAt: time.Now(),
	})
	require.NoError(t, err)

	from := time.Now().Add(-time.Hour)
	to := time.Now().Add(time.Hour)
	require.NoError(t, svc.SweepUsage(ctx, payer, cur, from, to))

	// 2 GB-month x $0.02 = 40_000 micros of pending (uninvoiced) charges.
	pending, err := svc.ListPendingCharges(ctx, payer, cur)
	require.NoError(t, err)
	var total int64
	found := false
	for _, p := range pending {
		total += p.Amount
		if p.Source == "metered:"+meter {
			found = true
			require.Equal(t, int64(40_000), p.Amount)
		}
	}
	require.True(t, found)
	require.Equal(t, int64(40_000), total)

	// Exposure (credit-limit input) includes the swept-but-uninvoiced charge.
	outstanding, err := svc.GetOutstandingOwed(ctx, payer, cur)
	require.NoError(t, err)
	require.Equal(t, int64(40_000), outstanding)

	// The sweep is watermarked: repeating it accrues nothing new.
	require.NoError(t, svc.SweepUsage(ctx, payer, cur, from, to))
	outstanding2, err := svc.GetOutstandingOwed(ctx, payer, cur)
	require.NoError(t, err)
	require.Equal(t, outstanding, outstanding2)
}
