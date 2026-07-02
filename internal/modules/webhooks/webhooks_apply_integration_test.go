//go:build integration

package webhooks

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/internal/modules/entitlements"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/stretchr/testify/require"
)

// #675 apply-layer integration tests: out-of-order/concurrent webhook delivery
// must not revert committed state, and money-bearing events must never ACK
// with zero durable effect.

type stripeApplyFixture struct {
	svc             *StripeWebhookService
	dbi             *db.DB
	pool            *pgxpool.Pool
	userID          string
	tenantSubjectID uuid.UUID
	productID       uuid.UUID
	priceID         uuid.UUID
	subID           uuid.UUID
	railSubID       string
	subSvc          *subscriptions.SubscriptionService
}

// newStripeApplyFixture seeds product+price+subscription and builds a
// StripeWebhookService wired to real services (no dedup layer — handlers are
// exercised directly).
func newStripeApplyFixture(t *testing.T, ctx context.Context, dbi *db.DB, pool *pgxpool.Pool, subStatus models.SubscriptionStatus, cancelType *models.CancelType, periodEnd time.Time) *stripeApplyFixture {
	t.Helper()
	q := gen.New(pool)
	now := time.Now().UTC().Truncate(time.Second)

	f := &stripeApplyFixture{
		dbi:       dbi,
		pool:      pool,
		userID:    uuid.New().String(),
		productID: uuid.New(),
		priceID:   uuid.New(),
		subID:     uuid.New(),
		railSubID: "sub_apply_" + uuid.New().String(),
	}
	f.tenantSubjectID = dbtest.EnsureCustomerIDPgx(ctx, t, pool, f.userID)

	description := "Test"
	billingDays := int32(720)
	_, err := q.CreateProduct(ctx, gen.CreateProductParams{
		ID:          f.productID,
		MerchantID:  dbtest.TestMerchantID.UUID(),
		Key:         "apply_product_" + uuid.New().String(),
		DisplayName: "Apply Product",
		Description: &description,
		Status:      string(models.CatalogStatusActive),
		CreatedAt:   now,
		UpdatedAt:   now,
	})
	require.NoError(t, err)
	_, err = q.CreatePrice(ctx, gen.CreatePriceParams{
		ID:                  f.priceID,
		MerchantID:          dbtest.TestMerchantID.UUID(),
		ProductID:           f.productID,
		Amount:              29_990_000, // 2999 cents
		Currency:            "usd",
		Status:              string(models.CatalogStatusActive),
		AccessDurationHours: &billingDays,
		AutoRenew:           true,
		CreatedAt:           now,
		UpdatedAt:           now,
	})
	require.NoError(t, err)

	periodStart := now.Add(-25 * 24 * time.Hour)
	var cancelTypeStr *string
	var cancelledAt *time.Time
	if cancelType != nil {
		ct := string(*cancelType)
		cancelTypeStr = &ct
		cancelledAt = &now
	}
	_, err = q.CreateSubscription(ctx, gen.CreateSubscriptionParams{
		ID:                    f.subID,
		MerchantID:            dbtest.TestMerchantID.UUID(),
		CustomerID:            f.tenantSubjectID,
		ProductID:             f.productID,
		PriceID:               &f.priceID,
		Status:                string(subStatus),
		Rail:                  string(models.RailStripe),
		RailSubscriptionID:    f.railSubID,
		CurrentPeriodStartsAt: &periodStart,
		CurrentPeriodEndsAt:   &periodEnd,
		CancelType:            cancelTypeStr,
		CancelledAt:           cancelledAt,
		StartedAt:             periodStart,
		CreatedAt:             now,
		UpdatedAt:             now,
	})
	require.NoError(t, err)

	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.notification_queue WHERE data->>'rail_subscription_id' = $1", f.railSubID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.entitlements WHERE customer_id = $1", f.tenantSubjectID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.payments WHERE customer_id = $1", f.tenantSubjectID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.subscriptions WHERE id = $1", f.subID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.prices WHERE id = $1", f.priceID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.products WHERE id = $1", f.productID)
	})

	priceSvc := catalog.NewPriceService(dbi)
	productSvc := catalog.NewProductService(dbi)
	entitlementSvc := entitlements.NewEntitlementService(dbi)
	paymentSvc := payments.NewPaymentService(dbi)
	notifSvc := subscriptions.NewNotificationService(dbi, nil)
	lifecycle := subscriptions.NewSubscriptionLifecycleService(dbi, productSvc, priceSvc, entitlementSvc, notifSvc, paymentSvc, nil)
	f.subSvc = subscriptions.NewSubscriptionService(dbi, priceSvc, productSvc, nil, nil, nil)

	f.svc = &StripeWebhookService{
		DB:                           dbi,
		PriceService:                 priceSvc,
		ProductService:               productSvc,
		SubscriptionService:          f.subSvc,
		SubscriptionLifecycleService: lifecycle,
		NotificationService:          notifSvc,
		PaymentService:               paymentSvc,
	}
	return f
}

