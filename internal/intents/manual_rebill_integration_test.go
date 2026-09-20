//go:build integration

package intents

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/internal/modules/payments/charge"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
	"github.com/open-rails/openrails/pkg/merchant"
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
	saleBody           atomic.Value // string Direct Post response
	saleStatus         atomic.Int64 // optional HTTP status (0 = 200)
	charged            atomic.Bool  // query reports a successful sale for the order id
	saleCalls          atomic.Int64
	queryCalls         atomic.Int64
	saleAuthKey        atomic.Value // security_key the last sale authenticated with (#730)
	saleForm           atomic.Value // url.Values: full form of the last sale (#297 wire assertions)
	txnID              string
	orderID            atomic.Value
	amount             atomic.Value
	updateCalls        atomic.Int64
	updateForm         atomic.Value
	loseUpdateResponse atomic.Bool
}

func newFakeNMIRebillGateway(t *testing.T, fx rebillFixture) (*fakeNMIRebillGateway, *nmi.NMIClient) {
	t.Helper()
	f := &fakeNMIRebillGateway{txnID: "txn-rebill-" + uuid.NewString()[:8]}
	f.saleBody.Store("response=1&transactionid=" + f.txnID)
	f.orderID.Store(fx.orderRef)
	f.amount.Store("9.99")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/subscriptions/") {
			_ = json.NewEncoder(w).Encode(map[string]any{"id": fx.payload.RailSubscriptionID, "amount": f.amount.Load().(string), "customer_vault_id": fx.payload.Instrument.RailCustomerRef, "delayed_condition": "active", "paused_subscription": "0", "next_billing_date": fx.payload.Renewal.PeriodEnd.UTC().Format("2006-01-02"), "plan": map[string]any{"id": "plan-" + fx.subID.String(), "plan_amount": f.amount.Load().(string), "plan_payments": "0", "day_frequency": "30"}})
			return
		}
		if r.Method == http.MethodGet && r.URL.Path == "/payments/"+f.txnID {
			if !f.charged.Load() {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"id": f.txnID, "amount": f.amount.Load().(string), "currency": "USD", "response": "1", "customer_vault_id": fx.payload.Instrument.RailCustomerRef, "actions": []map[string]any{{"type": "sale", "amount": f.amount.Load().(string), "success": true, "response": "1"}}})
			return
		}
		_ = r.ParseForm()
		if r.Form.Get("report_type") == "transaction" || r.URL.Query().Get("report_type") == "transaction" {
			f.queryCalls.Add(1)
			orderID := r.Form.Get("order_id")
			if orderID == "" {
				orderID = r.URL.Query().Get("order_id")
			}
			if f.charged.Load() && orderID == f.orderID.Load().(string) {
				fmt.Fprintf(w, `<nm_response><transaction><transaction_id>%s</transaction_id><order_id>%s</order_id><action><action_type>sale</action_type><success>1</success></action></transaction></nm_response>`, f.txnID, orderID)
			} else {
				fmt.Fprint(w, `<nm_response></nm_response>`)
			}
			return
		}
		if r.Form.Get("recurring") == "update_subscription" {
			f.updateCalls.Add(1)
			f.updateForm.Store(r.Form)
			f.amount.Store(r.Form.Get("plan_amount"))
			if f.loseUpdateResponse.Swap(false) {
				w.WriteHeader(http.StatusBadGateway)
				return
			}
			fmt.Fprint(w, "response=1")
			return
		}
		if r.Form.Get("type") == "sale" {
			f.saleCalls.Add(1)
			f.saleAuthKey.Store(r.Form.Get("security_key"))
			f.saleForm.Store(r.Form)
			f.orderID.Store(r.Form.Get("orderid"))
			if st := f.saleStatus.Load(); st != 0 {
				w.WriteHeader(int(st))
				return
			}
			if strings.HasPrefix(f.saleBody.Load().(string), "response=1") {
				f.charged.Store(true)
			}
			_, _ = w.Write([]byte(f.saleBody.Load().(string)))
			return
		}
		_, _ = w.Write([]byte("response=1"))
	}))
	t.Cleanup(srv.Close)

	client, err := nmi.NewAccountClient(fx.merchantID, fx.pspID, "nmi", &config.NMIProviderSettings{
		SecurityKey:   "test_security_key",
		WebhookSecret: "test_secret",
	}, true)
	require.NoError(t, err)
	client.DirectPostURL = srv.URL
	client.QueryURL = srv.URL
	client.V5BaseURL = srv.URL
	return f, client
}

