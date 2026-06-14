//go:build integration

package intents

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/integrations/nmi"
)

// fakeNMIRebillGateway scripts the gateway for rebill flows: the Direct Post
// API answers recurring=rebill_subscription sales, the Query API answers the
// order_id transaction search.
type fakeNMIRebillGateway struct {
	saleBody   atomic.Value // string Direct Post response
	saleStatus atomic.Int64 // optional HTTP status (0 = 200)
	charged    atomic.Bool  // query reports a successful sale for the order id
	saleCalls  atomic.Int64
	queryCalls atomic.Int64
	txnID      string
}

func newFakeNMIRebillGateway(t *testing.T) (*fakeNMIRebillGateway, *nmi.NMIClient) {
	t.Helper()
	f := &fakeNMIRebillGateway{txnID: "txn-rebill-" + uuid.NewString()[:8]}
	f.saleBody.Store("response=1&transactionid=" + f.txnID)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.Form.Get("report_type") == "transaction" || r.URL.Query().Get("report_type") == "transaction" {
			f.queryCalls.Add(1)
			orderID := r.Form.Get("order_id")
			if orderID == "" {
				orderID = r.URL.Query().Get("order_id")
			}
			if f.charged.Load() {
				fmt.Fprintf(w, `<nm_response><transaction><transaction_id>%s</transaction_id><order_id>%s</order_id><action><action_type>sale</action_type><success>1</success></action></transaction></nm_response>`, f.txnID, orderID)
			} else {
				fmt.Fprint(w, `<nm_response></nm_response>`)
			}
			return
		}
		if r.Form.Get("type") == "sale" {
			f.saleCalls.Add(1)
			if st := f.saleStatus.Load(); st != 0 {
				w.WriteHeader(int(st))
				return
			}
			_, _ = w.Write([]byte(f.saleBody.Load().(string)))
			return
		}
		_, _ = w.Write([]byte("response=1"))
	}))
	t.Cleanup(srv.Close)

	client, err := nmi.NewClient("mobius", &config.NMIProviderSettings{
		SecurityKey:   "test_security_key",
		WebhookSecret: "test_secret",
	}, true)
	require.NoError(t, err)
	client.DirectPostURL = srv.URL
	client.QueryURL = srv.URL
	return f, client
}

type rebillFixture struct {
	db        *db.DB
	store     *Store
	subID     uuid.UUID
	periodEnd time.Time
	orderRef  string
}

// seedPastDueSubscription inserts product/price/payment-method/subscription
// in the dunning posture: past_due, missed period end in the recent past.
func seedPastDueSubscription(t *testing.T) rebillFixture {
	t.Helper()
	ctx := context.Background()
	dsn := dbtest.SharedPostgresDSN(t)
	dbi := dbtest.OpenAppDB(t, dsn)
	pool := dbi.Pool()

	fx := rebillFixture{db: dbi, store: NewStore(dbi)}
	fx.subID = uuid.New()
	now := time.Now().UTC().Truncate(time.Second)
	fx.periodEnd = now.Add(-time.Minute)
	fx.orderRef = fmt.Sprintf("rebill-%s-%d", fx.subID, fx.periodEnd.Unix())

	userID := dbtest.EnsureCustomerIDPgx(ctx, t, pool, uuid.NewString())
	productID := uuid.New()
	priceID := uuid.New()
	paymentMethodID := uuid.New()
	suffix := uuid.NewString()[:8]

	exec := func(sql string, args ...any) {
		t.Helper()
		_, err := pool.Exec(ctx, sql, args...)
		require.NoError(t, err)
	}
	tenantID := dbtest.TestTenantID.UUID()
	exec(`INSERT INTO openrails.products (id, slug, display_name, merchant_id) VALUES ($1, $2, $2, $3)`,
		productID, "rebill-prod-"+suffix, tenantID)
	exec(`INSERT INTO openrails.prices (id, product_id, amount, currency, billing_cycle_days, merchant_id)
	      VALUES ($1, $2, 999, 'usd', 30, $3)`, priceID, productID, tenantID)
	exec(`INSERT INTO openrails.payment_methods (id, customer_id, processor, vault_id, billing_id, initial_transaction_id, merchant_id)
	      VALUES ($1, $2, 'mobius', $3, $4, $5, $6)`,
		paymentMethodID, userID, "vault-"+suffix, "bill-"+suffix, "txn-init-"+suffix, tenantID)
	exec(`INSERT INTO openrails.subscriptions
	        (id, price_id, product_id, status, processor, processor_subscription_id, payment_method_id,
	         current_period_starts_at, current_period_ends_at, started_at, next_retry_at, retry_attempts, customer_id, merchant_id)
	      VALUES ($1, $2, $3, 'past_due', 'mobius', $4, $5, $6, $7, $6, $8, 1, $9, $10)`,
		fx.subID, priceID, productID, "psid-"+suffix, paymentMethodID,
		fx.periodEnd.Add(-30*24*time.Hour), fx.periodEnd, now.Add(-30*time.Second), userID, tenantID)

	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.provider_intents WHERE subscription_id = $1", fx.subID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.notification_queue WHERE customer_id = $1", userID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.payments WHERE subscription_id = $1", fx.subID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.entitlements WHERE customer_id = $1", userID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.subscriptions WHERE id = $1", fx.subID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.payment_methods WHERE id = $1", paymentMethodID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.prices WHERE id = $1", priceID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.products WHERE id = $1", productID)
	})
	return fx
}

