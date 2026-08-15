//go:build integration

package webhooks

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/modules/paymentmethods"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/modules/replaycache"
	"github.com/stretchr/testify/require"
)

func TestStripePortalPaymentMethodChangesConvergeFromProviderTruth(t *testing.T) {
	dbi := dbtest.OpenMerchantDB(t, dbtest.TestMerchantID.UUID())
	pool := dbi.Pool()
	baseCtx := dbtest.WithTestMerchant(context.Background())
	pspID := dbtest.EnsureTestPSP(baseCtx, t, pool, dbtest.TestMerchantID.UUID(), string(models.RailStripe))
	ctx := db.WithPSPID(baseCtx, pspID)

	reader := newStripePaymentStateFake()
	reader.methods["pm_old"] = stripeRemoteMethod("pm_old", "cus_portal", "1111")
	reader.methods["pm_new"] = stripeRemoteMethod("pm_new", "cus_portal", "2222")
	reader.customers["cus_portal"] = stripeRemoteCustomer("cus_portal", "sub_portal", reader.methods["pm_old"])

	userID := uuid.NewString()
	customerID := dbtest.EnsureCustomerIDPgx(ctx, t, pool, userID)
	require.NoError(t, payments.NewRailCustomerService(dbi).Upsert(ctx, userID, string(models.RailStripe), "cus_portal"))
	productID := createStripePaymentStateProduct(t, ctx, pool)
	subscriptionID := createStripePaymentStateSubscription(t, ctx, pool, pspID, customerID, productID, "sub_portal", nil)
	eventPrefix := "evt_stripe_state_" + uuid.NewString()
	t.Cleanup(func() {
		_, _ = pool.Exec(baseCtx, "DELETE FROM openrails.reconciliation_findings WHERE subject_key = $1", pspID.String()+":pm_new")
		_, _ = pool.Exec(baseCtx, "DELETE FROM openrails.webhook_events WHERE event_id LIKE $1", eventPrefix+"%")
		_, _ = pool.Exec(baseCtx, "DELETE FROM openrails.subscriptions WHERE id = $1", subscriptionID)
		_, _ = pool.Exec(baseCtx, "DELETE FROM openrails.payment_methods WHERE customer_id = $1", customerID)
		_, _ = pool.Exec(baseCtx, "DELETE FROM openrails.rail_customer_accounts WHERE customer_id = $1", customerID)
		_, _ = pool.Exec(baseCtx, "DELETE FROM openrails.products WHERE id = $1", productID)
	})

	idem := replaycache.NewStore(nil)
	t.Cleanup(idem.Close)
	dedup, err := NewDeduplicationService(idem, dbi)
	require.NoError(t, err)
	service := &StripeWebhookService{DB: dbi, DeduplicationService: dedup, StripePaymentState: reader}

	deliverStripePaymentStateEvent(t, ctx, service, eventPrefix+"_initial", "customer.updated", map[string]any{"id": "cus_portal"}, nil)
	oldMethod := requireStripeMethod(t, ctx, dbi, pspID, "pm_old")
	require.Equal(t, oldMethod.ID, *requireSubscription(t, ctx, pool, subscriptionID).PaymentMethodID)

	// A successful charge can use a non-default method. It may fill payment
	// history card details, but it must not infer or replace the subscription's
	// provider-owned default.
	deliverStripePaymentStateEvent(t, ctx, service, eventPrefix+"_charge", "charge.succeeded", map[string]any{
		"id": "ch_1", "customer": "cus_portal", "payment_method": "pm_new",
		"payment_method_details": map[string]any{"card": map[string]any{
			"brand": "visa", "last4": "2222", "exp_month": 12, "exp_year": 2030,
		}},
	}, nil)
	require.Equal(t, oldMethod.ID, *requireSubscription(t, ctx, pool, subscriptionID).PaymentMethodID)
	_, err = paymentmethods.NewPaymentMethodRepo(dbi).GetByRailMethodRefForPSP(ctx, string(models.RailStripe), pspID, "pm_new")
	require.ErrorIs(t, err, paymentmethods.ErrPaymentMethodNotFound)

	// Portal replacement: the event is only a wake-up signal. The fetched
	// subscription default selects the newly attached method.
	reader.customers["cus_portal"] = stripeRemoteCustomer("cus_portal", "sub_portal", reader.methods["pm_new"])
	deliverStripePaymentStateEvent(t, ctx, service, eventPrefix+"_attach", "payment_method.attached", map[string]any{
		"id": "pm_new", "customer": "cus_portal",
	}, nil)
	newMethod := requireStripeMethod(t, ctx, dbi, pspID, "pm_new")
	require.Equal(t, newMethod.ID, *requireSubscription(t, ctx, pool, subscriptionID).PaymentMethodID)

	// Detaching the current method parks evidence and converges the link to
	// Stripe's current fallback. The detached event omits its former customer;
	// the exact-PSP local mapping recovers that identity for the provider read.
	delete(reader.methods, "pm_new")
	reader.customers["cus_portal"] = stripeRemoteCustomer("cus_portal", "sub_portal", reader.methods["pm_old"])
	detachEventID := eventPrefix + "_detach"
	deliverStripePaymentStateEvent(t, ctx, service, detachEventID, "payment_method.detached", map[string]any{
		"id": "pm_new", "customer": nil,
	}, nil)
	newMethod = requireStripeMethod(t, ctx, dbi, pspID, "pm_new")
	require.Equal(t, payments.StripeDetachedParkReason, newMethod.ParkReason)
	require.Equal(t, oldMethod.ID, *requireSubscription(t, ctx, pool, subscriptionID).PaymentMethodID)
	require.Equal(t, int64(1), stripeDetachedFindingCount(t, ctx, pool, pspID, "pm_new"))

	// A stale attached delivery after detach cannot revive the method because
	// current Stripe truth no longer returns it.
	deliverStripePaymentStateEvent(t, ctx, service, eventPrefix+"_stale_attach", "payment_method.attached", map[string]any{
		"id": "pm_new", "customer": "cus_portal",
	}, nil)
	require.Equal(t, payments.StripeDetachedParkReason, requireStripeMethod(t, ctx, dbi, pspID, "pm_new").ParkReason)
	require.Equal(t, oldMethod.ID, *requireSubscription(t, ctx, pool, subscriptionID).PaymentMethodID)

	// Replaying the exact detach event is stopped by durable webhook dedup.
	callsBeforeReplay := reader.customerCallCount()
	deliverStripePaymentStateEvent(t, ctx, service, detachEventID, "payment_method.detached", map[string]any{
		"id": "pm_new", "customer": nil,
	}, nil)
	require.Equal(t, callsBeforeReplay, reader.customerCallCount())
	require.Equal(t, int64(1), stripeDetachedFindingCount(t, ctx, pool, pspID, "pm_new"))

	// A provider customer without a local mapping is a safe no-op, not a retry
	// loop and not a fabricated local customer or payment method.
	reader.notFoundCustomers["cus_unknown"] = true
	methodCountBefore := countStripeMethods(t, ctx, pool, pspID)
	deliverStripePaymentStateEvent(t, ctx, service, eventPrefix+"_unknown", "customer.updated", map[string]any{"id": "cus_unknown"}, nil)
	require.Equal(t, methodCountBefore, countStripeMethods(t, ctx, pool, pspID))
}

