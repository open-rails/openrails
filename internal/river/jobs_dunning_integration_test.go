//go:build integration

package riverjobs

import (
	"context"
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
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/internal/modules/entitlements"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/pkg/identity"
	"github.com/stretchr/testify/require"
)

func TestDunningWorker_RebillSuccessWithoutBundledCredits(t *testing.T) {
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
	productID := uuid.New()
	priceID := uuid.New()
	paymentMethodID := uuid.New()
	subID := uuid.New()
	userID := uuid.New().String()

	billingDays := 720
	billingDays32 := int32(billingDays)

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

	price := &models.Price{
		ID:                  priceID,
		ProductID:           productID,
		Archived:            false,
		Amount:              9_990_000,
		Currency:            "USD",
		AccessDurationHours: &billingDays,
		AutoRenew:           true,
		CreatedAt:           now,
		UpdatedAt:           now,
	}
	_, err = q.CreatePrice(ctx, gen.CreatePriceParams{
		ID:                  priceID,
		ProductID:           productID,
		Amount:              9_990_000,
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
	pspID := dbtest.EnsureTestPSP(ctx, t, pool, dbtest.TestMerchantID.UUID(), "nmi")
	paymentMethod := &models.PaymentMethod{
		ID:                   paymentMethodID,
		CustomerID:           tenantSubjectID,
		Rail:                 models.RailNMI,
		PspID:                pspID,
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
		PspID:                pspID,
		RailCustomerRef:      paymentMethod.RailCustomerRef,
		RailMethodRef:        paymentMethod.RailMethodRef,
		RebillDriver:         "openrails", // #682: legacy-imported shape, OpenRails drives rebills
		InitialTransactionID: paymentMethod.InitialTransactionID,
		CreatedAt:            now,
		UpdatedAt:            now,
	})
	require.NoError(t, err)
	dbtest.SeedNMIStoredCredentialRefs(ctx, t, pool, paymentMethodID)

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
		PspID:                 pspID,
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
		PspID:                 pspID,
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
	srv := httptest.NewServer(withRebillReceipts(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		_, _ = w.Write([]byte("response=1&transactionid=" + railTxnID))
	}), "9.99", "USD"))
	t.Cleanup(srv.Close)

	client, err := nmi.NewClient("mobius", &config.NMIProviderSettings{
		SecurityKey:   "test_security_key",
		WebhookSecret: "test_secret",
	}, true)
	require.NoError(t, err)
	client.DirectPostURL = srv.URL
	client.QueryURL = srv.URL
	client.V5BaseURL = srv.URL

	worker := &DunningWorker{
		DB:          dbi,
		NMIResolver: fakeDunningNMIResolver{client: client},
		// or#865: the worker's self-assembled intent Runner parks every intent
		// when no mode is stated — this fixture drives real rebills, so it
		// says "full".
		Config: fullModeConfig(),
	}

	priceSvc := catalog.NewPriceService(dbi)
	productSvc := catalog.NewProductService(dbi)
	entitlementSvc := entitlements.NewEntitlementService(dbi, nil)
	notifSvc := subscriptions.NewNotificationService(dbi, nil)
	paymentSvc := payments.NewPaymentService(dbi, nil)
	lifecycle := subscriptions.NewSubscriptionLifecycleService(dbi, productSvc, priceSvc, entitlementSvc, notifSvc, paymentSvc, nil)
	moneySvc := money.NewMoneyService(dbi, nil)

	// Same shape as the production Work loop: a merchant in the Go context is
	// not enough — the pass must run on a merchant-scoped CONNECTION, or every
	// read the charge path makes matches merchant_id = NULL under the FORCEd RLS
	// and the outcome silently comes back as "nothing to do".
	mctx := dbtest.WithTestMerchant(ctx)
	var outcome dunningOutcome
	require.NoError(t, dbi.RunInMerchantConn(mctx, func(sctx context.Context) error {
		var processErr error
		outcome, processErr = worker.processSubscription(sctx, sub, lifecycle, priceSvc, false)
		return processErr
	}))
	require.Equal(t, dunningOutcomeSucceeded, outcome)

	// A successful subscription renewal does not create a bundled balance.
	var bal *models.MoneyBalance
	require.NoError(t, dbi.RunInMerchantConn(mctx, func(sctx context.Context) error {
		var e error
		bal, e = moneySvc.GetBalanceForCustomer(sctx, identity.CustomerID(tenantSubjectID), "USD")
		return e
	}))
	require.Equal(t, int64(0), bal.Balance, "subscription renewals do not create bundled balances")
}