func (f *stripeApplyFixture) reload(t *testing.T, ctx context.Context) *models.Subscription {
	t.Helper()
	sub, err := f.subSvc.GetByRailSubscriptionID(ctx, string(models.RailStripe), f.railSubID)
	require.NoError(t, err)
	return sub
}

func (f *stripeApplyFixture) invoicePaidObj(txnSuffix string, periodStart, periodEnd time.Time) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(`{
		"id": "in_%s",
		"subscription": %q,
		"customer": "cus_apply",
		"amount_paid": 2999,
		"currency": "usd",
		"charge": "ch_%s",
		"metadata": {"user_id": %q, "internal_price_id": %q},
		"lines": {"data": [{"period": {"start": %d, "end": %d}, "price": {"id": "price_stripe_apply"}}]}
	}`, txnSuffix, f.railSubID, txnSuffix, f.userID, f.priceID, periodStart.Unix(), periodEnd.Unix()))
}

func (f *stripeApplyFixture) subscriptionUpdatedObj(status string, periodEnd time.Time) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(`{
		"id": %q,
		"status": %q,
		"current_period_end": %d
	}`, f.railSubID, status, periodEnd.Unix()))
}

func (f *stripeApplyFixture) paymentFailedObj(suffix string) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(`{
		"id": "in_%s",
		"subscription": %q,
		"amount_due": 2999,
		"currency": "usd",
		"payment_intent": "pi_%s"
	}`, suffix, f.railSubID, suffix))
}

// A stale subscription.updated(active) delivered AFTER invoice.payment_failed
// must not flip past_due back to active without payment.
func TestStripeStaleUpdatedCannotRevertPastDue(t *testing.T) {
	dsn := dbtest.SharedPostgresDSN(t)
	ctx := dbtest.WithTestMerchant(context.Background())
	dbi := dbtest.OpenAppDB(t, dsn)
	pool := dbi.Pool()

	periodEnd := time.Now().UTC().Add(5 * 24 * time.Hour).Truncate(time.Second)
	f := newStripeApplyFixture(t, ctx, dbi, pool, models.StatusActive, nil, periodEnd)

	// payment_failed at event-created 2000.
	require.NoError(t, f.svc.handleInvoicePaymentFailed(ctx, f.paymentFailedObj("fail1"), 2000))
	require.Equal(t, models.StatusPastDue, f.reload(t, ctx).Status)

	// Stale updated(active) created 1000 — must be ignored.
	require.NoError(t, f.svc.handleSubscriptionUpdated(ctx, f.subscriptionUpdatedObj("active", periodEnd), 1000))
	require.Equal(t, models.StatusPastDue, f.reload(t, ctx).Status, "stale subscription.updated(active) reactivated a past_due subscription")

	// Newer updated(active) created 3000 — applies (recovery).
	require.NoError(t, f.svc.handleSubscriptionUpdated(ctx, f.subscriptionUpdatedObj("active", periodEnd), 3000))
	require.Equal(t, models.StatusActive, f.reload(t, ctx).Status)

	// The failed attempt row is the durable trace of the failed payment.
	var failedRows int
	require.NoError(t, pool.QueryRow(ctx,
		"SELECT count(*) FROM openrails.payments WHERE customer_id = $1 AND status = 'failed'",
		f.tenantSubjectID).Scan(&failedRows))
	require.Equal(t, 1, failedRows)
}

