//go:build integration

package riverjobs

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
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
	"github.com/open-rails/openrails/pkg/identity"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDunningWorker_RebillSuccess_GrantsCreditsOnce(t *testing.T) {
	dsn := dbtest.SharedPostgresDSN(t)

	ctx := context.Background()
	dbi := dbtest.OpenAppDB(t, dsn)
	pool := dbtest.SharedMerchantPool(t, dbtest.TestMerchantID.UUID())
	q := gen.New(pool)
	dbtest.EnsureTestMerchant(ctx, t, pool)

	var exists bool
	require.NoError(t, pool.QueryRow(ctx,
		"SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_schema = $1 AND table_name='ledger_transfers')",
		dbi.DataPool().Schema()).
		Scan(&exists))
	if !exists {
		t.Skip("ledger_transfers not found in the configured schema; run migrations before integration tests")
	}

	now := time.Now().UTC().Truncate(time.Second)

	// The grant spec key is just a label now (#472: money has no credit_type);
	// Unit "USD" deposits into the USD money balance.
	grantLabel := "test_credits_" + uuid.New().String()
	productID := uuid.New()
	priceID := uuid.New()
	paymentMethodID := uuid.New()
	subID := uuid.New()
	userID := uuid.New().String()

	billingDays := 720
	billingDays32 := int32(billingDays)

	creditsSpecJSON, err := json.Marshal(models.CreditsSpec{
		grantLabel: {Unit: "USD", Amount: 100, Cadence: models.CreditGrantCadencePerRenewal},
	})
	require.NoError(t, err)
	description := "Test"
	_, err = q.CreateProduct(ctx, gen.CreateProductParams{
		ID:          productID,
		Key:         "test_product_" + uuid.New().String(),
		DisplayName: "Test Product",
		MerchantID:  dbtest.TestMerchantID.UUID(),
		Description: &description,
		CreditsSpec: creditsSpecJSON,
		Archived:    false,
		CreatedAt:   now,
		UpdatedAt:   now,
	})
	require.NoError(t, err)

	price := &models.Price{
		ID:                  priceID,
		ProductID:           productID,
		Archived:            false,
		Amount:              999,
		Currency:            "USD",
		AccessDurationHours: &billingDays,
		AutoRenew:           true,
		CreatedAt:           now,
		UpdatedAt:           now,
	}
	_, err = q.CreatePrice(ctx, gen.CreatePriceParams{
		ID:                  priceID,
		ProductID:           productID,
		Amount:              999,
		Currency:            "USD",
		MerchantID:          dbtest.TestMerchantID.UUID(),
		Archived:            false,
		AccessDurationHours: &billingDays32,
		AutoRenew:           true,
		CreatedAt:           now,
		UpdatedAt:           now,
	})
	require.NoError(t, err)

	billingID := "bill_" + uuid.New().String()
	tenantSubjectID := dbtest.EnsureCustomerIDPgx(ctx, t, pool, userID)
	paymentMethod := &models.PaymentMethod{
		ID:                   paymentMethodID,
		CustomerID:           tenantSubjectID,
		Rail:                 models.RailNMI,
		RailCustomerRef:      "vault_" + uuid.New().String(),
		RailMethodRef:        billingID,
		RebillDriver:         "openrails", // #682: legacy-imported shape, OpenRails drives rebills
		InitialTransactionID: "txn_initial_" + uuid.New().String(),
		CreatedAt:            now,
		UpdatedAt:            now,
	}
	_, err = q.CreatePaymentMethod(ctx, gen.CreatePaymentMethodParams{
		ID:                   paymentMethod.ID,
		MerchantID:           dbtest.TestMerchantID.UUID(),
		CustomerID:           paymentMethod.CustomerID,
		Rail:                 string(paymentMethod.Rail),
		RailCustomerRef:      paymentMethod.RailCustomerRef,
		RailMethodRef:        paymentMethod.RailMethodRef,
		RebillDriver:         "openrails", // #682: legacy-imported shape, OpenRails drives rebills
		InitialTransactionID: paymentMethod.InitialTransactionID,
		CreatedAt:            now,
		UpdatedAt:            now,
	})
	require.NoError(t, err)

	periodEnd := now.Add(-1 * time.Minute)
	periodStart := periodEnd.Add(-30 * 24 * time.Hour)
	nextRetry := now.Add(-30 * time.Second)

	sub := &models.Subscription{
		ID:                    subID,
		CustomerID:            tenantSubjectID,
		ProductID:             productID,
		PriceID:               priceID,
		Status:                models.StatusPastDue,
		Rail:                  models.RailNMI,
		RailSubscriptionID:    "sub_test_" + uuid.New().String(),
		PaymentMethodID:       &paymentMethodID,
		CurrentPeriodStartsAt: &periodStart,
		CurrentPeriodEndsAt:   &periodEnd,
		StartedAt:             periodStart,
		NextRetryAt:           &nextRetry,
		CreatedAt:             now,
		UpdatedAt:             now,
		Price:                 price,
		PaymentMethod:         paymentMethod,
	}
	_, err = q.CreateSubscription(ctx, gen.CreateSubscriptionParams{
		ID:                    sub.ID,
		MerchantID:            dbtest.TestMerchantID.UUID(),
		CustomerID:            sub.CustomerID,
		ProductID:             sub.ProductID,
		PriceID:               &priceID,
		Status:                string(sub.Status),
		Rail:                  string(sub.Rail),
		RailSubscriptionID:    sub.RailSubscriptionID,
		PaymentMethodID:       sub.PaymentMethodID,
		CurrentPeriodStartsAt: sub.CurrentPeriodStartsAt,
		CurrentPeriodEndsAt:   sub.CurrentPeriodEndsAt,
		StartedAt:             sub.StartedAt,
		NextRetryAt:           sub.NextRetryAt,
		CreatedAt:             now,
		UpdatedAt:             now,
	})
	require.NoError(t, err)

	t.Cleanup(func() {
		// money_blocks + money_transactions were dropped (#512); the ledger is
		// append-only and this test uses a unique customer, so there is no money
		// state to reset here.
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.payments WHERE subscription_id = $1", subID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.subscriptions WHERE id = $1", subID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.payment_methods WHERE id = $1", paymentMethodID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.prices WHERE id = $1", priceID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.products WHERE id = $1", productID)
	})

	// Stub NMI direct post endpoint for AttemptManualRebill.
	railTxnID := "txn_test_" + uuid.New().String()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		_, _ = w.Write([]byte("response=1&transactionid=" + railTxnID))
	}))
	t.Cleanup(srv.Close)

	client, err := nmi.NewClient("mobius", &config.NMIProviderSettings{
		SecurityKey:   "test_security_key",
		WebhookSecret: "test_secret",
	}, true)
	require.NoError(t, err)
	client.DirectPostURL = srv.URL
	client.QueryURL = srv.URL

	worker := &DunningWorker{
		DB:          dbi,
		NMIResolver: fakeDunningNMIResolver{client: client},
	}

	priceSvc := catalog.NewPriceService(dbi)
	productSvc := catalog.NewProductService(dbi)
	entitlementSvc := entitlements.NewEntitlementService(dbi, nil)
	notifSvc := subscriptions.NewNotificationService(dbi, nil)
	paymentSvc := payments.NewPaymentService(dbi, nil)
	lifecycle := subscriptions.NewSubscriptionLifecycleService(dbi, productSvc, priceSvc, entitlementSvc, notifSvc, paymentSvc, nil)
	moneySvc := money.NewMoneyService(dbi, nil)

	mctx := dbtest.WithTestMerchant(ctx)
	require.Equal(t, dunningOutcomeSucceeded, worker.processSubscription(mctx, sub, lifecycle, priceSvc, moneySvc, false))

	// The successful rebill granted the renewal's 100 USD credit lot exactly once.
	// Post-#512/#514 a credit grant materializes a SINGLE ledger deposit and the
	// balance is DERIVED (the old money_transactions 'deposit'/'subscription_renewal'
	// row is now a ledger_transfers deposit). Balance read through the money service
	// is schema-rewritten; a double grant would read 200.
	bal, err := moneySvc.GetBalanceForCustomer(mctx, identity.CustomerID(tenantSubjectID), "USD")
	require.NoError(t, err)
	require.Equal(t, int64(100), bal.Balance, "renewal granted the 100 USD credit lot exactly once")
}