func (fx rebillFixture) enqueueParams(attempt int) EnqueueParams {
	subID := fx.subID
	windowEnd := fx.periodEnd.Add(14 * 24 * time.Hour)
	return EnqueueParams{
		MerchantID:     dbtest.TestTenantID.UUID(),
		Provider:       "mobius",
		IntentType:     TypeManualRebill,
		SubscriptionID: &subID,
		Payload: ManualRebillPayload{
			SubscriptionID: fx.subID,
			PeriodEnd:      fx.periodEnd,
			Processor:      "mobius",
			OrderReference: fx.orderRef,
			Attempt:        attempt,
		},
		IdempotencyKey: ManualRebillIdempotencyKey(fx.subID, fx.periodEnd, "mobius", fx.orderRef, attempt),
		NextAttemptAt:  time.Now().UTC(),
		Origin:         OriginSystem,
		OriginReason:   "integration test",
		ExpiresAt:      &windowEnd,
	}
}

func (fx rebillFixture) rebillRunner(client *nmi.NMIClient, cfg *config.Config) *Runner {
	return &Runner{
		Store:    fx.store,
		Registry: NewRegistry(NewManualRebillHandler(fx.db, cfg, map[string]*nmi.NMIClient{"mobius": client}, nil, nil)),
		Config:   cfg,
	}
}

func (fx rebillFixture) subscription(t *testing.T) gen.OpenrailsSubscription {
	t.Helper()
	row, err := fx.db.Gen(context.Background()).GetSubscriptionByID(context.Background(), fx.subID)
	require.NoError(t, err)
	return row
}

func (fx rebillFixture) intentByID(t *testing.T, id uuid.UUID) gen.OpenrailsProviderIntent {
	t.Helper()
	row, err := fx.db.Gen(context.Background()).GetProviderIntent(context.Background(), id)
	require.NoError(t, err)
	return row
}

