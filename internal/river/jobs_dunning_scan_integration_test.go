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
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/internal/modules/entitlements"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/riverqueue/river"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// seedScanSubscription inserts a subscription row shaped for the dunning scan
// filter matrix. Returns the subscription id.
func seedScanSubscription(ctx context.Context, t *testing.T, q *gen.Queries, productID, priceID, customerID uuid.UUID, rail string, status models.SubscriptionStatus, nextRetryAt *time.Time) uuid.UUID {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Second)
	periodEnd := now.Add(-24 * time.Hour)
	periodStart := periodEnd.Add(-30 * 24 * time.Hour)
	id := uuid.New()
	params := gen.CreateSubscriptionParams{
		ID: id, MerchantID: dbtest.TestMerchantID.UUID(), CustomerID: customerID, ProductID: productID, PriceID: &priceID,
		Status: string(status), Rail: rail,
		RailSubscriptionID:    "sub_scan_" + uuid.New().String(),
		CurrentPeriodStartsAt: &periodStart, CurrentPeriodEndsAt: &periodEnd, StartedAt: periodStart,
		NextRetryAt: nextRetryAt, CreatedAt: now, UpdatedAt: now,
	}
	if status == models.StatusCancelled {
		// chk_cancelled_no_retry_schedule: the schema itself already makes a
		// cancelled row with a retry schedule unrepresentable — the strongest
		// possible pin that terminal rows can never re-enter the work list.
		cancelType := string(models.CancelTypeUser)
		params.CancelledAt = &now
		params.EndedAt = &now
		params.CancelType = &cancelType
		params.NextRetryAt = nil
	}
	_, err := q.CreateSubscription(ctx, params)
	require.NoError(t, err)
	return id
}

// TestDunningScan_DueQueryFilters pins the dunning work-list filters
// (ListDueDunningSubscriptions) against real rows: ONLY a past_due
// subscription on a requested rail whose next_retry_at is due is returned —
// future retries, other statuses, other rails and retry-less rows are all
// excluded. Replaces the tests/ dunning_worker filter/skip tests (#694), which
// asserted the same filters indirectly through no-op worker runs.
func TestDunningScan_DueQueryFilters(t *testing.T) {
	dsn := dbtest.SharedPostgresDSN(t)
	ctx := dbtest.WithTestMerchant(context.Background())
	dbi := dbtest.OpenAppDB(t, dsn)
	// RLS-enforcing harness: every row here is the test merchant's own; the
	// merchants row itself is control plane.
	pool := dbtest.SharedMerchantPool(t, dbtest.TestMerchantID.UUID())
	q := gen.New(pool)
	dbtest.EnsureTestMerchant(ctx, t, dbtest.OpenAppDB(t, dbtest.SharedSuperuserDSN(t)).Pool())
	now := time.Now().UTC().Truncate(time.Second)

	productID, priceID := uuid.New(), uuid.New()
	description := "Scan filters"
	_, err := q.CreateProduct(ctx, gen.CreateProductParams{
		ID: productID, Key: "scan_product_" + uuid.New().String(), DisplayName: "Scan Product",
		MerchantID:  dbtest.TestMerchantID.UUID(),
		Description: &description, Archived: false, CreatedAt: now, UpdatedAt: now,
	})
	require.NoError(t, err)
	billingHours := int32(720)
	_, err = q.CreatePrice(ctx, gen.CreatePriceParams{
		ID: priceID, ProductID: productID, Amount: 999, Currency: "USD",
		MerchantID: dbtest.TestMerchantID.UUID(),
		Archived:   false, AccessDurationHours: &billingHours, AutoRenew: true, CreatedAt: now, UpdatedAt: now,
	})
	require.NoError(t, err)

	// One customer per row: live statuses share the one-live-sub-per-
	// (customer, product) lifecycle slot (uq_subscriptions_customer_product_lifecycle).
	newCustomer := func() uuid.UUID { return dbtest.EnsureCustomerIDPgx(ctx, t, pool, uuid.New().String()) }
	pastRetry := now.Add(-time.Hour)
	futureRetry := now.Add(24 * time.Hour)

	due := seedScanSubscription(ctx, t, q, productID, priceID, newCustomer(), string(models.RailNMI), models.StatusPastDue, &pastRetry)
	notDueYet := seedScanSubscription(ctx, t, q, productID, priceID, newCustomer(), string(models.RailNMI), models.StatusPastDue, &futureRetry)
	active := seedScanSubscription(ctx, t, q, productID, priceID, newCustomer(), string(models.RailNMI), models.StatusActive, &pastRetry)
	cancelled := seedScanSubscription(ctx, t, q, productID, priceID, newCustomer(), string(models.RailNMI), models.StatusCancelled, &pastRetry)
	otherRail := seedScanSubscription(ctx, t, q, productID, priceID, newCustomer(), string(models.RailCCBill), models.StatusPastDue, &pastRetry)
	noRetryAt := seedScanSubscription(ctx, t, q, productID, priceID, newCustomer(), string(models.RailNMI), models.StatusPastDue, nil)
	seeded := map[uuid.UUID]string{
		due: "due", notDueYet: "notDueYet", active: "active",
		cancelled: "cancelled", otherRail: "otherRail", noRetryAt: "noRetryAt",
	}
	t.Cleanup(func() {
		for id := range seeded {
			_, _ = pool.Exec(ctx, "DELETE FROM openrails.subscriptions WHERE id = $1", id)
		}
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.prices WHERE id = $1", priceID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.products WHERE id = $1", productID)
	})

	got := map[uuid.UUID]bool{}
	require.NoError(t, dbi.RunInMerchantConn(ctx, func(mctx context.Context) error {
		rows, qerr := subscriptions.NewSubscriptionRepo(dbi).ListDueDunningSubscriptions(mctx, []string{string(models.RailNMI)}, time.Now().UTC())
		if qerr != nil {
			return qerr
		}
		for _, s := range rows {
			if _, mine := seeded[s.ID]; mine {
				got[s.ID] = true
			}
		}
		return nil
	}))
	assert.Equal(t, map[uuid.UUID]bool{due: true}, got,
		"exactly the due past_due NMI row is in the work list (no future retries, other statuses, other rails, or retry-less rows)")

	// Nil-clients guard: a deployment without NMI clients (e.g. Stripe-only)
	// skips the run cleanly instead of erroring the River job.
	worker := &DunningWorker{DB: dbi}
	require.NoError(t, worker.Work(context.Background(), &river.Job[DunningArgs]{Args: DunningArgs{}}))
}