func TestStripeDetachIsIsolatedToExactPSP(t *testing.T) {
	dbi := dbtest.OpenMerchantDB(t, dbtest.TestMerchantID.UUID())
	pool := dbi.Pool()
	baseCtx := dbtest.WithTestMerchant(context.Background())
	pspA := dbtest.EnsureTestPSP(baseCtx, t, pool, dbtest.TestMerchantID.UUID(), string(models.RailStripe))
	pspB := uuid.New()
	_, err := pool.Exec(baseCtx, `
		INSERT INTO openrails.psps (id, merchant_id, rail, environment, account_id, key, archived)
		VALUES ($1, $2, 'stripe', 'test', $3, $4, false)`,
		pspB, dbtest.TestMerchantID.UUID(), "acct_test_"+uuid.NewString(), "stripe_test_"+uuid.NewString())
	require.NoError(t, err)

	ctxA := db.WithPSPID(baseCtx, pspA)
	ctxB := db.WithPSPID(baseCtx, pspB)
	userID := uuid.NewString()
	customerID := dbtest.EnsureCustomerIDPgx(ctxA, t, pool, userID)
	customers := payments.NewRailCustomerService(dbi)
	require.NoError(t, customers.Upsert(ctxA, userID, string(models.RailStripe), "cus_shared"))
	require.NoError(t, customers.Upsert(ctxB, userID, string(models.RailStripe), "cus_shared"))
	card := &payments.StripeCard{Brand: "Visa", Last4: "4242", Expiry: "12/30"}
	methodA, err := payments.UpsertStripeCardForCustomer(ctxA, dbi, customers, nil, "cus_shared", "pm_shared", "txn_a", card)
	require.NoError(t, err)
	methodB, err := payments.UpsertStripeCardForCustomer(ctxB, dbi, customers, nil, "cus_shared", "pm_shared", "txn_b", card)
	require.NoError(t, err)
	require.NotEqual(t, methodA.ID, methodB.ID)
	require.Equal(t, pspA, methodA.PspID)
	require.Equal(t, pspB, methodB.PspID)

	productA := createStripePaymentStateProduct(t, ctxA, pool)
	productB := createStripePaymentStateProduct(t, ctxB, pool)
	subA := createStripePaymentStateSubscription(t, ctxA, pool, pspA, customerID, productA, "sub_a_"+uuid.NewString(), &methodA.ID)
	subB := createStripePaymentStateSubscription(t, ctxB, pool, pspB, customerID, productB, "sub_b_"+uuid.NewString(), &methodB.ID)
	t.Cleanup(func() {
		_, _ = pool.Exec(baseCtx, "DELETE FROM openrails.subscriptions WHERE id = ANY($1::uuid[])", []uuid.UUID{subA, subB})
		_, _ = pool.Exec(baseCtx, "DELETE FROM openrails.payment_methods WHERE id = ANY($1::uuid[])", []uuid.UUID{methodA.ID, methodB.ID})
		_, _ = pool.Exec(baseCtx, "DELETE FROM openrails.rail_customer_accounts WHERE customer_id = $1", customerID)
		_, _ = pool.Exec(baseCtx, "DELETE FROM openrails.products WHERE id = ANY($1::uuid[])", []uuid.UUID{productA, productB})
		_, _ = pool.Exec(baseCtx, "DELETE FROM openrails.psps WHERE id = $1", pspB)
	})

	parked, err := payments.ParkDetachedStripePaymentMethod(ctxA, dbi, "pm_shared")
	require.NoError(t, err)
	require.Equal(t, methodA.ID, parked.ID)
	require.Equal(t, payments.StripeDetachedParkReason, requireStripeMethod(t, ctxA, dbi, pspA, "pm_shared").ParkReason)
	require.Empty(t, requireStripeMethod(t, ctxB, dbi, pspB, "pm_shared").ParkReason)
	require.Nil(t, requireSubscription(t, ctxA, pool, subA).PaymentMethodID)
	require.Equal(t, methodB.ID, *requireSubscription(t, ctxB, pool, subB).PaymentMethodID)
}