type rebillFixture struct {
	merchantID uuid.UUID
	db         *db.DB
	store      *Store
	subID      uuid.UUID
	periodEnd  time.Time
	orderRef   string
	pspID      uuid.UUID
	payload    ManualRebillPayload
}

// seedPastDueSubscription inserts product/price/payment-method/subscription
// in the dunning posture: past_due, missed period end in the recent past.
func seedPastDueSubscription(t *testing.T) rebillFixture {
	t.Helper()
	return seedPastDueSubscriptionForMerchant(t, dbtest.TestMerchantID.UUID())
}

func seedPastDueSubscriptionForMerchant(t *testing.T, merchantID uuid.UUID) rebillFixture {
	return seedPastDueSubscriptionAt(t, merchantID, time.Now().UTC())
}

func seedPastDueSubscriptionAt(t *testing.T, merchantID uuid.UUID, now time.Time) rebillFixture {
	t.Helper()
	ctx := merchant.WithID(context.Background(), merchant.ID(merchantID))
	dbi := dbtest.OpenMerchantDB(t, merchantID)
	pool := dbi.Pool()

	fx := rebillFixture{db: dbi, store: NewStore(dbi), merchantID: merchantID}
	_, err := pool.Exec(ctx, `INSERT INTO openrails.merchants(id,slug) VALUES($1,$2) ON CONFLICT(id) DO NOTHING`, merchantID, "rebill-"+merchantID.String())
	require.NoError(t, err)
	fx.subID = uuid.New()
	now = now.UTC().Truncate(time.Second)
	fx.periodEnd = now.Add(-time.Minute)
	fx.orderRef = rebillOrderReference(ManualRebillIdempotencyKey(fx.subID, fx.periodEnd, "nmi", 1))

	userID := uuid.New()
	_, err = pool.Exec(ctx, `INSERT INTO openrails.customers(merchant_id,id) VALUES($1,$2)`, merchantID, userID)
	require.NoError(t, err)
	productID := uuid.New()
	priceID := uuid.New()
	paymentMethodID := uuid.New()
	suffix := uuid.NewString()[:8]

	exec := func(sql string, args ...any) {
		t.Helper()
		_, err := pool.Exec(ctx, sql, args...)
		require.NoError(t, err)
	}
	tenantID := merchantID
	fx.pspID = dbtest.EnsureTestPSP(ctx, t, pool, tenantID, "nmi")
	exec(`INSERT INTO openrails.products (id, key, display_name, merchant_id, entitlements_spec) VALUES ($1, $2, $2, $3, '{"premium":null}')`,
		productID, "rebill-prod-"+suffix, tenantID)
	exec(`INSERT INTO openrails.prices (id, product_id, amount, currency, access_duration_hours, auto_renew, merchant_id)
	      VALUES ($1, $2, 9990000, 'USD', 720, true, $3)`, priceID, productID, tenantID)
	exec(`INSERT INTO openrails.payment_methods
	        (id, customer_id, rail, psp_id, rail_customer_ref, rail_method_ref,
	         initial_transaction_id, stored_credential_recurring_ref, merchant_id, rebill_driver)
	      VALUES ($1, $2, 'nmi', $3, $4, $5, $6, $7, $8, 'openrails')`,
		paymentMethodID, userID, fx.pspID, "vault-"+suffix, "bill-"+suffix,
		"txn-init-"+suffix, "txn-recurring-init-"+suffix, tenantID)
	exec(`INSERT INTO openrails.subscriptions
	        (id, price_id, product_id, status, rail, rail_subscription_id, payment_method_id,
	         current_period_starts_at, current_period_ends_at, started_at, next_retry_at, retry_attempts, customer_id, merchant_id, psp_id, entitlements_spec_snapshot)
	      VALUES ($1, $2, $3, 'past_due', 'nmi', $4, $5, $6, $7, $6, $8, 1, $9, $10, $11, '{"premium":null}')`,
		fx.subID, priceID, productID, "psid-"+suffix, paymentMethodID,
		fx.periodEnd.Add(-30*24*time.Hour), fx.periodEnd, now.Add(-30*time.Second), userID, tenantID, fx.pspID)

	ctx = merchant.WithID(ctx, merchant.ID(merchantID))
	require.NoError(t, dbi.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		d := dbi.NewWithPgxTx(tx)
		sub, err := subscriptions.NewSubscriptionRepo(d).GetByIDForUpdate(ctx, fx.subID)
		if err != nil {
			return err
		}
		terms, err := subscriptions.PrepareRenewalTerms(ctx, d, sub, now)
		if err != nil {
			return err
		}
		method, err := d.Gen(ctx).GetPaymentMethodForShare(ctx, gen.GetPaymentMethodForShareParams{MerchantID: tenantID, ID: paymentMethodID})
		if err != nil {
			return err
		}
		minor, err := moneyutil.NativeToRailMinorExact(terms.Currency, terms.Amount)
		if err != nil {
			return err
		}
		fx.payload = ManualRebillPayload{Renewal: terms, PaymentMethodID: paymentMethodID, Instrument: charge.FreezeInstrument(method), Rail: "nmi", RailSubscriptionID: sub.RailSubscriptionID, Attempt: 1, FailureCount: 1, OrderReference: fx.orderRef, AmountMinor: minor}
		return nil
	}))

	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.rail_intents WHERE subscription_id = $1", fx.subID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.notifications WHERE customer_id = $1", userID)
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
		MerchantID:     fx.merchantID,
		Provider:       fx.payload.Rail,
		PspID:          fx.pspID,
		IntentType:     TypeManualRebill,
		SubscriptionID: &subID,
		PriceID:        &fx.payload.Renewal.PriceID,
		Payload: func() ManualRebillPayload {
			p := fx.payload
			p.Attempt = attempt
			p.OrderReference = rebillOrderReference(ManualRebillIdempotencyKey(fx.subID, fx.periodEnd, fx.payload.Rail, attempt))
			return p
		}(),
		IdempotencyKey: ManualRebillIdempotencyKey(fx.subID, fx.periodEnd, fx.payload.Rail, attempt),
		NextAttemptAt:  time.Now().UTC(),
		Origin:         OriginSystem,
		OriginReason:   "integration test",
		ExpiresAt:      &windowEnd,
	}
}