// TestDunningScan_MissingPaymentMethodParksInsteadOfFailing pins the two
// nothing-chargeable shapes (replaces the tests/ dunning_worker
// missing-payment-method tests (#694), which only asserted
// status ∈ {past_due, cancelled}):
//
//   - an OPENRAILS-driven payment method with missing vault refs PARKS as
//     `unknown` (#840) — no provider mutation, no charge intent, and crucially
//     no dunning failure recorded: the absence of one of OUR OWN rows is not a
//     decline, and must never burn a retry ordinal or reach a terminal outcome;
//   - NO payment method at all on NMI means PROVIDER-AUTO-BILLED (#635/#682:
//     vault-less legacy import) — the sub is left completely untouched for
//     provider-pull reconciliation, never failed or charged.
func TestDunningScan_MissingPaymentMethodParksInsteadOfFailing(t *testing.T) {
	dsn := dbtest.SharedPostgresDSN(t)
	ctx := dbtest.WithTestMerchant(context.Background())
	dbi := dbtest.OpenAppDB(t, dsn)
	// RLS-enforcing harness: every row here is the test merchant's own; the
	// merchants row itself is control plane.
	pool := dbtest.SharedMerchantPool(t, dbtest.TestMerchantID.UUID())
	q := gen.New(pool)
	dbtest.EnsureTestMerchant(ctx, t, dbtest.OpenAppDB(t, dbtest.SharedSuperuserDSN(t)).Pool())
	now := time.Now().UTC().Truncate(time.Second)

	productID, priceID, subID := uuid.New(), uuid.New(), uuid.New()
	description := "Missing PM"
	_, err := q.CreateProduct(ctx, gen.CreateProductParams{
		ID: productID, Key: "nopm_product_" + uuid.New().String(), DisplayName: "No PM Product",
		MerchantID:  dbtest.TestMerchantID.UUID(),
		Description: &description, Archived: false, CreatedAt: now, UpdatedAt: now,
	})
	require.NoError(t, err)
	billingHours := int32(720) // 30-day cycle: monthly dunning tier
	_, err = q.CreatePrice(ctx, gen.CreatePriceParams{
		ID: priceID, ProductID: productID, Amount: 999, Currency: "USD",
		MerchantID: dbtest.TestMerchantID.UUID(),
		Archived:   false, AccessDurationHours: &billingHours, AutoRenew: true, CreatedAt: now, UpdatedAt: now,
	})
	require.NoError(t, err)

	// In-window: period ended a day ago on a 30-day cycle (window is 14d).
	periodEnd := now.Add(-24 * time.Hour)
	periodStart := periodEnd.Add(-30 * 24 * time.Hour)
	nextRetry := now.Add(-time.Minute)

	// Sub A: openrails-driven payment method whose vault refs are MISSING —
	// nothing chargeable, and nothing that could justify a failure either.
	customerID := dbtest.EnsureCustomerIDPgx(ctx, t, pool, uuid.New().String())
	paymentMethodID := uuid.New()
	_, err = q.CreatePaymentMethod(ctx, gen.CreatePaymentMethodParams{
		ID: paymentMethodID, MerchantID: dbtest.TestMerchantID.UUID(), CustomerID: customerID, Rail: string(models.RailNMI),
		RailCustomerRef: "", RailMethodRef: "", // missing vault!
		RebillDriver:         "openrails", // OpenRails drives rebills — NOT provider-auto-billed
		InitialTransactionID: "txn_initial_" + uuid.New().String(), CreatedAt: now, UpdatedAt: now,
	})
	require.NoError(t, err)
	_, err = q.CreateSubscription(ctx, gen.CreateSubscriptionParams{
		ID: subID, MerchantID: dbtest.TestMerchantID.UUID(), CustomerID: customerID, ProductID: productID, PriceID: &priceID,
		Status: string(models.StatusPastDue), Rail: string(models.RailNMI),
		RailSubscriptionID: "sub_nopm_" + uuid.New().String(), PaymentMethodID: &paymentMethodID,
		CurrentPeriodStartsAt: &periodStart, CurrentPeriodEndsAt: &periodEnd, StartedAt: periodStart,
		NextRetryAt: &nextRetry, CreatedAt: now, UpdatedAt: now,
	})
	require.NoError(t, err)

	// Sub B: NO payment method at all — the #635 vault-less provider-billed
	// shape; dunning must leave it completely alone.
	autoBilledSubID := uuid.New()
	autoBilledCustomer := dbtest.EnsureCustomerIDPgx(ctx, t, pool, uuid.New().String())
	_, err = q.CreateSubscription(ctx, gen.CreateSubscriptionParams{
		ID: autoBilledSubID, MerchantID: dbtest.TestMerchantID.UUID(), CustomerID: autoBilledCustomer, ProductID: productID, PriceID: &priceID,
		Status: string(models.StatusPastDue), Rail: string(models.RailNMI),
		RailSubscriptionID:    "sub_autobilled_" + uuid.New().String(),
		CurrentPeriodStartsAt: &periodStart, CurrentPeriodEndsAt: &periodEnd, StartedAt: periodStart,
		NextRetryAt: &nextRetry, CreatedAt: now, UpdatedAt: now,
	})
	require.NoError(t, err)

	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.rail_intents WHERE subscription_id = ANY($1)", []uuid.UUID{subID, autoBilledSubID})
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.subscriptions WHERE id = ANY($1)", []uuid.UUID{subID, autoBilledSubID})
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.payment_methods WHERE id = $1", paymentMethodID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.prices WHERE id = $1", priceID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.products WHERE id = $1", productID)
	})

	// Configured client whose only allowed traffic is the read-only profile
	// query — any mutation is counted and fails the test.
	var nmiMutations atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.Form.Get("report_type") == "profile" {
			_, _ = w.Write([]byte(`<?xml version="1.0"?><nm_response><merchant><company>NoPM TEST</company><email>nopm@acme.test</email></merchant></nm_response>`))
			return
		}
		nmiMutations.Add(1)
		_, _ = w.Write([]byte("response=1"))
	}))
	t.Cleanup(srv.Close)
	client, err := nmi.NewClient("mobius", &config.NMIProviderSettings{
		SecurityKey: "nopm_security_key", WebhookSecret: "s",
	}, true)
	require.NoError(t, err)
	client.DirectPostURL = srv.URL
	client.QueryURL = srv.URL

	worker := &DunningWorker{DB: dbi, NMIResolver: fakeDunningNMIResolver{client: client}}

	priceSvc := catalog.NewPriceService(dbi)
	productSvc := catalog.NewProductService(dbi)
	entitlementSvc := entitlements.NewEntitlementService(dbi, nil)
	notifSvc := subscriptions.NewNotificationService(dbi, nil)
	paymentSvc := payments.NewPaymentService(dbi, nil)
	lifecycle := subscriptions.NewSubscriptionLifecycleService(dbi, productSvc, priceSvc, entitlementSvc, notifSvc, paymentSvc, nil)
	moneySvc := money.NewMoneyService(dbi, nil)

	subSvc := subscriptions.NewSubscriptionService(dbi, priceSvc, productSvc, nil, nil, nil)

	// --- Sub A: openrails-driven pm with missing vault refs -> park (#840) ---
	var outcome dunningOutcome
	require.NoError(t, dbi.RunInMerchantConn(ctx, func(mctx context.Context) error {
		sub, gerr := subSvc.GetByID(mctx, subID)
		if gerr != nil {
			return gerr
		}
		outcome = worker.processSubscription(mctx, sub, lifecycle, priceSvc, moneySvc, false)
		return nil
	}))
	assert.Equal(t, dunningOutcomeFailed, outcome)
	assert.Zero(t, nmiMutations.Load(), "nothing chargeable exists; no provider mutation may be sent")

	// No charge intent reaches the ledger — the pm check runs before enqueue.
	var intentCount int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM openrails.rail_intents WHERE subscription_id = $1`, subID).Scan(&intentCount))
	assert.Zero(t, intentCount, "missing vault refs never record a charge intent")

	// #840: parked for provider verification — NOT failed. No retry ordinal is
	// burned and no claim is stamped, because no attempt was ever made.
	var status string
	var retryAttempts *int32
	var lastRetryAt, nextRetryAt *time.Time
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT status, retry_attempts, last_retry_at, next_retry_at FROM openrails.subscriptions WHERE id = $1`, subID).
		Scan(&status, &retryAttempts, &lastRetryAt, &nextRetryAt))
	assert.Equal(t, string(models.StatusUnknown), status,
		"missing local vault refs park the row; the unknown-cohort probe verifies it against the provider")
	assert.Nil(t, retryAttempts, "an attempt we never made cannot count toward retry_attempts")
	assert.Nil(t, lastRetryAt, "the claim must not run: last_retry_at is dunning forensic evidence")
	assert.Nil(t, nextRetryAt, "a parked row leaves the dunning queue")

	// --- Sub B: pm-less = provider-auto-billed (#635) -> untouched ---
	require.NoError(t, dbi.RunInMerchantConn(ctx, func(mctx context.Context) error {
		autoSub, gerr := subSvc.GetByID(mctx, autoBilledSubID)
		if gerr != nil {
			return gerr
		}
		outcome = worker.processSubscription(mctx, autoSub, lifecycle, priceSvc, moneySvc, false)
		return nil
	}))
	assert.Equal(t, dunningOutcomeFailed, outcome)
	assert.Zero(t, nmiMutations.Load(), "provider-auto-billed sub must never be charged by us")

	var autoStatus string
	var autoRetryAttempts *int32
	var autoNextRetryAt *time.Time
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT status, retry_attempts, next_retry_at FROM openrails.subscriptions WHERE id = $1`, autoBilledSubID).
		Scan(&autoStatus, &autoRetryAttempts, &autoNextRetryAt))
	assert.Equal(t, string(models.StatusPastDue), autoStatus, "provider-billed sub awaits provider-pull reconciliation")
	assert.Nil(t, autoRetryAttempts, "no failure policy applied — the provider owns billing")
	require.NotNil(t, autoNextRetryAt)
	assert.WithinDuration(t, nextRetry, autoNextRetryAt.UTC(), time.Second, "retry state untouched (no claim)")
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM openrails.rail_intents WHERE subscription_id = $1`, autoBilledSubID).Scan(&intentCount))
	assert.Zero(t, intentCount, "no charge intent for a provider-billed sub")
}
