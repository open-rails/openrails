//go:build integration

package riverjobs

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/internal/modules/entitlements"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestDunningWorker_MaterializeRecordsParkedIntent (#366): under
// mode=limited the dunning scan records the in-window charge decision as a
// PENDING manual_rebill intent on the ledger — no NMI call, no claim (the
// imported last_retry_at forensic evidence stays untouched), no lifecycle
// movement. The #365 account fingerprint is stamped at enqueue.
func TestDunningWorker_MaterializeRecordsParkedIntent(t *testing.T) {
	dsn := dbtest.SharedPostgresDSN(t)
	ctx := context.Background()
	dbi := dbtest.OpenAppDB(t, dsn)
	pool := dbi.Pool()
	q := gen.New(pool)
	now := time.Now().UTC().Truncate(time.Second)

	productID, priceID, paymentMethodID, subID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	userID := uuid.New().String()
	billingDays := 30
	billingDays32 := int32(billingDays)

	description := "Materialize test"
	_, err := q.CreateProduct(ctx, gen.CreateProductParams{
		ID: productID, Slug: "mat_product_" + uuid.New().String(), DisplayName: "Materialize Product",
		MerchantID:  dbtest.TestMerchantID.UUID(),
		Description: &description, Status: string(models.CatalogStatusActive), CreatedAt: now, UpdatedAt: now,
	})
	require.NoError(t, err)
	_, err = q.CreatePrice(ctx, gen.CreatePriceParams{
		ID: priceID, ProductID: productID, Amount: 999, Currency: "usd",
		MerchantID: dbtest.TestMerchantID.UUID(),
		Status:     string(models.CatalogStatusActive), BillingCycleDays: &billingDays32, CreatedAt: now, UpdatedAt: now,
	})
	require.NoError(t, err)

	billingID := "bill_" + uuid.New().String()
	tenantSubjectID := dbtest.EnsureCustomerIDPgx(ctx, t, pool, userID)
	_, err = q.CreatePaymentMethod(ctx, gen.CreatePaymentMethodParams{
		ID: paymentMethodID, CustomerID: tenantSubjectID, Rail: string(models.RailMobius),
		VaultID: "vault_" + uuid.New().String(), BillingID: &billingID,
		InitialTransactionID: "txn_initial_" + uuid.New().String(), CreatedAt: now, UpdatedAt: now,
	})
	require.NoError(t, err)

	// In-window: period ended a day ago on a 30-day cycle (window is weeks).
	periodEnd := now.Add(-24 * time.Hour)
	periodStart := periodEnd.Add(-30 * 24 * time.Hour)
	nextRetry := now.Add(-time.Minute)
	_, err = q.CreateSubscription(ctx, gen.CreateSubscriptionParams{
		ID: subID, MerchantID: dbtest.TestMerchantID.UUID(), CustomerID: tenantSubjectID, ProductID: productID, PriceID: &priceID,
		Status: string(models.StatusPastDue), Rail: string(models.RailMobius),
		RailSubscriptionID: "sub_mat_" + uuid.New().String(), PaymentMethodID: &paymentMethodID,
		CurrentPeriodStartsAt: &periodStart, CurrentPeriodEndsAt: &periodEnd, StartedAt: periodStart,
		NextRetryAt: &nextRetry, CreatedAt: now, UpdatedAt: now,
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.provider_intents WHERE subscription_id = $1", subID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.subscriptions WHERE id = $1", subID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.payment_methods WHERE id = $1", paymentMethodID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.prices WHERE id = $1", priceID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.products WHERE id = $1", productID)
	})

	// Materialize must never send a provider MUTATION. The only NMI traffic
	// allowed is the read-only profile query the #365 fingerprint stamp makes.
	var nmiMutations atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.Form.Get("report_type") == "profile" {
			_, _ = w.Write([]byte(`<?xml version="1.0"?><nm_response><merchant><company>Mat TEST</company><email>mat@acme.test</email></merchant></nm_response>`))
			return
		}
		nmiMutations.Add(1)
		_, _ = w.Write([]byte("response=1"))
	}))
	t.Cleanup(srv.Close)
	client, err := nmi.NewClient("mobius", &config.NMIProviderSettings{
		SecurityKey: "mat_security_key", WebhookSecret: "s",
	}, true)
	require.NoError(t, err)
	client.DirectPostURL = srv.URL
	client.QueryURL = srv.URL

	worker := &DunningWorker{DB: dbi, NMIClients: map[string]*nmi.NMIClient{"mobius": client}}

	priceSvc := catalog.NewPriceService(dbi)
	productSvc := catalog.NewProductService(dbi)
	entitlementSvc := entitlements.NewEntitlementService(dbi, nil)
	notifSvc := subscriptions.NewNotificationService(dbi, nil)
	paymentSvc := payments.NewPaymentService(dbi, nil)
	lifecycle := subscriptions.NewSubscriptionLifecycleService(dbi, productSvc, priceSvc, entitlementSvc, notifSvc, paymentSvc, nil, nil)
	moneySvc := money.NewMoneyService(dbi, nil)

	sub, err := subscriptions.NewSubscriptionService(dbi, priceSvc, productSvc, nil, nil, nil).GetByID(ctx, subID)
	require.NoError(t, err)

	outcome := worker.processSubscription(dbtest.WithTestMerchant(ctx), sub, lifecycle, priceSvc, moneySvc, true)
	assert.Equal(t, dunningOutcomeMaterialized, outcome)
	assert.Zero(t, nmiMutations.Load(), "materialize must not send a provider mutation")

	// The decision is on the ledger: pending, system-origin, window-bounded,
	// #518 provider account binding stamped.
	var status, origin, intentType string
	var providerAccountID *uuid.UUID
	var expiresAt *time.Time
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT status, origin, intent_type, provider_account_id, expires_at
		 FROM openrails.provider_intents WHERE subscription_id = $1`, subID).
		Scan(&status, &origin, &intentType, &providerAccountID, &expiresAt))
	assert.Equal(t, intents.StatusPending, status)
	assert.Equal(t, string(intents.OriginSystem), origin)
	assert.Equal(t, intents.TypeManualRebill, intentType)
	require.NotNil(t, providerAccountID)
	require.NotNil(t, expiresAt, "parked charge must be bounded by the dunning window")

	// No lifecycle movement, and the forensic retry fields are untouched
	// (materialize never claims).
	var subStatus string
	var lastRetryAt *time.Time
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT status, last_retry_at FROM openrails.subscriptions WHERE id = $1`, subID).
		Scan(&subStatus, &lastRetryAt))
	assert.Equal(t, string(models.StatusPastDue), subStatus)
	assert.Nil(t, lastRetryAt, "materialize must not write last_retry_at (dunning forensics evidence)")

	// Idempotent: a second materialize pass refreshes the same pending intent.
	outcome = worker.processSubscription(dbtest.WithTestMerchant(ctx), sub, lifecycle, priceSvc, moneySvc, true)
	assert.Equal(t, dunningOutcomeMaterialized, outcome)
	var intentCount int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM openrails.provider_intents WHERE subscription_id = $1`, subID).Scan(&intentCount))
	assert.Equal(t, 1, intentCount)
}

// TestDunningWorker_MaterializeWindowExpiryStillCancelsLocally (#366): the
// window-expired path is LOCAL convergence (cancel + downgrade, no charge)
// and applies under materialize exactly as under full mode.
func TestDunningWorker_MaterializeWindowExpiryStillCancelsLocally(t *testing.T) {
	dsn := dbtest.SharedPostgresDSN(t)
	ctx := context.Background()
	dbi := dbtest.OpenAppDB(t, dsn)
	pool := dbi.Pool()
	q := gen.New(pool)
	now := time.Now().UTC().Truncate(time.Second)

	productID, priceID, subID := uuid.New(), uuid.New(), uuid.New()
	userID := uuid.New().String()
	billingDays32 := int32(30)

	description := "Window expiry test"
	_, err := q.CreateProduct(ctx, gen.CreateProductParams{
		ID: productID, Slug: "wexp_product_" + uuid.New().String(), DisplayName: "Window Expiry Product",
		MerchantID:  dbtest.TestMerchantID.UUID(),
		Description: &description, Status: string(models.CatalogStatusActive), CreatedAt: now, UpdatedAt: now,
	})
	require.NoError(t, err)
	_, err = q.CreatePrice(ctx, gen.CreatePriceParams{
		ID: priceID, ProductID: productID, Amount: 999, Currency: "usd",
		MerchantID: dbtest.TestMerchantID.UUID(),
		Status:     string(models.CatalogStatusActive), BillingCycleDays: &billingDays32, CreatedAt: now, UpdatedAt: now,
	})
	require.NoError(t, err)

	tenantSubjectID := dbtest.EnsureCustomerIDPgx(ctx, t, pool, userID)
	// Months-stale: the legacy-migration shape. Window for 30-day cycle is
	// ~2 weeks; 90 days is far past it.
	periodEnd := now.Add(-90 * 24 * time.Hour)
	periodStart := periodEnd.Add(-30 * 24 * time.Hour)
	nextRetry := now.Add(-time.Minute)
	_, err = q.CreateSubscription(ctx, gen.CreateSubscriptionParams{
		ID: subID, MerchantID: dbtest.TestMerchantID.UUID(), CustomerID: tenantSubjectID, ProductID: productID, PriceID: &priceID,
		Status: string(models.StatusPastDue), Rail: string(models.RailMobius),
		RailSubscriptionID:    "sub_wexp_" + uuid.New().String(),
		CurrentPeriodStartsAt: &periodStart, CurrentPeriodEndsAt: &periodEnd, StartedAt: periodStart,
		NextRetryAt: &nextRetry, CreatedAt: now, UpdatedAt: now,
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.provider_intents WHERE subscription_id = $1", subID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.subscriptions WHERE id = $1", subID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.prices WHERE id = $1", priceID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.products WHERE id = $1", productID)
	})

	var nmiMutations atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.Form.Get("report_type") == "profile" {
			_, _ = w.Write([]byte(`<?xml version="1.0"?><nm_response><merchant><company>Mat TEST</company><email>mat@acme.test</email></merchant></nm_response>`))
			return
		}
		nmiMutations.Add(1)
	}))
	t.Cleanup(srv.Close)
	client, err := nmi.NewClient("mobius", &config.NMIProviderSettings{
		SecurityKey: "wexp_security_key", WebhookSecret: "s",
	}, true)
	require.NoError(t, err)
	client.DirectPostURL = srv.URL
	client.QueryURL = srv.URL

	worker := &DunningWorker{DB: dbi, NMIClients: map[string]*nmi.NMIClient{"mobius": client}}

	priceSvc := catalog.NewPriceService(dbi)
	productSvc := catalog.NewProductService(dbi)
	entitlementSvc := entitlements.NewEntitlementService(dbi, nil)
	notifSvc := subscriptions.NewNotificationService(dbi, nil)
	paymentSvc := payments.NewPaymentService(dbi, nil)
	lifecycle := subscriptions.NewSubscriptionLifecycleService(dbi, productSvc, priceSvc, entitlementSvc, notifSvc, paymentSvc, nil, nil)
	moneySvc := money.NewMoneyService(dbi, nil)

	sub, err := subscriptions.NewSubscriptionService(dbi, priceSvc, productSvc, nil, nil, nil).GetByID(ctx, subID)
	require.NoError(t, err)

	outcome := worker.processSubscription(dbtest.WithTestMerchant(ctx), sub, lifecycle, priceSvc, moneySvc, true)
	assert.Equal(t, dunningOutcomeWindowExpired, outcome)
	assert.Zero(t, nmiMutations.Load(), "window expiry never charges")

	var subStatus string
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT status FROM openrails.subscriptions WHERE id = $1`, subID).Scan(&subStatus))
	assert.NotEqual(t, string(models.StatusPastDue), subStatus, "stale subscription must be cancelled, not left premium")
}