func (fx rebillFixture) rebillRunner(client *nmi.NMIClient, cfg *config.Config) *Runner {
	return &Runner{
		Store:    fx.store,
		Registry: NewRegistry(NewManualRebillHandler(fx.db, cfg, fakeNMIResolver{client: client}, nil)),
		Config:   cfg,
	}
}

func (fx rebillFixture) subscription(t *testing.T) gen.OpenrailsSubscription {
	t.Helper()
	row, err := fx.db.Gen(context.Background()).GetSubscriptionByID(context.Background(), fx.subID)
	require.NoError(t, err)
	return row
}

func (fx rebillFixture) intentByID(t *testing.T, id uuid.UUID) gen.OpenrailsRailIntent {
	t.Helper()
	row, err := fx.db.Gen(context.Background()).GetRailIntent(context.Background(), id)
	require.NoError(t, err)
	return row
}

// TestManualRebillSynchronousSuccessRenewsLifecycle: the worker-facing path —
// enqueue+execute approves the charge and the handler's finalize renews the
// membership (period advances, payment row recorded).
func TestManualRebillSynchronousSuccessRenewsLifecycle(t *testing.T) {
	fx := seedPastDueSubscription(t)
	fake, client := newFakeNMIRebillGateway(t, fx)

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
	fake, client := newFakeNMIRebillGateway(t, fx)

	row, err := fx.rebillRunner(client, limitedModeConfig()).EnqueueAndExecute(context.Background(), fx.enqueueParams(1))
	require.NoError(t, err)
	assert.Equal(t, StatusPending, row.Status)
	require.NotNil(t, row.LastFailureReason)
	assert.Contains(t, *row.LastFailureReason, "mode=limited")
	assert.Zero(t, fake.saleCalls.Load(), "nothing charged under limited")
	assert.Equal(t, "past_due", string(fx.subscription(t).Status))

	_, err = fx.db.Pool().Exec(context.Background(),
		"UPDATE openrails.rail_intents SET next_attempt_at = now() WHERE id = $1", row.ID)
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
	fake, client := newFakeNMIRebillGateway(t, fx)
	fake.saleStatus.Store(http.StatusBadGateway) // outcome lost mid-flight

	row, err := fx.rebillRunner(client, fullModeConfig()).EnqueueAndExecute(context.Background(), fx.enqueueParams(1))
	require.NoError(t, err)
	require.Equal(t, StatusUnknownNeedsVerify, row.Status, "a possibly-sent charge must verify, never blind-retry")
	assert.Equal(t, "past_due", string(fx.subscription(t).Status), "no lifecycle change while unresolved")

	// A successful empty search is inconclusive and cannot re-arm execution.
	fake.charged.Store(false)
	_, err = fx.db.Pool().Exec(context.Background(), "UPDATE openrails.rail_intents SET next_attempt_at=now() WHERE id=$1", row.ID)
	require.NoError(t, err)
	_, err = fx.rebillRunner(client, fullModeConfig()).RunVerifyOnce(context.Background())
	require.NoError(t, err)
	require.Equal(t, StatusUnknownNeedsVerify, fx.intentByID(t, row.ID).Status)
	resumed := row
	resumed.Attempts = 2
	outcome := fx.rebillRunner(client, fullModeConfig()).Registry.Lookup(TypeManualRebill).Execute(context.Background(), resumed)
	require.Equal(t, OutcomeAmbiguous, outcome.Class)
	require.EqualValues(t, 1, fake.saleCalls.Load())

	// The charge actually landed at NMI.
	fake.charged.Store(true)
	_, err = fx.db.Pool().Exec(context.Background(),
		"UPDATE openrails.rail_intents SET next_attempt_at = now() WHERE id = $1", row.ID)
	require.NoError(t, err)
	_, err = fx.rebillRunner(client, fullModeConfig()).RunVerifyOnce(context.Background())
	require.NoError(t, err)

	got := fx.intentByID(t, row.ID)
	assert.Equal(t, StatusSucceeded, got.Status)
	// Terminal replay retains the qualified provider facts and accepted terms.
	assert.Contains(t, string(got.ResultEvidence), fake.txnID, "transaction_id pointer retained for the dunning repair path")
	assert.Contains(t, string(got.ResultEvidence), "qualified_receipt", "qualified custody survives terminal replay")
	assert.EqualValues(t, 1, fake.saleCalls.Load(), "exactly one charge attempt ever reached the gateway")

	sub := fx.subscription(t)
	assert.Equal(t, "active", string(sub.Status), "late-confirmed success repaired the lifecycle from the verifier")
	require.NotNil(t, sub.CurrentPeriodEndsAt)
	assert.True(t, sub.CurrentPeriodEndsAt.After(fx.periodEnd))
}

