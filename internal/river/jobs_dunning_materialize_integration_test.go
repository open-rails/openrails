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
	pool := dbtest.SharedMerchantPool(t, dbtest.TestMerchantID.UUID())
	q := gen.New(pool)
	dbtest.EnsureTestMerchant(ctx, t, pool)
	now := time.Now().UTC().Truncate(time.Second)

	productID, priceID, paymentMethodID, subID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	userID := uuid.New().String()
	billingDays := 720
	billingDays32 := int32(billingDays)

	description := "Materialize test"
	_, err := q.CreateProduct(ctx, gen.CreateProductParams{
		ID: productID, Key: "mat_product_" + uuid.New().String(), DisplayName: "Materialize Product",
		MerchantID:  dbtest.TestMerchantID.UUID(),
		Description: &description, Archived: false, CreatedAt: now, UpdatedAt: now,
	})
	require.NoError(t, err)
	_, err = q.CreatePrice(ctx, gen.CreatePriceParams{
		ID: priceID, ProductID: productID, Amount: 999, Currency: "USD",
		MerchantID: dbtest.TestMerchantID.UUID(),
		Archived:   false, AccessDurationHours: &billingDays32, AutoRenew: true, CreatedAt: now, UpdatedAt: now,
	})
	require.NoError(t, err)

	billingID := "bill_" + uuid.New().String()
	tenantSubjectID := dbtest.EnsureCustomerIDPgx(ctx, t, pool, userID)
	pspID := dbtest.EnsureTestPSP(ctx, t, pool, dbtest.TestMerchantID.UUID(), string(models.RailNMI))
	_, err = q.CreatePaymentMethod(ctx, gen.CreatePaymentMethodParams{
		ID: paymentMethodID, MerchantID: dbtest.TestMerchantID.UUID(), CustomerID: tenantSubjectID, Rail: string(models.RailNMI),
		PspID:           pspID,
		RailCustomerRef: "vault_" + uuid.New().String(), RailMethodRef: billingID,
		RebillDriver:         "openrails", // #682: legacy-imported shape, OpenRails drives rebills
		InitialTransactionID: "txn_initial_" + uuid.New().String(), CreatedAt: now, UpdatedAt: now,
	})
	require.NoError(t, err)
	dbtest.SeedNMIStoredCredentialRefs(ctx, t, pool, paymentMethodID)

	// In-window: period ended a day ago on a 30-day cycle (window is weeks).
	periodEnd := now.Add(-24 * time.Hour)
	periodStart := periodEnd.Add(-30 * 24 * time.Hour)
	nextRetry := now.Add(-time.Minute)
	_, err = q.CreateSubscription(ctx, gen.CreateSubscriptionParams{
		ID: subID, MerchantID: dbtest.TestMerchantID.UUID(), CustomerID: tenantSubjectID, ProductID: productID, PriceID: &priceID,
		Status: string(models.StatusPastDue), Rail: string(models.RailNMI),
		PspID:              pspID,
		RailSubscriptionID: "sub_mat_" + uuid.New().String(), PaymentMethodID: &paymentMethodID,
		CurrentPeriodStartsAt: &periodStart, CurrentPeriodEndsAt: &periodEnd, StartedAt: periodStart,
		NextRetryAt: &nextRetry, CreatedAt: now, UpdatedAt: now,
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.rail_intents WHERE subscription_id = $1", subID)
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

	// No Config on purpose: these two tests call processSubscription with
	// materialize=true, which only ENQUEUES the rebill intent (Store.Enqueue) —
	// or#865's origin x mode gate never runs, so there is no mode to state.
	worker := &DunningWorker{DB: dbi, NMIResolver: fakeDunningNMIResolver{client: client}}

	priceSvc := catalog.NewPriceService(dbi)
	productSvc := catalog.NewProductService(dbi)
	entitlementSvc := entitlements.NewEntitlementService(dbi, nil)
	notifSvc := subscriptions.NewNotificationService(dbi, nil)
	paymentSvc := payments.NewPaymentService(dbi, nil)
	lifecycle := subscriptions.NewSubscriptionLifecycleService(dbi, productSvc, priceSvc, entitlementSvc, notifSvc, paymentSvc, nil)
	moneySvc := money.NewMoneyService(dbi, nil)

	// Same shape as the production Work loop: the pass runs on a merchant-scoped
	// connection, so both the read and the enqueue carry the GUC. On the bare
	// context subscriptions' FORCEd RLS matched merchant_id = NULL and GetByID
	// returned "no rows in result set" for a row that exists.
	var outcome dunningOutcome
	var sub *models.Subscription
	require.NoError(t, dbi.RunInMerchantConn(dbtest.WithTestMerchant(ctx), func(mctx context.Context) error {
		var e error
		sub, e = subscriptions.NewSubscriptionService(dbi, priceSvc, productSvc, nil, nil, nil).GetByID(mctx, subID)
		if e != nil {
			return e
		}
		outcome = worker.processSubscription(mctx, sub, lifecycle, priceSvc, moneySvc, true)
		return nil
	}))
	assert.Equal(t, dunningOutcomeMaterialized, outcome)
	assert.Zero(t, nmiMutations.Load(), "materialize must not send a provider mutation")

	// The decision is on the ledger: pending, system-origin, window-bounded.
	// (or#893: psp_id is now total provenance — the materialized intent carries
	// the subscription's own PSP, stamped from sub.PspID at enqueue.)
	var status, origin, intentType string
	var gotPSPID uuid.UUID
	var expiresAt *time.Time
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT status, origin, intent_type, psp_id, expires_at
		 FROM openrails.rail_intents WHERE subscription_id = $1`, subID).
		Scan(&status, &origin, &intentType, &gotPSPID, &expiresAt))
	assert.Equal(t, intents.StatusPending, status)
	assert.Equal(t, string(intents.OriginSystem), origin)
	assert.Equal(t, intents.TypeManualRebill, intentType)
	assert.Equal(t, pspID, gotPSPID, "or#893: the materialized intent carries the subscription's PSP")
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
	require.NoError(t, dbi.RunInMerchantConn(dbtest.WithTestMerchant(ctx), func(mctx context.Context) error {
		outcome = worker.processSubscription(mctx, sub, lifecycle, priceSvc, moneySvc, true)
		return nil
	}))
	assert.Equal(t, dunningOutcomeMaterialized, outcome)
	var intentCount int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM openrails.rail_intents WHERE subscription_id = $1`, subID).Scan(&intentCount))
	assert.Equal(t, 1, intentCount)
}