// TestManualRebillSynchronousSuccessRenewsLifecycle: the worker-facing path —
// enqueue+execute approves the charge and the handler's finalize renews the
// membership (period advances, payment row recorded).
func TestManualRebillSynchronousSuccessRenewsLifecycle(t *testing.T) {
	fx := seedPastDueSubscription(t)
	fake, client := newFakeNMIRebillGateway(t)

	row, err := fx.rebillRunner(client, fullModeConfig()).EnqueueAndExecute(context.Background(), fx.enqueueParams(1))
	require.NoError(t, err)
	assert.Equal(t, StatusSucceeded, row.Status)
	assert.Contains(t, string(row.ResultEvidence), fake.txnID)
	assert.EqualValues(t, 1, fake.saleCalls.Load())

	sub := fx.subscription(t)
	assert.Equal(t, "active", string(sub.Status), "finalize renews the membership")
	require.NotNil(t, sub.CurrentPeriodEndsAt)
	assert.True(t, sub.CurrentPeriodEndsAt.After(fx.periodEnd), "period advanced")

	var paymentCount int
	require.NoError(t, fx.db.Pool().QueryRow(context.Background(),
		"SELECT count(*) FROM openrails.payments WHERE subscription_id = $1 AND transaction_id = $2",
		fx.subID, fake.txnID).Scan(&paymentCount))
	assert.Equal(t, 1, paymentCount, "renewal persisted the charge")
}

// TestManualRebillSystemOriginParksUnderLimitedThenDrains pins the designed
// mode behavior: dunning charges are system-origin and WAIT under limited;
// when full mode returns the scheduled executor charges and renews.
func TestManualRebillSystemOriginParksUnderLimitedThenDrains(t *testing.T) {
	fx := seedPastDueSubscription(t)
	fake, client := newFakeNMIRebillGateway(t)

	row, err := fx.rebillRunner(client, limitedModeConfig()).EnqueueAndExecute(context.Background(), fx.enqueueParams(1))
	require.NoError(t, err)
	assert.Equal(t, StatusPending, row.Status)
	require.NotNil(t, row.LastFailureReason)
	assert.Contains(t, *row.LastFailureReason, "mode=limited")
	assert.Zero(t, fake.saleCalls.Load(), "nothing charged under limited")
	assert.Equal(t, "past_due", string(fx.subscription(t).Status))

	_, err = fx.db.Pool().Exec(context.Background(),
		"UPDATE openrails.provider_intents SET next_attempt_at = now() WHERE id = $1", row.ID)
	require.NoError(t, err)
	_, err = fx.rebillRunner(client, fullModeConfig()).RunExecuteOnce(context.Background())
	require.NoError(t, err)

	assert.Equal(t, StatusSucceeded, fx.intentByID(t, row.ID).Status)
	assert.Equal(t, "active", string(fx.subscription(t).Status), "drain repaired the lifecycle without a waiting worker")
}

// TestManualRebillAmbiguousVerifyLateSuccessRepairsLifecycle is the core
// money-mover regression: the charge's outcome is lost mid-flight, the
// verifier later finds the sale by order reference at NMI, and the intent's
// finalize repairs the subscription lifecycle FROM THE VERIFIER — no dunning
// worker involved, and no second charge ever sent.
func TestManualRebillAmbiguousVerifyLateSuccessRepairsLifecycle(t *testing.T) {
	fx := seedPastDueSubscription(t)
	fake, client := newFakeNMIRebillGateway(t)
	fake.saleStatus.Store(http.StatusBadGateway) // outcome lost mid-flight

	row, err := fx.rebillRunner(client, fullModeConfig()).EnqueueAndExecute(context.Background(), fx.enqueueParams(1))
	require.NoError(t, err)
	require.Equal(t, StatusUnknownNeedsVerify, row.Status, "a possibly-sent charge must verify, never blind-retry")
	assert.Equal(t, "past_due", string(fx.subscription(t).Status), "no lifecycle change while unresolved")

	// The charge actually landed at NMI.
	fake.charged.Store(true)
	_, err = fx.db.Pool().Exec(context.Background(),
		"UPDATE openrails.provider_intents SET next_attempt_at = now() WHERE id = $1", row.ID)
	require.NoError(t, err)
	_, err = fx.rebillRunner(client, fullModeConfig()).RunVerifyOnce(context.Background())
	require.NoError(t, err)

	got := fx.intentByID(t, row.ID)
	assert.Equal(t, StatusSucceeded, got.Status)
	assert.Contains(t, string(got.ResultEvidence), `"verified_existing": true`)
	assert.EqualValues(t, 1, fake.saleCalls.Load(), "exactly one charge attempt ever reached the gateway")

	sub := fx.subscription(t)
	assert.Equal(t, "active", string(sub.Status), "late-confirmed success repaired the lifecycle from the verifier")
	require.NotNil(t, sub.CurrentPeriodEndsAt)
	assert.True(t, sub.CurrentPeriodEndsAt.After(fx.periodEnd))
}

