//go:build integration

package intents

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/integrations/nmi"
)

// fakeNMIRebillGateway scripts the gateway for rebill flows: the Direct Post
// API answers recurring=rebill_subscription sales, the Query API answers the
// order_id transaction search.
type fakeNMIRebillGateway struct {
	saleBody    atomic.Value // string Direct Post response
	saleStatus  atomic.Int64 // optional HTTP status (0 = 200)
	charged     atomic.Bool  // query reports a successful sale for the order id
	saleCalls   atomic.Int64
	queryCalls  atomic.Int64
	saleAuthKey atomic.Value // security_key the last sale authenticated with (#730)
	saleForm    atomic.Value // url.Values: full form of the last sale (#297 wire assertions)
	txnID       string
	// exact is the v5 read of txnID: what NMI recorded for the landed sale.
	exactVault, exactAmount, exactCurrency atomic.Value
}

// recordSale overrides what the v5 exact read reports for the landed sale. By
// default it is what NMI records: the vault the sale was sent with, at the
// plan amount (the fixture's $9.99 USD).
func (f *fakeNMIRebillGateway) recordSale(vault, amount, currency string) {
	f.exactVault.Store(vault)
	f.exactAmount.Store(amount)
	f.exactCurrency.Store(currency)
}

func newFakeNMIRebillGateway(t *testing.T) (*fakeNMIRebillGateway, *nmi.NMIClient) {
	t.Helper()
	f := &fakeNMIRebillGateway{txnID: "txn-rebill-" + uuid.NewString()[:8]}
	f.saleBody.Store("response=1&transactionid=" + f.txnID)

	f.recordSale("", rebillAmountWire, "USD")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/payments/"+f.txnID) && f.charged.Load() {
			amount := f.exactAmount.Load().(string)
			vault := f.exactVault.Load().(string)
			if form, ok := f.saleForm.Load().(url.Values); ok && vault == "" {
				vault = form.Get("customer_vault_id")
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"object": "transaction", "id": f.txnID, "response": "1", "response_code": "100",
				"amount": amount, "currency": f.exactCurrency.Load().(string), "customer_vault_id": vault,
				"actions": []map[string]any{{"id": f.txnID, "type": "sale", "amount": amount, "success": true}},
			})
			return
		}
		if r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/payments/") {
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"type":"notFound","error_code":"E_NOT_FOUND","message":"transaction not found"}`)
			return
		}
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
			f.saleAuthKey.Store(r.Form.Get("security_key"))
			f.saleForm.Store(r.Form)
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
	client.V5BaseURL = srv.URL
	return f, client
}

type rebillFixture struct {
	db        *db.DB
	store     *Store
	subID     uuid.UUID
	periodEnd time.Time
	orderRef  string
	pspID     uuid.UUID
	methodID  uuid.UUID
	vault     string
	billingID string
}

// The fixture price is $9.99 (native micros); the rebill freezes it.
const (
	rebillAmount      = 9_990_000
	rebillAmountMinor = 999
	rebillAmountWire  = "9.99"
)

// seedPastDueSubscription inserts product/price/payment-method/subscription
// in the dunning posture: past_due, missed period end in the recent past.
func seedPastDueSubscription(t *testing.T) rebillFixture {
	t.Helper()
	ctx := context.Background()
	dbi := dbtest.OpenMerchantDB(t, dbtest.TestMerchantID.UUID())
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
	tenantID := dbtest.TestMerchantID.UUID()
	fx.pspID = dbtest.EnsureTestPSP(ctx, t, pool, tenantID, "mobius")
	exec(`INSERT INTO openrails.products (id, key, display_name, merchant_id) VALUES ($1, $2, $2, $3)`,
		productID, "rebill-prod-"+suffix, tenantID)
	exec(`INSERT INTO openrails.prices (id, product_id, amount, currency, access_duration_hours, auto_renew, merchant_id)
	      VALUES ($1, $2, $3, 'USD', 720, true, $4)`, priceID, productID, rebillAmount, tenantID)
	fx.methodID, fx.vault, fx.billingID = paymentMethodID, "vault-"+suffix, "bill-"+suffix
	exec(`INSERT INTO openrails.payment_methods
	        (id, customer_id, rail, psp_id, rail_customer_ref, rail_method_ref,
	         initial_transaction_id, stored_credential_recurring_ref, merchant_id)
	      VALUES ($1, $2, 'mobius', $3, $4, $5, $6, $7, $8)`,
		paymentMethodID, userID, fx.pspID, "vault-"+suffix, "bill-"+suffix,
		"txn-init-"+suffix, "txn-recurring-init-"+suffix, tenantID)
	exec(`INSERT INTO openrails.subscriptions
	        (id, price_id, product_id, status, rail, rail_subscription_id, payment_method_id,
	         current_period_starts_at, current_period_ends_at, started_at, next_retry_at, retry_attempts, customer_id, merchant_id, psp_id)
	      VALUES ($1, $2, $3, 'past_due', 'mobius', $4, $5, $6, $7, $6, $8, 1, $9, $10, $11)`,
		fx.subID, priceID, productID, "psid-"+suffix, paymentMethodID,
		fx.periodEnd.Add(-30*24*time.Hour), fx.periodEnd, now.Add(-30*time.Second), userID, tenantID, fx.pspID)

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
		MerchantID:     dbtest.TestMerchantID.UUID(),
		Provider:       "mobius",
		PspID:          fx.pspID,
		IntentType:     TypeManualRebill,
		SubscriptionID: &subID,
		Payload: ManualRebillPayload{
			SubscriptionID:  fx.subID,
			PeriodEnd:       fx.periodEnd,
			Rail:            "mobius",
			OrderReference:  fx.orderRef,
			Attempt:         attempt,
			PaymentMethodID: fx.methodID,
			Instrument:      RebillInstrument{PSPID: fx.pspID, Custodian: models.CustodianPSP, RailCustomerRef: fx.vault, RailMethodRef: fx.billingID},
			Currency:        "USD",
			Amount:          rebillAmount,
			AmountMinor:     rebillAmountMinor,
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
	fake, client := newFakeNMIRebillGateway(t)
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

	// The charge actually landed at NMI, exactly as frozen.
	fake.charged.Store(true)
	_, err = fx.db.Pool().Exec(context.Background(),
		"UPDATE openrails.rail_intents SET next_attempt_at = now() WHERE id = $1", row.ID)
	require.NoError(t, err)
	_, err = fx.rebillRunner(client, fullModeConfig()).RunVerifyOnce(context.Background())
	require.NoError(t, err)

	got := fx.intentByID(t, row.ID)
	assert.Equal(t, StatusSucceeded, got.Status)
	// #607: the tombstone is slimmed to the dunning pointer keys — the
	// transaction_id the repair path reads survives; the verified_existing
	// forensic marker is dropped (retained in the mutation log).
	assert.Contains(t, string(got.ResultEvidence), fake.txnID, "transaction_id pointer retained for the dunning repair path")
	assert.NotContains(t, string(got.ResultEvidence), "verified_existing", "forensic evidence pruned")
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
		"UPDATE openrails.rail_intents SET next_attempt_at = now() WHERE id = $1", row.ID)
	require.NoError(t, err)
	_, err = fx.rebillRunner(client, fullModeConfig()).RunExecuteOnce(context.Background())
	require.NoError(t, err)

	got := fx.intentByID(t, row.ID)
	assert.Equal(t, StatusSuperseded, got.Status)
	assert.Zero(t, fake.saleCalls.Load(), "a recovered subscription must never be re-charged")
}

// TestManualRebillPaidPeriodSupersedes: a renewal payment can land before the
// subscription advance commits. The durable payment is enough to supersede a
// stale dunning charge even while the subscription still reads past_due.
func TestManualRebillPaidPeriodSupersedes(t *testing.T) {
	fx := seedPastDueSubscription(t)
	fake, client := newFakeNMIRebillGateway(t)

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
	assert.Equal(t, StatusSuperseded, row.Status)
	assert.Zero(t, fake.saleCalls.Load(), "a paid billing period must never be re-charged")
	assert.Zero(t, fake.queryCalls.Load(), "local payment evidence supersedes without a provider call")
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

// TestManualRebillContradictedReceiptStaysUnknown (#809 R4 P1): a rebill whose
// response was lost converges only on the exact frozen charge. A sale under
// the period's order reference on another vault, for another amount, or in
// another currency keeps the operation unknown with the contradiction
// retained: no payment, no renewal, no resend; the operator can neither name
// it as the receipt nor attest non-execution. The exact sale then settles —
// through the verifier, or through the operator's receipt.
func TestManualRebillContradictedReceiptStaysUnknown(t *testing.T) {
	for _, tc := range []struct {
		name, vault, amount, currency string
		byOperator                    bool
	}{
		{name: "another vault", vault: "someone-elses-vault", amount: rebillAmountWire, currency: "USD"},
		{name: "another amount", amount: "0.01", currency: "USD"},
		{name: "another currency", amount: rebillAmountWire, currency: "EUR", byOperator: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := seedPastDueSubscription(t)
			fake, client := newFakeNMIRebillGateway(t)
			fake.saleStatus.Store(http.StatusBadGateway)
			runner := fx.rebillRunner(client, fullModeConfig())
			ctx := dbtest.WithTestMerchant(context.Background())
			row, err := runner.EnqueueAndExecute(context.Background(), fx.enqueueParams(1))
			require.NoError(t, err)
			require.Equal(t, StatusUnknownNeedsVerify, row.Status)

			fake.charged.Store(true)
			fake.recordSale(tc.vault, tc.amount, tc.currency)
			_, err = fx.db.Pool().Exec(ctx, "UPDATE openrails.rail_intents SET next_attempt_at = now() WHERE id = $1", row.ID)
			require.NoError(t, err)
			_, err = runner.RunVerifyOnce(context.Background())
			require.NoError(t, err)
			got := fx.intentByID(t, row.ID)
			require.Equal(t, StatusUnknownNeedsVerify, got.Status, "a contradicting sale never settles")
			require.Contains(t, string(got.ResultEvidence), rebillEvidenceContradiction)
			require.Equal(t, "past_due", string(fx.subscription(t).Status), "no renewal")
			require.Zero(t, fx.paymentsFor(t, fake.txnID), "no payment recorded")

			_, err = runner.Resolve(ctx, row.ID, Resolution{ProviderReference: fake.txnID, Actor: "ops", Reason: "portal"})
			require.ErrorIs(t, err, ErrResolutionRejected, "the contradicting sale is not this rebill's receipt")
			_, err = runner.Resolve(ctx, row.ID, Resolution{NotExecuted: true, Actor: "ops", Reason: "portal"})
			require.ErrorIs(t, err, ErrResolutionRejected, "non-execution cannot be attested against provider evidence")
			require.Equal(t, StatusUnknownNeedsVerify, fx.intentByID(t, row.ID).Status)
			require.EqualValues(t, 1, fake.saleCalls.Load(), "nothing is resent")

			fake.recordSale("", rebillAmountWire, "USD")
			if tc.byOperator {
				resolved, err := runner.Resolve(ctx, row.ID, Resolution{ProviderReference: fake.txnID, Actor: "ops", Reason: "portal"})
				require.NoError(t, err)
				require.Equal(t, StatusSucceeded, resolved.Status)
			} else {
				_, err = fx.db.Pool().Exec(ctx, "UPDATE openrails.rail_intents SET next_attempt_at = now() WHERE id = $1", row.ID)
				require.NoError(t, err)
				_, err = runner.RunVerifyOnce(context.Background())
				require.NoError(t, err)
				require.Equal(t, StatusSucceeded, fx.intentByID(t, row.ID).Status)
			}
			require.Equal(t, "active", string(fx.subscription(t).Status))
			require.Equal(t, 1, fx.paymentsFor(t, fake.txnID))
			var amount int64
			require.NoError(t, fx.db.Pool().QueryRow(ctx, "SELECT amount FROM openrails.payments WHERE subscription_id = $1 AND transaction_id = $2", fx.subID, fake.txnID).Scan(&amount))
			require.EqualValues(t, rebillAmount, amount, "the renewal records the frozen amount")
			require.EqualValues(t, 1, fake.saleCalls.Load())
		})
	}
}