// TestManualRebillDeclineIsTerminalWithEvidence: a clean decline terminates
// the ATTEMPT, preserving the response code as evidence for the worker's
// hard/soft classification; lifecycle and terminal state commit together.
func TestManualRebillDeclineIsTerminalWithEvidence(t *testing.T) {
	fx := seedPastDueSubscription(t)
	fake, client := newFakeNMIRebillGateway(t, fx)
	fake.saleBody.Store("response=2&responsetext=Insufficient funds&response_code=202")

	row, err := fx.rebillRunner(client, fullModeConfig()).EnqueueAndExecute(context.Background(), fx.enqueueParams(1))
	require.NoError(t, err)
	assert.Equal(t, StatusFailedTerminal, row.Status)
	assert.Contains(t, string(row.ResultEvidence), `"response_code": 202`)
	assert.Equal(t, "past_due", string(fx.subscription(t).Status), "retryable refusal retains the subscription")
	assert.EqualValues(t, 2, *fx.subscription(t).RetryAttempts, "decline policy committed with the terminal result")
	assert.NotNil(t, fx.subscription(t).NextRetryAt)
}

// TestManualRebillRecoveredSubscriptionSupersedes: a parked charge for a
// subscription that recovered (webhook rebill, user fix) is superseded by the
// relevance check instead of firing a stale charge.
func TestManualRebillRecoveredSubscriptionSupersedes(t *testing.T) {
	fx := seedPastDueSubscription(t)
	fake, client := newFakeNMIRebillGateway(t, fx)

	row, err := fx.rebillRunner(client, limitedModeConfig()).EnqueueAndExecute(context.Background(), fx.enqueueParams(1))
	require.NoError(t, err)
	require.Equal(t, StatusPending, row.Status)

	// The subscription recovers while the intent waits for full mode.
	_, err = fx.db.Pool().Exec(context.Background(),
		"UPDATE openrails.subscriptions SET status = 'active', next_retry_at = NULL WHERE id = $1", fx.subID)
	require.NoError(t, err)

	_, err = fx.db.Pool().Exec(context.Background(),
		"UPDATE openrails.rail_intents SET next_attempt_at = now() WHERE id = $1", row.ID)
	require.NoError(t, err)
	_, err = fx.rebillRunner(client, fullModeConfig()).RunExecuteOnce(context.Background())
	require.NoError(t, err)

	got := fx.intentByID(t, row.ID)
	assert.Equal(t, StatusFailedTerminal, got.Status)
	assert.Contains(t, string(got.ResultEvidence), `"not_executed": true`)
	assert.Zero(t, fake.saleCalls.Load(), "a recovered subscription must never be re-charged")
}