// TestManualRebillDeclineIsTerminalWithEvidence: a clean decline terminates
// the ATTEMPT, preserving the response code as evidence for the worker's
// hard/soft classification; the lifecycle is the worker's call, not the
// handler's.
func TestManualRebillDeclineIsTerminalWithEvidence(t *testing.T) {
	fx := seedPastDueSubscription(t)
	fake, client := newFakeNMIRebillGateway(t)
	fake.saleBody.Store("response=2&responsetext=Stolen card&response_code=252")

	row, err := fx.rebillRunner(client, fullModeConfig()).EnqueueAndExecute(context.Background(), fx.enqueueParams(1))
	require.NoError(t, err)
	assert.Equal(t, StatusFailedTerminal, row.Status)
	assert.Contains(t, string(row.ResultEvidence), `"response_code": 252`)
	assert.Equal(t, "past_due", string(fx.subscription(t).Status), "decline lifecycle handling belongs to the dunning worker")
}

// TestManualRebillRecoveredSubscriptionSupersedes: a parked charge for a
// subscription that recovered (webhook rebill, user fix) is superseded by the
// relevance check instead of firing a stale charge.
func TestManualRebillRecoveredSubscriptionSupersedes(t *testing.T) {
	fx := seedPastDueSubscription(t)
	fake, client := newFakeNMIRebillGateway(t)

	row, err := fx.rebillRunner(client, limitedModeConfig()).EnqueueAndExecute(context.Background(), fx.enqueueParams(1))
	require.NoError(t, err)
	require.Equal(t, StatusPending, row.Status)

	// The subscription recovers while the intent waits for full mode.
	_, err = fx.db.Pool().Exec(context.Background(),
		"UPDATE openrails.subscriptions SET status = 'active', next_retry_at = NULL WHERE id = $1", fx.subID)
	require.NoError(t, err)

	_, err = fx.db.Pool().Exec(context.Background(),
		"UPDATE openrails.provider_intents SET next_attempt_at = now() WHERE id = $1", row.ID)
	require.NoError(t, err)
	_, err = fx.rebillRunner(client, fullModeConfig()).RunExecuteOnce(context.Background())
	require.NoError(t, err)

	got := fx.intentByID(t, row.ID)
	assert.Equal(t, StatusSuperseded, got.Status)
	assert.Zero(t, fake.saleCalls.Load(), "a recovered subscription must never be re-charged")
}

// TestManualRebillWindowExpiryNeverFires: an intent that outlives the dunning
// window (mode outage longer than the window) expires instead of charging a
// stale period.
func TestManualRebillWindowExpiryNeverFires(t *testing.T) {
	fx := seedPastDueSubscription(t)
	fake, client := newFakeNMIRebillGateway(t)

	params := fx.enqueueParams(1)
	expired := time.Now().Add(-time.Hour).UTC()
	params.ExpiresAt = &expired

	row, err := fx.rebillRunner(client, fullModeConfig()).EnqueueAndExecute(context.Background(), params)
	require.NoError(t, err)
	// The synchronous claim refuses expired intents; the executor sweep
	// expires the row.
	require.NotEqual(t, StatusSucceeded, row.Status)

	_, err = fx.rebillRunner(client, fullModeConfig()).RunExecuteOnce(context.Background())
	require.NoError(t, err)

	got := fx.intentByID(t, row.ID)
	assert.Equal(t, StatusExpired, got.Status)
	assert.Zero(t, fake.saleCalls.Load(), "expired dunning charges never fire")
	assert.Equal(t, "past_due", string(fx.subscription(t).Status))
}