type stripePaymentStateFake struct {
	mu                sync.Mutex
	methods           map[string]*payments.StripePaymentMethodState
	customers         map[string]*payments.StripeCustomerPaymentState
	notFoundCustomers map[string]bool
	customerCalls     int
}

func newStripePaymentStateFake() *stripePaymentStateFake {
	return &stripePaymentStateFake{
		methods:           make(map[string]*payments.StripePaymentMethodState),
		customers:         make(map[string]*payments.StripeCustomerPaymentState),
		notFoundCustomers: make(map[string]bool),
	}
}

func (f *stripePaymentStateFake) PaymentMethod(_ context.Context, id string) (*payments.StripePaymentMethodState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.methods[id], nil
}

func (f *stripePaymentStateFake) CustomerPaymentState(_ context.Context, id string) (*payments.StripeCustomerPaymentState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.customerCalls++
	if f.notFoundCustomers[id] {
		return nil, payments.ErrStripeObjectNotFound
	}
	return f.customers[id], nil
}

func (f *stripePaymentStateFake) customerCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.customerCalls
}

func stripeRemoteMethod(id, customerID, last4 string) *payments.StripePaymentMethodState {
	return &payments.StripePaymentMethodState{
		ID: id, CustomerID: customerID,
		Card: &payments.StripeCard{Brand: "Visa", Last4: last4, Expiry: "12/30"},
	}
}