// TestManualRebillPaidPeriodSupersedes: a renewal payment can land before the
// subscription advance commits. The durable payment is enough to supersede a
// stale dunning charge even while the subscription still reads past_due.
func TestManualRebillPaidPeriodSupersedes(t *testing.T) {
	fx := seedPastDueSubscription(t)
	fake, client := newFakeNMIRebillGateway(t, fx)

	_, err := fx.db.Pool().Exec(context.Background(), `
		INSERT INTO openrails.payments
			(id, merchant_id, customer_id, price_id, subscription_id, rail, psp_id,
			 transaction_id, amount, list_amount, currency, status, money_movement, purchased_at)
		SELECT $1, merchant_id, customer_id, price_id, id, rail, psp_id,
		       $2, 999, 999, 'USD', 'completed', 'rail', $3
		FROM openrails.subscriptions
		WHERE id = $4`, uuid.New(), "txn-renewal-"+uuid.NewString()[:8], fx.periodEnd, fx.subID)
	require.NoError(t, err)

	row, err := fx.rebillRunner(client, fullModeConfig()).EnqueueAndExecute(context.Background(), fx.enqueueParams(1))
	require.NoError(t, err)
	assert.Equal(t, StatusFailedTerminal, row.Status)
	assert.Contains(t, string(row.ResultEvidence), `"not_executed": true`)
	assert.Zero(t, fake.saleCalls.Load(), "a paid billing period must never be re-charged")
	assert.Zero(t, fake.queryCalls.Load(), "local payment evidence supersedes without a provider call")
}

// TestManualRebillWindowExpiryNeverFires: an intent that outlives the dunning
// window (mode outage longer than the window) expires instead of charging a
// stale period.
func TestManualRebillWindowExpiryNeverFires(t *testing.T) {
	fx := seedPastDueSubscription(t)
	fake, client := newFakeNMIRebillGateway(t, fx)

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
