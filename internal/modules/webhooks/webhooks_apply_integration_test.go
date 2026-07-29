//go:build integration

package webhooks

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
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

// #684 convergence tests, ported from the #675 apply-layer ordering tests.
// Handlers no longer carry state: every subscription-state event collapses to
// "fetch current provider truth, converge via the decider", so the old
// ordering scenarios (stale updated(active) after payment_failed; invoice.paid
// ∥ subscription.updated in either order; delayed events reverting state) are
// trivially order-free — N converges against the same truth are idempotent.
// The provider transport is a REAL httptest server speaking the Stripe wire
// shape (a transport fake, not a logic mock); the decider is the real one.

// fakeStripeAPI serves GET /v1/subscriptions/{id} with configurable truth and
// counts requests (the coalescing/fetch-count assertions).
type fakeStripeAPI struct {
	mu       sync.Mutex
	truth    map[string]string // railSubID -> subscription JSON; missing = 404
	failWith int               // != 0: every request answers this status
	requests atomic.Int64
	srv      *httptest.Server
}

func newFakeStripeAPI(t *testing.T) *fakeStripeAPI {
	t.Helper()
	f := &fakeStripeAPI{truth: map[string]string{}}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.requests.Add(1)
		f.mu.Lock()
		failWith := f.failWith
		var body string
		var ok bool
		if r.URL.Path != "" {
			id := r.URL.Path[len("/v1/subscriptions/"):]
			body, ok = f.truth[id]
		}
		f.mu.Unlock()
		if failWith != 0 {
			w.WriteHeader(failWith)
			_, _ = w.Write([]byte(`{"error":{"message":"provider down"}}`))
			return
		}
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":{"type":"invalid_request_error","message":"No such subscription"}}`))
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeStripeAPI) setTruth(railSubID, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if body == "" {
		delete(f.truth, railSubID)
	} else {
		f.truth[railSubID] = body
	}
}

func (f *fakeStripeAPI) setFailure(status int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failWith = status
}

// stripeSubscriptionTruth builds the Stripe subscription JSON (with expanded
// latest_invoice) the converge fetch consumes.
type stripeInvoiceTruth struct {
	ID            string
	Paid          bool
	AmountPaid    int64
	AmountDue     int64
	Charge        string
	PaymentIntent string
	Created       time.Time
}

func (f *stripeApplyFixture) subscriptionTruth(status string, periodStart, periodEnd time.Time, inv *stripeInvoiceTruth) string {
	m := map[string]any{
		"id":                   f.railSubID,
		"status":               status,
		"cancel_at_period_end": false,
		"current_period_start": periodStart.Unix(),
		"current_period_end":   periodEnd.Unix(),
		"customer":             f.railCustomerID,
	}
	if inv != nil {
		m["latest_invoice"] = map[string]any{
			"id":             inv.ID,
			"paid":           inv.Paid,
			"amount_paid":    inv.AmountPaid,
			"amount_due":     inv.AmountDue,
			"charge":         inv.Charge,
			"payment_intent": inv.PaymentIntent,
			"currency":       "usd",
			"created":        inv.Created.Unix(),
		}
	}
	b, _ := json.Marshal(m)
	return string(b)
}

type stripeApplyFixture struct {
	svc             *StripeConvergeService
	api             *fakeStripeAPI
	dbi             *db.DB
	pool            *pgxpool.Pool
	userID          string
	tenantSubjectID uuid.UUID
	productID       uuid.UUID
	priceID         uuid.UUID
	subID           uuid.UUID
	railSubID       string
	railCustomerID  string
	subSvc          *subscriptions.SubscriptionService
}