// Renewal (invoice.paid) and a stale subscription.updated carrying the OLD
// period must converge on the renewed period in either apply order.
func TestStripeInvoicePaidVsStaleUpdatedBothOrders(t *testing.T) {
	dsn := dbtest.SharedPostgresDSN(t)
	ctx := dbtest.WithTestMerchant(context.Background())
	dbi := dbtest.OpenAppDB(t, dsn)
	pool := dbi.Pool()

	now := time.Now().UTC().Truncate(time.Second)
	oldEnd := now.Add(5 * 24 * time.Hour)
	newEnd := now.Add(35 * 24 * time.Hour)

	t.Run("paid then stale updated", func(t *testing.T) {
		f := newStripeApplyFixture(t, ctx, dbi, pool, models.StatusActive, nil, oldEnd)
		require.NoError(t, f.svc.handleInvoicePaid(ctx, f.invoicePaidObj("a1", now, newEnd), 2000))
		require.True(t, f.reload(t, ctx).CurrentPeriodEndsAt.Equal(newEnd), "renewal did not extend the period")

		require.NoError(t, f.svc.handleSubscriptionUpdated(ctx, f.subscriptionUpdatedObj("active", oldEnd), 1000))
		sub := f.reload(t, ctx)
		require.True(t, sub.CurrentPeriodEndsAt.Equal(newEnd), "stale subscription.updated reverted the renewed period end to %v", sub.CurrentPeriodEndsAt)
		require.Equal(t, models.StatusActive, sub.Status)
	})

	t.Run("stale updated then paid", func(t *testing.T) {
		f := newStripeApplyFixture(t, ctx, dbi, pool, models.StatusActive, nil, oldEnd)
		require.NoError(t, f.svc.handleSubscriptionUpdated(ctx, f.subscriptionUpdatedObj("active", oldEnd), 1000))
		require.NoError(t, f.svc.handleInvoicePaid(ctx, f.invoicePaidObj("b1", now, newEnd), 2000))
		sub := f.reload(t, ctx)
		require.True(t, sub.CurrentPeriodEndsAt.Equal(newEnd), "renewal after an older updated did not win: %v", sub.CurrentPeriodEndsAt)
	})
}