// TestDunningWorker_ConflictRepairFromDurableSuccessfulIntent pins the
// crash-repair contract that the manual_rebill_attempts table used to provide
// and the ledger now does (#358 phase C): a previous pass charged the period
// (durable succeeded intent with the transaction id as evidence) but died
// before the lifecycle update. The next pass's enqueue conflicts on the
// content-addressed key, gets the durable success back, repairs the local
// lifecycle — and never sends a second charge.
func TestDunningWorker_ConflictRepairFromDurableSuccessfulIntent(t *testing.T) {
	dsn := dbtest.SharedPostgresDSN(t)

	ctx := context.Background()
	dbi := dbtest.OpenAppDB(t, dsn)
	pool := dbtest.SharedMerchantPool(t, dbtest.TestMerchantID.UUID())
	q := gen.New(pool)
	dbtest.EnsureTestMerchant(ctx, t, pool)

	now := time.Now().UTC().Truncate(time.Second)

	productID := uuid.New()
	priceID := uuid.New()
	paymentMethodID := uuid.New()
	subID := uuid.New()
	userID := uuid.New().String()

	billingHours32 := int32(30 * 24)
	description := "Test"
	_, err := q.CreateProduct(ctx, gen.CreateProductParams{
		ID:          productID,
		Key:         "test_product_" + uuid.New().String(),
		DisplayName: "Test Product",
		MerchantID:  dbtest.TestMerchantID.UUID(),
		Description: &description,
		Archived:    false,
		CreatedAt:   now,
		UpdatedAt:   now,
	})
	require.NoError(t, err)
	_, err = q.CreatePrice(ctx, gen.CreatePriceParams{
		ID: priceID, ProductID: productID, Amount: 999, Currency: "USD", MerchantID: dbtest.TestMerchantID.UUID(),
		Archived: false, AccessDurationHours: &billingHours32, AutoRenew: true,
		CreatedAt: now, UpdatedAt: now,
	})
	require.NoError(t, err)

	billingID := "bill_" + uuid.New().String()
	tenantSubjectID := dbtest.EnsureCustomerIDPgx(ctx, t, pool, userID)
	_, err = q.CreatePaymentMethod(ctx, gen.CreatePaymentMethodParams{
		ID: paymentMethodID, MerchantID: dbtest.TestMerchantID.UUID(), CustomerID: tenantSubjectID, Rail: "nmi",
		RailCustomerRef: "vault_" + uuid.New().String(), RailMethodRef: billingID,
		RebillDriver:         "openrails", // #682: legacy-imported shape, OpenRails drives rebills
		InitialTransactionID: "txn_initial_" + uuid.New().String(),
		CreatedAt:            now, UpdatedAt: now,
	})
	require.NoError(t, err)

	periodEnd := now.Add(-1 * time.Minute)
	periodStart := periodEnd.Add(-30 * 24 * time.Hour)
	nextRetry := now.Add(-30 * time.Second)
	_, err = q.CreateSubscription(ctx, gen.CreateSubscriptionParams{
		ID: subID, MerchantID: dbtest.TestMerchantID.UUID(), CustomerID: tenantSubjectID, ProductID: productID, PriceID: &priceID,
		Status: string(models.StatusPastDue), Rail: "nmi",
		RailSubscriptionID:    "sub_test_" + uuid.New().String(),
		PaymentMethodID:       &paymentMethodID,
		CurrentPeriodStartsAt: &periodStart, CurrentPeriodEndsAt: &periodEnd,
		StartedAt: periodStart, NextRetryAt: &nextRetry,
		CreatedAt: now, UpdatedAt: now,
	})
	require.NoError(t, err)

	// The durable success from the crashed pass: attempt ordinal 0
	// (retry_attempts is NULL), evidence carries the charge's transaction id.
	durableTxn := "txn_durable_" + uuid.NewString()[:8]
	orderRef := fmt.Sprintf("rebill-%s-%d", subID, periodEnd.Unix())
	payloadJSON := fmt.Sprintf(
		`{"subscription_id":%q,"period_end":%q,"rail":"nmi","order_reference":%q,"attempt":0}`,
		subID, periodEnd.Format(time.RFC3339), orderRef)
	_, err = pool.Exec(ctx, `
		INSERT INTO openrails.rail_intents
		  (rail, intent_type, subscription_id, payload, idempotency_key, status, origin, executed_at, result_evidence, merchant_id)
		VALUES ('nmi', $1, $2, $3, $4, 'succeeded', 'system', now(), $5, $6)`,
		intents.TypeManualRebill, subID, payloadJSON,
		intents.ManualRebillIdempotencyKey(subID, periodEnd, "nmi", orderRef, 0),
		fmt.Sprintf(`{"transaction_id": %q}`, durableTxn), dbtest.TestMerchantID.UUID())
	require.NoError(t, err)

	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.rail_intents WHERE subscription_id = $1", subID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.payments WHERE subscription_id = $1", subID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.subscriptions WHERE id = $1", subID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.payment_methods WHERE id = $1", paymentMethodID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.prices WHERE id = $1", priceID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.products WHERE id = $1", productID)
	})

	// Gateway that REFUSES sales: any charge attempt fails the test premise.
	var saleAttempts int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.Form.Get("type") == "sale" {
			saleAttempts++
		}
		_, _ = w.Write([]byte("response=3&responsetext=should never be called"))
	}))
	t.Cleanup(srv.Close)
	client, err := nmi.NewClient("mobius", &config.NMIProviderSettings{
		SecurityKey:   "test_security_key",
		WebhookSecret: "test_secret",
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

	sub, err := subscriptions.NewSubscriptionRepo(dbi).GetByID(ctx, subID)
	require.NoError(t, err)

	outcome := worker.processSubscription(dbtest.WithTestMerchant(ctx), sub, lifecycle, priceSvc, moneySvc, false)
	require.Equal(t, dunningOutcomeSucceeded, outcome)
	assert.Zero(t, saleAttempts, "the durable success must be repaired, never re-charged")

	refreshed, err := dbi.Gen(ctx).GetSubscriptionByID(ctx, subID)
	require.NoError(t, err)
	assert.Equal(t, string(models.StatusActive), string(refreshed.Status), "lifecycle repaired from the ledger")
	require.NotNil(t, refreshed.CurrentPeriodEndsAt)
	assert.True(t, refreshed.CurrentPeriodEndsAt.After(periodEnd))

	var paymentCount int
	require.NoError(t, pool.QueryRow(ctx,
		"SELECT count(*) FROM openrails.payments WHERE subscription_id = $1 AND transaction_id = $2",
		subID, durableTxn).Scan(&paymentCount))
	assert.Equal(t, 1, paymentCount, "repair persisted the durable charge")
}