// TestDunningWorker_MaterializeStalenessParksLocally (#366 + #839): the
// stale-window path is LOCAL convergence and applies under materialize exactly
// as under full mode — but since #839 that convergence is a PARK, not a cancel.
// Limited mode used to be documented as "local window-expiry cancellations
// apply"; there is no longer any local terminal cancellation on this path to
// apply, in any mode. The charge is still skipped: a months-stale rebill is
// never fired by a catch-up run.
func TestDunningWorker_MaterializeStalenessParksLocally(t *testing.T) {
	dsn := dbtest.SharedPostgresDSN(t)
	ctx := context.Background()
	dbi := dbtest.OpenAppDB(t, dsn)
	// RLS-enforcing harness: this fixture seeds and asserts on ONE merchant's
	// own rows, so it uses the merchant-pinned pool rather than a raw one.
	pool := dbtest.SharedMerchantPool(t, dbtest.TestMerchantID.UUID())
	q := gen.New(pool)
	dbtest.EnsureTestMerchant(ctx, t,
		dbtest.OpenAppDB(t, dbtest.SharedSuperuserDSN(t)).Pool())
	now := time.Now().UTC().Truncate(time.Second)

	productID, priceID, subID := uuid.New(), uuid.New(), uuid.New()
	userID := uuid.New().String()
	billingDays32 := int32(30)

	description := "Window expiry test"
	_, err := q.CreateProduct(ctx, gen.CreateProductParams{
		ID: productID, Key: "wexp_product_" + uuid.New().String(), DisplayName: "Window Expiry Product",
		MerchantID:  dbtest.TestMerchantID.UUID(),
		Description: &description, Archived: false, CreatedAt: now, UpdatedAt: now,
	})
	require.NoError(t, err)
	_, err = q.CreatePrice(ctx, gen.CreatePriceParams{
		ID: priceID, ProductID: productID, Amount: 999, Currency: "USD",
		MerchantID: dbtest.TestMerchantID.UUID(),
		Archived:   false, AccessDurationHours: &billingDays32, AutoRenew: true, CreatedAt: now, UpdatedAt: now,
	})
	require.NoError(t, err)

	tenantSubjectID := dbtest.EnsureCustomerIDPgx(ctx, t, pool, userID)
	pspID := dbtest.EnsureTestPSP(ctx, t, pool, dbtest.TestMerchantID.UUID(), string(models.RailNMI))
	// Months-stale: the legacy-migration shape. Window for 30-day cycle is
	// ~2 weeks; 90 days is far past it.
	periodEnd := now.Add(-90 * 24 * time.Hour)
	periodStart := periodEnd.Add(-30 * 24 * time.Hour)
	nextRetry := now.Add(-time.Minute)
	_, err = q.CreateSubscription(ctx, gen.CreateSubscriptionParams{
		ID: subID, MerchantID: dbtest.TestMerchantID.UUID(), CustomerID: tenantSubjectID, ProductID: productID, PriceID: &priceID,
		Status: string(models.StatusPastDue), Rail: string(models.RailNMI),
		PspID:                 pspID,
		RailSubscriptionID:    "sub_wexp_" + uuid.New().String(),
		CurrentPeriodStartsAt: &periodStart, CurrentPeriodEndsAt: &periodEnd, StartedAt: periodStart,
		NextRetryAt: &nextRetry, CreatedAt: now, UpdatedAt: now,
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.rail_intents WHERE subscription_id = $1", subID)
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

	// No Config on purpose: these two tests call processSubscription with
	// materialize=true, which only ENQUEUES the rebill intent (Store.Enqueue) —
	// or#865's origin x mode gate never runs, so there is no mode to state.
	worker := &DunningWorker{DB: dbi, NMIResolver: fakeDunningNMIResolver{client: client}}

	priceSvc := catalog.NewPriceService(dbi)
	productSvc := catalog.NewProductService(dbi)
	entitlementSvc := entitlements.NewEntitlementService(dbi, nil)
	notifSvc := subscriptions.NewNotificationService(dbi, nil)
	paymentSvc := payments.NewPaymentService(dbi, nil)
	lifecycle := subscriptions.NewSubscriptionLifecycleService(dbi, productSvc, priceSvc, entitlementSvc, notifSvc, paymentSvc, nil)
	moneySvc := money.NewMoneyService(dbi, nil)

	// Same shape as the production Work loop: the pass runs on a merchant-scoped
	// connection, so both the read and the lifecycle writes carry the GUC.
	var outcome dunningOutcome
	require.NoError(t, dbi.RunInMerchantConn(dbtest.WithTestMerchant(ctx), func(mctx context.Context) error {
		sub, err := subscriptions.NewSubscriptionService(dbi, priceSvc, productSvc, nil, nil, nil).GetByID(mctx, subID)
		if err != nil {
			return err
		}
		outcome = worker.processSubscription(mctx, sub, lifecycle, priceSvc, moneySvc, true)
		return nil
	}))
	assert.Equal(t, dunningOutcomeWindowExpired, outcome)
	assert.Zero(t, nmiMutations.Load(), "window expiry never charges")

	var subStatus string
	var cancelledAt *time.Time
	var deletionScheduledAt *time.Time
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT status, cancelled_at, deletion_scheduled_at FROM openrails.subscriptions WHERE id = $1`, subID).
		Scan(&subStatus, &cancelledAt, &deletionScheduledAt))
	assert.Equal(t, string(models.StatusUnknown), subStatus,
		"a stale subscription parks for provider verification; staleness is a clock reading, not a death certificate (#839)")
	assert.Nil(t, cancelledAt, "no local terminal cancellation on the staleness path, in any mode")
	assert.Nil(t, deletionScheduledAt, "and therefore no irreversible NMI vault delete is ever queued for it")
}