// A renewal charge blocked by a terminal local subscription must leave a
// durable repair alert (customer WAS charged) instead of a silent ACK.
func TestStripeTerminalBlockedRenewalWritesRepairAlert(t *testing.T) {
	dsn := dbtest.SharedPostgresDSN(t)
	ctx := dbtest.WithTestMerchant(context.Background())
	dbi := dbtest.OpenAppDB(t, dsn)
	pool := dbi.Pool()

	now := time.Now().UTC().Truncate(time.Second)
	cancelType := models.CancelTypeChargeback
	f := newStripeApplyFixture(t, ctx, dbi, pool, models.StatusCancelled, &cancelType, now.Add(-24*time.Hour))

	err := f.svc.handleInvoicePaid(ctx, f.invoicePaidObj("tb1", now, now.Add(30*24*time.Hour)), 2000)
	require.NoError(t, err, "terminal-blocked renewal must ACK after recording the repair alert")

	require.Equal(t, models.StatusCancelled, f.reload(t, ctx).Status, "terminal subscription must stay cancelled")

	var alerts int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM openrails.notification_queue
		 WHERE data->>'operation' = 'terminal_blocked_renewal_success'
		   AND data->>'transaction_id' = 'ch_tb1'`).Scan(&alerts))
	require.Equal(t, 1, alerts, "terminal-blocked renewal left no durable trace")
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.notification_queue WHERE data->>'transaction_id' = 'ch_tb1'")
	})
}

// A CCBill void racing the sale must return a retryable error (redelivery
// wins once the sale materializes), never a plain ACK.
func TestCCBillVoidBeforeSaleReturnsRetryableError(t *testing.T) {
	dsn := dbtest.SharedPostgresDSN(t)
	ctx := dbtest.WithTestMerchant(context.Background())
	dbi := dbtest.OpenAppDB(t, dsn)

	body, err := json.Marshal(CCBillVoidEvent{
		TransactionID:  "void_txn_" + uuid.New().String(),
		SubscriptionID: "ccbill_missing_" + uuid.New().String(),
		ClientAccnum:   "1234",
		ClientSubacc:   "0000",
		Timestamp:      time.Now().UTC().Format("2006-01-02 15:04:05"),
		Amount:         "9.99",
		CurrencyCode:   "840",
		Reason:         "fraud void",
	})
	require.NoError(t, err)

	svc := &CCBillWebhookService{
		DB:   dbi,
		Data: CCBillWebhookEvent{EventType: EventTypeVoid, EventBody: body},
	}
	err = svc.handleVoid(ctx)
	require.Error(t, err, "void before sale must not ACK")
	require.False(t, IsWebhookErrorNonRetryable(err), "void before sale must stay retryable")
}

// Same property for CCBill chargebacks.
func TestCCBillChargebackBeforeSaleReturnsRetryableError(t *testing.T) {
	dsn := dbtest.SharedPostgresDSN(t)
	ctx := dbtest.WithTestMerchant(context.Background())
	dbi := dbtest.OpenAppDB(t, dsn)

	body, err := json.Marshal(CCBillChargebackEvent{
		TransactionID:  "cb_txn_" + uuid.New().String(),
		SubscriptionID: "ccbill_missing_" + uuid.New().String(),
		ClientAccnum:   "1234",
		ClientSubacc:   "0000",
		Timestamp:      time.Now().UTC().Format("2006-01-02 15:04:05"),
		Amount:         "9.99",
		CurrencyCode:   "840",
		Reason:         "chargeback",
	})
	require.NoError(t, err)

	svc := &CCBillWebhookService{
		DB:   dbi,
		Data: CCBillWebhookEvent{EventType: EventTypeChargeback, EventBody: body},
	}
	err = svc.handleChargeback(ctx)
	require.Error(t, err, "chargeback before sale must not ACK")
	require.False(t, IsWebhookErrorNonRetryable(err), "chargeback before sale must stay retryable")
}

// An NMI void whose original payment is unknown must return a retryable error
// regardless of whether the subscription resolved.
func TestNMIVoidBeforeSaleReturnsRetryableError(t *testing.T) {
	dsn := dbtest.SharedPostgresDSN(t)
	ctx := dbtest.WithTestMerchant(context.Background())
	dbi := dbtest.OpenAppDB(t, dsn)

	body, err := json.Marshal(map[string]any{"transaction_id": "void_missing_" + uuid.New().String()})
	require.NoError(t, err)

	svc := &NMIWebhookService{
		DB:             dbi,
		Rail:           string(models.RailNMI),
		PaymentService: payments.NewPaymentService(dbi),
		Data:           NMIWebhookEvent{EventID: uuid.New().String(), EventType: EventTypeNMIVoidSuccess, EventBody: body},
	}
	err = svc.handleVoidSuccess(ctx)
	require.Error(t, err, "void with unresolved original payment must not ACK")
	require.False(t, IsWebhookErrorNonRetryable(err))
}

// An NMI refund of a one-time purchase (no subscription anywhere in the
// payload) must reverse the original payment — previously it ACKed with zero
// effect because the reversal block was gated on subscription != nil.
func TestNMIOneOffRefundReversesPayment(t *testing.T) {
	dsn := dbtest.SharedPostgresDSN(t)
	ctx := dbtest.WithTestMerchant(context.Background())
	dbi := dbtest.OpenAppDB(t, dsn)
	pool := dbi.Pool()
	q := gen.New(pool)

	now := time.Now().UTC().Truncate(time.Second)
	userID := uuid.New().String()
	tenantSubjectID := dbtest.EnsureCustomerIDPgx(ctx, t, pool, userID)
	productID := uuid.New()
	priceID := uuid.New()

	description := "Test"
	_, err := q.CreateProduct(ctx, gen.CreateProductParams{
		ID:          productID,
		MerchantID:  dbtest.TestMerchantID.UUID(),
		Key:         "refund_product_" + uuid.New().String(),
		DisplayName: "Refund Product",
		Description: &description,
		Status:      string(models.CatalogStatusActive),
		CreatedAt:   now,
		UpdatedAt:   now,
	})
	require.NoError(t, err)
	_, err = q.CreatePrice(ctx, gen.CreatePriceParams{
		ID:         priceID,
		MerchantID: dbtest.TestMerchantID.UUID(),
		ProductID:  productID,
		Amount:     19_990_000,
		Currency:   "usd",
		Status:     string(models.CatalogStatusActive),
		CreatedAt:  now,
		UpdatedAt:  now,
	})
	require.NoError(t, err)

	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.payments WHERE customer_id = $1", tenantSubjectID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.prices WHERE id = $1", priceID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.products WHERE id = $1", productID)
	})

	paymentSvc := payments.NewPaymentService(dbi)
	originalTxnID := "orig_" + uuid.New().String()
	refundTxnID := "refund_" + uuid.New().String()
	original := &models.Payment{
		ID:            uuid.New(),
		CustomerID:    tenantSubjectID,
		PriceID:       priceID,
		Rail:          models.RailNMI,
		TransactionID: originalTxnID,
		Amount:        19_990_000,
		ListAmount:    19_990_000,
		Currency:      "usd",
		Status:        payments.PaymentStatusCompletedValue,
		PurchasedAt:   now,
		CreatedAt:     now,
	}
	require.NoError(t, paymentSvc.Create(ctx, original))

	refundBody, err := json.Marshal(map[string]any{
		"transaction_id": refundTxnID,
		"amount":         "19.99",
		"currency":       "USD",
		"transaction":    map[string]any{"transaction_id": originalTxnID},
	})
	require.NoError(t, err)

	svc := &NMIWebhookService{
		DB:             dbi,
		Rail:           string(models.RailNMI),
		PaymentService: paymentSvc,
		Data:           NMIWebhookEvent{EventID: uuid.New().String(), EventType: EventTypeNMIRefundSuccess, EventBody: refundBody},
	}
	require.NoError(t, svc.handleRefundSuccess(ctx))

	reversal, err := paymentSvc.GetByTransactionID(ctx, models.RailNMI, refundTxnID)
	require.NoError(t, err, "one-off refund left no payment reversal row")
	require.NotNil(t, reversal.RefundedPaymentID)
	require.Equal(t, original.ID, *reversal.RefundedPaymentID)
	require.Equal(t, int64(-19_990_000), reversal.Amount)

	// Redelivery is a no-op (idempotent).
	require.NoError(t, svc.handleRefundSuccess(ctx))
	total, err := paymentSvc.GetRefundTotalByPaymentID(ctx, original.ID)
	require.NoError(t, err)
	require.Equal(t, int64(19_990_000), total)
}

// An NMI one-off refund whose sale has not landed yet must return a retryable
// error so redelivery wins the race.
func TestNMIOneOffRefundBeforeSaleRetries(t *testing.T) {
	dsn := dbtest.SharedPostgresDSN(t)
	ctx := dbtest.WithTestMerchant(context.Background())
	dbi := dbtest.OpenAppDB(t, dsn)

	refundBody, err := json.Marshal(map[string]any{
		"transaction_id": "refund_race_" + uuid.New().String(),
		"amount":         "19.99",
		"currency":       "USD",
		"transaction":    map[string]any{"transaction_id": "orig_never_seen_" + uuid.New().String()},
	})
	require.NoError(t, err)

	svc := &NMIWebhookService{
		DB:             dbi,
		Rail:           string(models.RailNMI),
		PaymentService: payments.NewPaymentService(dbi),
		Data:           NMIWebhookEvent{EventID: uuid.New().String(), EventType: EventTypeNMIRefundSuccess, EventBody: refundBody},
	}
	err = svc.handleRefundSuccess(ctx)
	require.Error(t, err, "refund before sale must not ACK")
	require.False(t, IsWebhookErrorNonRetryable(err))
}

// A transient/config credit-grant failure must fail the event (retry) instead
// of warn-and-ack losing the period's credit lot.
func TestCCBillRenewalCreditGrantFailurePropagates(t *testing.T) {
	dsn := dbtest.SharedPostgresDSN(t)
	ctx := dbtest.WithTestMerchant(context.Background())
	dbi := dbtest.OpenAppDB(t, dsn)
	pool := dbi.Pool()
	q := gen.New(pool)

	now := time.Now().UTC().Truncate(time.Second)
	billingDays := int32(30)
	productID := uuid.New()
	priceID := uuid.New()
	subID := uuid.New()
	userID := uuid.New().String()
	tenantSubjectID := dbtest.EnsureCustomerIDPgx(ctx, t, pool, userID)
	ccbillSubID := "ccbill_sub_" + uuid.New().String()

	// Invalid unit: GrantSubscriptionCredits rejects it, and the handler must
	// now propagate that instead of ACKing the renewal.
	creditsSpecJSON, err := json.Marshal(models.CreditsSpec{
		"broken_credits": {Unit: "NOPE", Amount: 100, Cadence: models.CreditGrantCadencePerRenewal},
	})
	require.NoError(t, err)
	description := "Test"
	_, err = q.CreateProduct(ctx, gen.CreateProductParams{
		ID:          productID,
		MerchantID:  dbtest.TestMerchantID.UUID(),
		Key:         "broken_credit_product_" + uuid.New().String(),
		DisplayName: "Broken Credit Product",
		Description: &description,
		CreditsSpec: creditsSpecJSON,
		Status:      string(models.CatalogStatusActive),
		CreatedAt:   now,
		UpdatedAt:   now,
	})
	require.NoError(t, err)
	_, err = q.CreatePrice(ctx, gen.CreatePriceParams{
		ID:                  priceID,
		MerchantID:          dbtest.TestMerchantID.UUID(),
		ProductID:           productID,
		Amount:              9_990_000,
		Currency:            "usd",
		Status:              string(models.CatalogStatusActive),
		AccessDurationHours: &billingDays,
		AutoRenew:           true,
		CreatedAt:           now,
		UpdatedAt:           now,
	})
	require.NoError(t, err)
	periodEnd := now.Add(30 * 24 * time.Hour)
	periodStart := now
	_, err = q.CreateSubscription(ctx, gen.CreateSubscriptionParams{
		ID:                    subID,
		MerchantID:            dbtest.TestMerchantID.UUID(),
		CustomerID:            tenantSubjectID,
		ProductID:             productID,
		PriceID:               &priceID,
		Status:                string(models.StatusActive),
		Rail:                  string(models.RailCCBill),
		RailSubscriptionID:    ccbillSubID,
		CurrentPeriodStartsAt: &periodStart,
		CurrentPeriodEndsAt:   &periodEnd,
		StartedAt:             now,
		CreatedAt:             now,
		UpdatedAt:             now,
	})
	require.NoError(t, err)

	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.ledger_transfers WHERE customer_id = $1", tenantSubjectID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.grants WHERE customer_id = $1", tenantSubjectID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.payments WHERE subscription_id = $1", subID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.subscriptions WHERE id = $1", subID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.prices WHERE id = $1", priceID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.products WHERE id = $1", productID)
	})

	priceSvc := catalog.NewPriceService(dbi)
	productSvc := catalog.NewProductService(dbi)
	entitlementSvc := entitlements.NewEntitlementService(dbi)
	paymentSvc := payments.NewPaymentService(dbi)
	lifecycle := subscriptions.NewSubscriptionLifecycleService(dbi, productSvc, priceSvc, entitlementSvc, nil, paymentSvc, nil)
	subSvc := subscriptions.NewSubscriptionService(dbi, priceSvc, productSvc, nil, nil, nil)

	body, err := json.Marshal(CCBillRenewalSuccessEvent{
		TransactionID:      "txn_" + uuid.New().String(),
		SubscriptionID:     ccbillSubID,
		ClientAccnum:       "1234",
		ClientSubacc:       "0000",
		Timestamp:          now.Format("2006-01-02 15:04:05"),
		BilledAmount:       "9.99",
		BilledCurrencyCode: "usd",
		NextRenewalDate:    now.Add(60 * 24 * time.Hour).Format("2006-01-02"),
	})
	require.NoError(t, err)

	svc := &CCBillWebhookService{
		Data:                         CCBillWebhookEvent{EventType: EventTypeRenewalSuccess, EventBody: body},
		DB:                           dbi,
		SubscriptionService:          subSvc,
		SubscriptionLifecycleService: lifecycle,
		MoneyService:                 money.NewMoneyService(dbi),
	}
	err = svc.handleRenewalSuccess(ctx)
	require.Error(t, err, "credit grant failure must fail the event for retry, not warn-and-ack")
	require.False(t, IsWebhookErrorNonRetryable(err))
}