// newStripeApplyFixture seeds product+price+subscription and builds a
// StripeConvergeService whose prober reads the fake Stripe transport.
func newStripeApplyFixture(t *testing.T, ctx context.Context, dbi *db.DB, pool *pgxpool.Pool, subStatus models.SubscriptionStatus, cancelType *models.CancelType, periodEnd time.Time) *stripeApplyFixture {
	t.Helper()
	q := gen.New(pool)
	now := time.Now().UTC().Truncate(time.Second)

	f := &stripeApplyFixture{
		dbi:       dbi,
		pool:      pool,
		api:       newFakeStripeAPI(t),
		userID:    uuid.New().String(),
		productID: uuid.New(),
		priceID:   uuid.New(),
		subID:     uuid.New(),
		railSubID: "sub_apply_" + uuid.New().String(),
	}
	f.railCustomerID = "cus_apply_" + uuid.New().String()
	f.tenantSubjectID = dbtest.EnsureCustomerIDPgx(ctx, t, pool, f.userID)

	description := "Test"
	billingDays := int32(720)
	_, err := q.CreateProduct(ctx, gen.CreateProductParams{
		ID:          f.productID,
		MerchantID:  dbtest.TestMerchantID.UUID(),
		Key:         "apply_product_" + uuid.New().String(),
		DisplayName: "Apply Product",
		Description: &description,
		Archived:    false,
		CreatedAt:   now,
		UpdatedAt:   now,
	})
	require.NoError(t, err)
	_, err = q.CreatePrice(ctx, gen.CreatePriceParams{
		ID:                  f.priceID,
		MerchantID:          dbtest.TestMerchantID.UUID(),
		ProductID:           f.productID,
		Amount:              29_990_000, // 2999 cents
		Currency:            "USD",
		Archived:            false,
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
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.rail_customer_accounts WHERE account_id = $1", f.railCustomerID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.notification_queue WHERE customer_id = $1", f.tenantSubjectID)
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
	lifecycle := subscriptions.NewSubscriptionLifecycleService(dbi, productSvc, priceSvc, entitlementSvc, notifSvc, paymentSvc)
	f.subSvc = subscriptions.NewSubscriptionService(dbi, priceSvc, productSvc, nil, nil, nil)

	f.svc = &StripeConvergeService{
		DB:                           dbi,
		Prober:                       &subscriptions.HTTPStripeLivenessProber{SecretKey: "sk_test_fake", BaseURL: f.api.srv.URL},
		PriceService:                 priceSvc,
		ProductService:               productSvc,
		SubscriptionService:          f.subSvc,
		SubscriptionLifecycleService: lifecycle,
		PaymentService:               paymentSvc,
		NotificationService:          notifSvc,
	}
	return f
}

func (f *stripeApplyFixture) reload(t *testing.T, ctx context.Context) *models.Subscription {
	t.Helper()
	sub, err := f.subSvc.GetByRailSubscriptionID(ctx, string(models.RailStripe), f.railSubID)
	require.NoError(t, err)
	return sub
}

func (f *stripeApplyFixture) paymentCount(t *testing.T, ctx context.Context, status string) int {
	t.Helper()
	var n int
	require.NoError(t, f.pool.QueryRow(ctx,
		"SELECT count(*) FROM openrails.payments WHERE customer_id = $1 AND status = $2",
		f.tenantSubjectID, status).Scan(&n))
	return n
}

// #675 scenario 1, converged: a failed renewal followed by ANY number of
// duplicated/stale wake-ups cannot flip past_due back to active — every
// converge re-fetches the SAME truth. Recovery happens only when provider
// truth actually changes.
func TestStripeConvergeStaleEventsCannotRevertPastDue(t *testing.T) {
	ctx := dbtest.WithTestMerchant(context.Background())
	dbi := dbtest.OpenMerchantDB(t, dbtest.TestMerchantID.UUID())
	pool := dbi.Pool()

	now := time.Now().UTC().Truncate(time.Second)
	periodEnd := now.Add(5 * 24 * time.Hour)
	f := newStripeApplyFixture(t, ctx, dbi, pool, models.StatusActive, nil, periodEnd)

	// Provider truth: renewal failed — subscription past_due with an unpaid
	// latest invoice (the fetchable decline record).
	f.api.setTruth(f.railSubID, f.subscriptionTruth("past_due", now.Add(-25*24*time.Hour), periodEnd, &stripeInvoiceTruth{
		ID: "in_fail1", Paid: false, AmountDue: 2999, PaymentIntent: "pi_fail1", Created: now,
	}))

	_, err := f.svc.Converge(ctx, f.railSubID)
	require.NoError(t, err)
	require.Equal(t, models.StatusPastDue, f.reload(t, ctx).Status)
	require.Equal(t, 1, f.paymentCount(t, ctx, "failed"), "fetched decline must land as the durable failed-attempt row")

	// "Stale subscription.updated(active)" is now just another wake-up: it
	// re-fetches the SAME truth. Any order, any count — state cannot revert.
	for i := 0; i < 3; i++ {
		_, err = f.svc.Converge(ctx, f.railSubID)
		require.NoError(t, err)
	}
	require.Equal(t, models.StatusPastDue, f.reload(t, ctx).Status, "duplicate/stale wake-ups reverted past_due")
	require.Equal(t, 1, f.paymentCount(t, ctx, "failed"), "failed-attempt row must not duplicate")

	// Provider truth changes: Stripe recovered the payment (the invoice is now
	// paid, subscription active again). Convergence follows truth — a real
	// recovery, not a stale payload.
	f.api.setTruth(f.railSubID, f.subscriptionTruth("active", now.Add(-25*24*time.Hour), periodEnd, &stripeInvoiceTruth{
		ID: "in_rec1", Paid: true, AmountPaid: 2999, Charge: "ch_rec1", Created: now,
	}))
	_, err = f.svc.Converge(ctx, f.railSubID)
	require.NoError(t, err)
	sub := f.reload(t, ctx)
	require.Equal(t, models.StatusActive, sub.Status)
	require.True(t, sub.CurrentPeriodEndsAt.Equal(periodEnd), "recovery keeps the provider period end")
	require.Equal(t, 1, f.paymentCount(t, ctx, "completed"), "recovered charge backfilled exactly once")
}

// #675 scenario 2, converged: invoice.paid ∥ subscription.updated in either
// order collapse to converges against the SAME renewed truth — the period end
// is the provider's, exactly one payment row exists, in every order/count.
func TestStripeConvergeRenewalIdempotentAnyOrder(t *testing.T) {
	ctx := dbtest.WithTestMerchant(context.Background())
	dbi := dbtest.OpenMerchantDB(t, dbtest.TestMerchantID.UUID())
	pool := dbi.Pool()

	now := time.Now().UTC().Truncate(time.Second)
	oldEnd := now.Add(-1 * time.Hour) // the boundary just lapsed; Stripe billed it
	newEnd := now.Add(30 * 24 * time.Hour)
	f := newStripeApplyFixture(t, ctx, dbi, pool, models.StatusActive, nil, oldEnd)

	f.api.setTruth(f.railSubID, f.subscriptionTruth("active", oldEnd, newEnd, &stripeInvoiceTruth{
		ID: "in_b1", Paid: true, AmountPaid: 2999, Charge: "ch_b1", Created: oldEnd,
	}))

	// Both "orders" (and duplicates) are the same operation now.
	for i := 0; i < 2; i++ {
		_, err := f.svc.Converge(ctx, f.railSubID)
		require.NoError(t, err)
		sub := f.reload(t, ctx)
		require.Equal(t, models.StatusActive, sub.Status)
		require.True(t, sub.CurrentPeriodEndsAt.Equal(newEnd), "converge %d did not land the renewed period end: %v", i, sub.CurrentPeriodEndsAt)
		require.Equal(t, 1, f.paymentCount(t, ctx, "completed"), "renewal payment must exist exactly once")
	}
	require.EqualValues(t, 2, f.api.requests.Load(), "each converge is exactly one provider fetch")
}

// #675 scenario 3, converged: a renewal charge against a terminal local
// subscription cannot resurrect it (terminal rows take no transition), but the
// charge IS money truth and must leave a durable payment row.
func TestStripeConvergeTerminalRowKeepsMoneyTruth(t *testing.T) {
	ctx := dbtest.WithTestMerchant(context.Background())
	dbi := dbtest.OpenMerchantDB(t, dbtest.TestMerchantID.UUID())
	pool := dbi.Pool()

	now := time.Now().UTC().Truncate(time.Second)
	cancelType := models.CancelTypeChargeback
	f := newStripeApplyFixture(t, ctx, dbi, pool, models.StatusCancelled, &cancelType, now.Add(-24*time.Hour))

	f.api.setTruth(f.railSubID, f.subscriptionTruth("active", now.Add(-time.Hour), now.Add(30*24*time.Hour), &stripeInvoiceTruth{
		ID: "in_tb1", Paid: true, AmountPaid: 2999, Charge: "ch_tb1", Created: now.Add(-time.Hour),
	}))

	_, err := f.svc.Converge(ctx, f.railSubID)
	require.NoError(t, err)

	require.Equal(t, models.StatusCancelled, f.reload(t, ctx).Status, "terminal subscription must stay cancelled")
	var txn string
	require.NoError(t, pool.QueryRow(ctx,
		"SELECT transaction_id FROM openrails.payments WHERE customer_id = $1 AND status = 'completed'",
		f.tenantSubjectID).Scan(&txn))
	require.Equal(t, "ch_tb1", txn, "the charge against the terminal row is money truth and must be recorded")
}

// Fetch-404 IS evidence: Stripe answering "no such subscription" is
// provider-confirmed-gone, and the REAL decider turns it into a terminal
// cancel (#679 certainty ladder), never a guess.
func TestStripeConvergeFetch404IsProviderConfirmedGone(t *testing.T) {
	ctx := dbtest.WithTestMerchant(context.Background())
	dbi := dbtest.OpenMerchantDB(t, dbtest.TestMerchantID.UUID())
	pool := dbi.Pool()

	now := time.Now().UTC().Truncate(time.Second)
	f := newStripeApplyFixture(t, ctx, dbi, pool, models.StatusActive, nil, now.Add(5*24*time.Hour))
	// No truth registered: the fake answers 404.

	_, err := f.svc.Converge(ctx, f.railSubID)
	require.NoError(t, err)

	sub := f.reload(t, ctx)
	require.Equal(t, models.StatusCancelled, sub.Status, "provider-confirmed-gone must cancel through the decider")
	require.NotNil(t, sub.CancelType)
	require.Equal(t, models.CancelTypeExpired, *sub.CancelType)
}

// Provider API down: the converge attempt fails RETRYABLY — the dirty mark
// parks (the job retries), local state and access stay intact, and a later
// converge against healthy truth converges normally.
func TestStripeConvergeProviderDownParksAndRecovers(t *testing.T) {
	ctx := dbtest.WithTestMerchant(context.Background())
	dbi := dbtest.OpenMerchantDB(t, dbtest.TestMerchantID.UUID())
	pool := dbi.Pool()

	now := time.Now().UTC().Truncate(time.Second)
	periodEnd := now.Add(5 * 24 * time.Hour)
	f := newStripeApplyFixture(t, ctx, dbi, pool, models.StatusActive, nil, periodEnd)

	f.api.setFailure(http.StatusInternalServerError)
	_, err := f.svc.Converge(ctx, f.railSubID)
	require.Error(t, err, "provider outage must fail the converge for retry")
	require.Equal(t, models.StatusActive, f.reload(t, ctx).Status, "outage must not move local state; access intact")

	f.api.setFailure(0)
	f.api.setTruth(f.railSubID, f.subscriptionTruth("active", now.Add(-25*24*time.Hour), periodEnd, nil))
	_, err = f.svc.Converge(ctx, f.railSubID)
	require.NoError(t, err)
	require.Equal(t, models.StatusActive, f.reload(t, ctx).Status)
}

var _ = fmt.Sprintf // keep fmt for the CCBill/NMI sections below

// A CCBill void racing the sale must return a retryable error (redelivery
// wins once the sale materializes), never a plain ACK.
func TestCCBillVoidBeforeSaleReturnsRetryableError(t *testing.T) {
	ctx := dbtest.WithTestMerchant(context.Background())
	dbi := dbtest.OpenMerchantDB(t, dbtest.TestMerchantID.UUID())

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
	ctx := dbtest.WithTestMerchant(context.Background())
	dbi := dbtest.OpenMerchantDB(t, dbtest.TestMerchantID.UUID())

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
	ctx := dbtest.WithTestMerchant(context.Background())
	dbi := dbtest.OpenMerchantDB(t, dbtest.TestMerchantID.UUID())

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
	ctx := dbtest.WithTestMerchant(context.Background())
	dbi := dbtest.OpenMerchantDB(t, dbtest.TestMerchantID.UUID())
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
		Archived:    false,
		CreatedAt:   now,
		UpdatedAt:   now,
	})
	require.NoError(t, err)
	_, err = q.CreatePrice(ctx, gen.CreatePriceParams{
		ID:         priceID,
		MerchantID: dbtest.TestMerchantID.UUID(),
		ProductID:  productID,
		Amount:     19_990_000,
		Currency:   "USD",
		Archived:   false,
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
		Currency:      "USD",
		Status:        payments.PaymentStatusCompletedValue,
		MoneyMovement: models.MoneyMovementRail,
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
	ctx := dbtest.WithTestMerchant(context.Background())
	dbi := dbtest.OpenMerchantDB(t, dbtest.TestMerchantID.UUID())

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
	ctx := dbtest.WithTestMerchant(context.Background())
	dbi := dbtest.OpenMerchantDB(t, dbtest.TestMerchantID.UUID())
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
		Archived:    false,
		CreatedAt:   now,
		UpdatedAt:   now,
	})
	require.NoError(t, err)
	_, err = q.CreatePrice(ctx, gen.CreatePriceParams{
		ID:                  priceID,
		MerchantID:          dbtest.TestMerchantID.UUID(),
		ProductID:           productID,
		Amount:              9_990_000,
		Currency:            "USD",
		Archived:            false,
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
	lifecycle := subscriptions.NewSubscriptionLifecycleService(dbi, productSvc, priceSvc, entitlementSvc, nil, paymentSvc)
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