func stripeRemoteCustomer(customerID, subscriptionID string, method *payments.StripePaymentMethodState) *payments.StripeCustomerPaymentState {
	return &payments.StripeCustomerPaymentState{
		CustomerID: customerID,
		Subscriptions: []payments.StripeSubscriptionPaymentState{{
			SubscriptionID: subscriptionID, PaymentMethod: method,
		}},
	}
}

func deliverStripePaymentStateEvent(
	t *testing.T,
	ctx context.Context,
	service *StripeWebhookService,
	eventID string,
	eventType string,
	object any,
	previous any,
) {
	t.Helper()
	data := map[string]any{"object": object}
	if previous != nil {
		data["previous_attributes"] = previous
	}
	payload, err := json.Marshal(map[string]any{"id": eventID, "type": eventType, "data": data})
	require.NoError(t, err)
	require.NoError(t, service.HandleStripeWebhook(ctx, payload))
}

func createStripePaymentStateProduct(t *testing.T, ctx context.Context, pool gen.DBTX) uuid.UUID {
	t.Helper()
	now := time.Now().UTC()
	id := uuid.New()
	description := "Stripe payment-state test"
	_, err := gen.New(pool).CreateProduct(ctx, gen.CreateProductParams{
		ID: id, MerchantID: dbtest.TestMerchantID.UUID(), Key: "stripe_state_" + uuid.NewString(),
		DisplayName: "Stripe state", Description: &description, CreatedAt: now, UpdatedAt: now,
	})
	require.NoError(t, err)
	return id
}

func createStripePaymentStateSubscription(
	t *testing.T,
	ctx context.Context,
	pool gen.DBTX,
	pspID uuid.UUID,
	customerID uuid.UUID,
	productID uuid.UUID,
	railSubscriptionID string,
	paymentMethodID *uuid.UUID,
) uuid.UUID {
	t.Helper()
	now := time.Now().UTC()
	id := uuid.New()
	_, err := gen.New(pool).CreateSubscription(ctx, gen.CreateSubscriptionParams{
		ID: id, MerchantID: dbtest.TestMerchantID.UUID(), CustomerID: customerID,
		ProductID: productID, Status: string(models.StatusActive), StartedAt: now,
		Rail: string(models.RailStripe), RailSubscriptionID: railSubscriptionID,
		PaymentMethodID: paymentMethodID, PspID: pspID, CreatedAt: now, UpdatedAt: now,
	})
	require.NoError(t, err)
	return id
}

func requireStripeMethod(t *testing.T, ctx context.Context, dbi *db.DB, pspID uuid.UUID, methodRef string) *models.PaymentMethod {
	t.Helper()
	method, err := paymentmethods.NewPaymentMethodRepo(dbi).GetByRailMethodRefForPSP(
		db.WithPSPID(ctx, pspID), string(models.RailStripe), pspID, methodRef,
	)
	require.NoError(t, err)
	return method
}

func requireSubscription(t *testing.T, ctx context.Context, pool gen.DBTX, id uuid.UUID) gen.OpenrailsSubscription {
	t.Helper()
	subscription, err := gen.New(pool).GetSubscriptionByID(ctx, id)
	require.NoError(t, err)
	return subscription
}

func stripeDetachedFindingCount(t *testing.T, ctx context.Context, pool gen.DBTX, pspID uuid.UUID, methodRef string) int64 {
	t.Helper()
	var count int64
	err := pool.QueryRow(ctx, `
		SELECT count(*) FROM openrails.reconciliation_findings
		WHERE finding_type = 'consistency.stripe_payment_method_detached'
		  AND subject_key = $1`, pspID.String()+":"+methodRef).Scan(&count)
	require.NoError(t, err)
	return count
}

func countStripeMethods(t *testing.T, ctx context.Context, pool gen.DBTX, pspID uuid.UUID) int64 {
	t.Helper()
	var count int64
	err := pool.QueryRow(ctx, `SELECT count(*) FROM openrails.payment_methods WHERE rail = 'stripe' AND psp_id = $1`, pspID).Scan(&count)
	require.NoError(t, err)
	return count
}

var _ payments.StripePaymentStateReader = (*stripePaymentStateFake)(nil)
