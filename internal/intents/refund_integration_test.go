//go:build integration

package intents

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/open-rails/openrails/internal/railresolve"
	"net/http"
	"net/http/httptest"
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
	"github.com/open-rails/openrails/internal/integrations/ccbill"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
)

// fakeNMIRefundGateway scripts the v5 gateway for refund flows:
// POST /payments/{id}/refund answers the refund, GET /payments/{id} answers
// the verifier's transaction read (refund action present or not).
type fakeNMIRefundGateway struct {
	refundBody   atomic.Value // string JSON refund response
	refundStatus atomic.Int64 // optional HTTP status (0 = 200)
	refunded     atomic.Bool  // the transaction read reports the refund action
	refundCalls  atomic.Int64
	queryCalls   atomic.Int64
	psid         string // original transaction id, echoed by the read
}

func newFakeNMIRefundGateway(t *testing.T, originalTxn string) (*fakeNMIRefundGateway, *nmi.NMIClient) {
	t.Helper()
	f := &fakeNMIRefundGateway{psid: originalTxn}
	f.refundBody.Store(`{"object":"transaction","id":"txn_refund_1","response":"1","response_text":"SUCCESS"}`)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet:
			f.queryCalls.Add(1)
			if f.refunded.Load() {
				fmt.Fprintf(w, `{"object":"transaction","id":"%s","actions":[
					{"id":"%s","type":"sale","success":true,"amount":"10.00"},
					{"id":"txn_refund_1","type":"refund","success":true,"amount":"5.00"}
				]}`, f.psid, f.psid)
			} else {
				fmt.Fprintf(w, `{"object":"transaction","id":"%s","actions":[{"id":"%s","type":"sale","success":true,"amount":"10.00"}]}`, f.psid, f.psid)
			}
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/refund"):
			f.refundCalls.Add(1)
			if st := f.refundStatus.Load(); st != 0 {
				w.WriteHeader(int(st))
				fmt.Fprint(w, `{"type":"internalError","error_code":"E_INTERNAL","message":"gateway error"}`)
				return
			}
			_, _ = w.Write([]byte(f.refundBody.Load().(string)))
		default:
			_, _ = w.Write([]byte(`{}`))
		}
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

type refundFixture struct {
	db            *db.DB
	store         *Store
	paymentID     uuid.UUID
	reservationID uuid.UUID
	originalTxn   string
	pspID         uuid.UUID
}

// seedRefundablePayment inserts product/price/payment (completed nmi
// charge) plus the open admin refund reservation the intent finalizes.
func seedRefundablePayment(t *testing.T, amountCents int64) refundFixture {
	t.Helper()
	ctx := dbtest.WithTestMerchant(context.Background())
	dbi := dbtest.OpenMerchantDB(t, dbtest.TestMerchantID.UUID())
	pool := dbi.Pool()

	fx := refundFixture{db: dbi, store: NewStore(dbi)}
	fx.paymentID = uuid.New()
	fx.originalTxn = "txn-" + uuid.NewString()[:8]
	userID := dbtest.EnsureCustomerIDPgx(ctx, t, pool, uuid.NewString())

	productID := uuid.New()
	priceID := uuid.New()
	suffix := uuid.NewString()[:8]

	exec := func(sql string, args ...any) {
		t.Helper()
		_, err := pool.Exec(ctx, sql, args...)
		require.NoError(t, err)
	}
	tenantID := dbtest.TestMerchantID.UUID()
	fx.pspID = dbtest.EnsureTestPSP(ctx, t, pool, tenantID, "nmi")
	// ReserveRefund's payment Create falls back to the ambient-context PSP
	// (db.RequirePSPID) when the reservation row's own PspID field is unset.
	ctx = db.WithPSPID(ctx, fx.pspID)
	exec(`INSERT INTO openrails.products (id, key, display_name, merchant_id) VALUES ($1, $2, $2, $3)`,
		productID, "refund-prod-"+suffix, tenantID)
	exec(`INSERT INTO openrails.prices (id, product_id, amount, currency, merchant_id) VALUES ($1, $2, 1000, 'USD', $3)`,
		priceID, productID, tenantID)
	exec(`INSERT INTO openrails.payments (id, price_id, rail, psp_id, transaction_id, amount, list_amount, currency, status, customer_id, merchant_id, money_movement)
	      VALUES ($1, $2, 'nmi', $3, $4, 1000, 1000, 'USD', 'completed', $5, $6, 'rail')`,
		fx.paymentID, priceID, fx.pspID, fx.originalTxn, userID, tenantID)

	reservation, err := payments.NewPaymentService(dbi).ReserveRefund(ctx, fx.paymentID,
		"admin_refund_reservation:"+fx.paymentID.String(), amountCents,
		map[string]any{"admin_refund_idempotency_key": "it-key", "admin_refund_status": "pending"})
	require.NoError(t, err)
	fx.reservationID = reservation.ID

	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.rail_intents WHERE payment_id = $1", fx.paymentID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.payments WHERE id = $1 OR refunded_payment_id = $1", fx.paymentID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.prices WHERE id = $1", priceID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.products WHERE id = $1", productID)
	})
	return fx
}

func (fx refundFixture) payload(amountCents int64) RefundPayload {
	return RefundPayload{
		OriginalPaymentID: fx.paymentID,
		ReservationID:     fx.reservationID,
		AmountCents:       moneyutil.Cents(amountCents),
		ProviderTarget:    fx.originalTxn,
	}
}

func (fx refundFixture) enqueueParams(amountCents int64) EnqueueParams {
	paymentID := fx.paymentID
	return EnqueueParams{
		MerchantID:     dbtest.TestMerchantID.UUID(),
		Provider:       "nmi",
		PspID:          fx.pspID,
		IntentType:     TypeNMIRefund,
		PaymentID:      &paymentID,
		Payload:        fx.payload(amountCents),
		IdempotencyKey: RefundIdempotencyKey(fx.paymentID, "it-key"),
		NextAttemptAt:  time.Now().UTC(),
		Origin:         OriginAdmin,
		OriginReason:   "integration test",
	}
}

func (fx refundFixture) refundRunner(client *nmi.NMIClient, cfg *config.Config) *Runner {
	return &Runner{
		Store:    fx.store,
		Registry: NewRegistry(NewNMIRefundHandler(fx.db, fakeNMIResolver{client: client}, nil)),
		Config:   cfg,
	}
}

func (fx refundFixture) reservation(t *testing.T) (status, transactionID string, metadata map[string]any) {
	t.Helper()
	row, err := payments.NewPaymentService(fx.db).GetByID(context.Background(), fx.reservationID)
	require.NoError(t, err)
	return row.Status, row.TransactionID, row.Metadata
}

func (fx refundFixture) intentByID(t *testing.T, id uuid.UUID) gen.OpenrailsRailIntent {
	t.Helper()
	row, err := fx.db.Gen(context.Background()).GetRailIntent(context.Background(), id)
	require.NoError(t, err)
	return row
}

// TestNMIRefundSynchronousSuccess: the producer's enqueue+execute completes
// the refund inline — intent succeeded, reservation completed with the
// provider refund id.
func TestNMIRefundSynchronousSuccess(t *testing.T) {
	fx := seedRefundablePayment(t, 500)
	fake, client := newFakeNMIRefundGateway(t, fx.originalTxn)

	row, err := fx.refundRunner(client, fullModeConfig()).EnqueueAndExecute(context.Background(), fx.enqueueParams(500))
	require.NoError(t, err)
	assert.Equal(t, StatusSucceeded, row.Status)
	// #607: the refund tombstone retains its PAYLOAD (the admin producer reads
	// reservation_id off it for double-refund conflict detection); the forensic
	// evidence is slimmed. The completed reservation (below) is the source of
	// truth for the provider refund id.
	assert.NotEmpty(t, row.Payload, "refund payload retained on the succeeded tombstone")
	assert.NotContains(t, string(row.ResultEvidence), "txn_refund_1", "forensic evidence slimmed off the tombstone")
	assert.EqualValues(t, 1, fake.refundCalls.Load())

	status, txn, metadata := fx.reservation(t)
	assert.Equal(t, "completed", status)
	assert.Equal(t, "txn_refund_1", txn)
	assert.Equal(t, "completed", metadata["admin_refund_status"])
	assert.Equal(t, "txn_refund_1", metadata["provider_refund_id"])
	assert.Equal(t, "it-key", metadata["admin_refund_idempotency_key"], "reservation metadata is carried over, not replaced")
}

// TestNMIRefundParksUnderReadonlyAndDrainsUnderFull pins the phase B park
// contract: readonly parks the admin refund (pending, reason recorded, no
// provider traffic, reservation open); when the mode lifts the SCHEDULED
// executor drains it and the handler's finalize completes the reservation —
// no producer involved.
func TestNMIRefundParksUnderReadonlyAndDrainsUnderFull(t *testing.T) {
	fx := seedRefundablePayment(t, 500)
	fake, client := newFakeNMIRefundGateway(t, fx.originalTxn)

	row, err := fx.refundRunner(client, readonlyModeConfig()).EnqueueAndExecute(context.Background(), fx.enqueueParams(500))
	require.NoError(t, err)
	assert.Equal(t, StatusPending, row.Status, "parked is a state, not an error")
	require.NotNil(t, row.LastFailureReason)
	assert.Contains(t, *row.LastFailureReason, "mode=readonly")
	assert.EqualValues(t, 0, row.Attempts, "a park does not burn an attempt")
	assert.Zero(t, fake.refundCalls.Load())
	status, _, _ := fx.reservation(t)
	assert.Equal(t, "pending", status, "reservation stays open while the intent waits")

	// Mode lifts; the scheduled executor pass drains the queue.
	_, err = fx.db.Pool().Exec(context.Background(),
		"UPDATE openrails.rail_intents SET next_attempt_at = now() WHERE id = $1", row.ID)
	require.NoError(t, err)
	_, err = fx.refundRunner(client, fullModeConfig()).RunExecuteOnce(context.Background())
	require.NoError(t, err)

	assert.Equal(t, StatusSucceeded, fx.intentByID(t, row.ID).Status)
	status, txn, _ := fx.reservation(t)
	assert.Equal(t, "completed", status, "async drain completes the reservation via the handler's finalize")
	assert.Equal(t, "txn_refund_1", txn)
}

// An equal-size refund action or a currently empty read cannot attribute a lost
// response to this operation. Keep the reservation and never resend blindly.
func TestNMIRefundLostResponseNeedsExactReceipt(t *testing.T) {
	for _, priorRefund := range []bool{false, true} {
		t.Run(fmt.Sprint(priorRefund), func(t *testing.T) {
			fx := seedRefundablePayment(t, 500)
			fake, client := newFakeNMIRefundGateway(t, fx.originalTxn)
			fake.refundStatus.Store(http.StatusBadGateway)
			runner := fx.refundRunner(client, fullModeConfig())
			row, err := runner.EnqueueAndExecute(context.Background(), fx.enqueueParams(500))
			require.NoError(t, err)
			require.Equal(t, StatusUnknownNeedsVerify, row.Status)
			fake.refunded.Store(priorRefund)
			_, err = fx.db.Pool().Exec(context.Background(), "UPDATE openrails.rail_intents SET next_attempt_at=now() WHERE id=$1", row.ID)
			require.NoError(t, err)
			_, err = runner.RunVerifyOnce(context.Background())
			require.NoError(t, err)
			got := fx.intentByID(t, row.ID)
			assert.Equal(t, StatusUnknownNeedsVerify, got.Status)
			require.NotNil(t, got.LastFailureReason)
			assert.Contains(t, *got.LastFailureReason, "operator must verify")
			status, _, _ := fx.reservation(t)
			assert.Equal(t, "pending", status)
			assert.EqualValues(t, 1, fake.refundCalls.Load())
		})
	}
}

// fakeDataLinkDenyRefund serves an action-aware DataLink SMS: the refund action
// always returns the OVERLOADED denial code -7 (as CCBill did in the 2026-07-03
// safe fail-probe against a too-old transaction), while viewSubscriptionStatus
// returns known-zero counters so the pre-send verify lets each attempt proceed.
// newCCBillRefundIntegrationHandler arms the handler from the armed rail
// state (#788), pointing the built DataLink client at the fake server.
func newCCBillRefundIntegrationHandler(db *db.DB, client *ccbill.DataLinkClient) *CCBillRefundHandler {
	h := NewCCBillRefundHandler(db, nil, ccbillRefundTestRails(), nil)
	h.DataLinkBaseURL = client.BaseURL
	return h
}

func fakeDataLinkDenyRefund(t *testing.T) (*ccbill.DataLinkClient, *atomic.Int64) {
	t.Helper()
	var refundHits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.PostFormValue("action") == "voidOrRefundTransaction" {
			refundHits.Add(1)
			_, _ = w.Write([]byte(`<results>-7</results>`))
			return
		}
		// viewSubscriptionStatus: alive sub, no refunds/voids recorded.
		_, _ = w.Write([]byte(`<results><subscriptionStatus>2</subscriptionStatus><refundsIssued>0</refundsIssued><voidsIssued>0</voidsIssued></results>`))
	}))
	t.Cleanup(srv.Close)
	client := &ccbill.DataLinkClient{
		BaseURL: srv.URL, ClientAccNum: "900100", ClientSubAcc: "0000",
		Username: "u", Password: "p", HTTPClient: &http.Client{Timeout: 5 * time.Second},
	}
	return client, &refundHits
}

// TestCCBillRefundDenialBoundedRetryThenTerminal: -7 is CCBill's OVERLOADED
// denial (auth OR not-permitted/too-old). The refund did NOT execute, so it is
// bounded-retried (a few clean attempts cover a transient auth/IP flap) and then
// declared TERMINAL — never infinite Retryable behind a misleading "auth" reason.
// Terminal releases the reservation and the reason is operator-facing.
func TestCCBillRefundDenialBoundedRetryThenTerminal(t *testing.T) {
	fx := seedRefundablePayment(t, 500)
	client, refundHits := fakeDataLinkDenyRefund(t)
	ctx := context.Background()

	runner := &Runner{
		Store:    fx.store,
		Registry: NewRegistry(newCCBillRefundIntegrationHandler(fx.db, client)),
		Config:   fullModeConfig(),
	}
	paymentID := fx.paymentID
	ccbillPspID := dbtest.EnsureTestPSP(ctx, t, fx.db.Pool(), dbtest.TestMerchantID.UUID(), "ccbill")
	params := EnqueueParams{
		MerchantID: dbtest.TestMerchantID.UUID(),
		Provider:   "ccbill",
		PspID:      ccbillPspID,
		IntentType: TypeCCBillRefund,
		PaymentID:  &paymentID,
		Payload: RefundPayload{
			OriginalPaymentID:     fx.paymentID,
			ReservationID:         fx.reservationID,
			AmountCents:           moneyutil.Cents(500),
			ProviderTarget:        "sub_x", // CCBill subscriptionId (arbitrary here)
			ProviderTransactionID: "tx_x",
		},
		IdempotencyKey: RefundIdempotencyKey(fx.paymentID, "it-key"),
		NextAttemptAt:  time.Now().UTC(),
		Origin:         OriginAdmin,
		OriginReason:   "integration test",
	}

	// First (synchronous) attempt: -7 -> bounded retry, NOT terminal, NOT the old
	// "auth rejected" reason.
	row, err := runner.EnqueueAndExecute(ctx, params)
	require.NoError(t, err)
	require.Equal(t, StatusFailedRetryable, row.Status, "-7 first attempt is a bounded retry, not infinite/terminal")
	require.NotNil(t, row.LastFailureReason)
	assert.Contains(t, *row.LastFailureReason, "denied (-7)")
	assert.NotContains(t, *row.LastFailureReason, "auth rejected")
	status, _, _ := fx.reservation(t)
	assert.Equal(t, "pending", status, "reservation stays open while the intent retries")

	// Drive the scheduled executor until the bounded budget is spent -> terminal.
	for i := 0; i < ccbillDenialMaxAttempts+2; i++ {
		if fx.intentByID(t, row.ID).Status == StatusFailedTerminal {
			break
		}
		_, err = fx.db.Pool().Exec(ctx,
			"UPDATE openrails.rail_intents SET next_attempt_at = now() WHERE id = $1", row.ID)
		require.NoError(t, err)
		_, err = runner.RunExecuteOnce(ctx)
		require.NoError(t, err)
	}

	final := fx.intentByID(t, row.ID)
	assert.Equal(t, StatusFailedTerminal, final.Status, "a permanently-denied refund goes terminal, not forever-retryable")
	require.NotNil(t, final.LastFailureReason)
	assert.Contains(t, *final.LastFailureReason, "operator")
	assert.EqualValues(t, ccbillDenialMaxAttempts, refundHits.Load(), "exactly the bounded number of refund sends, then stop")
	status, _, _ = fx.reservation(t)
	assert.Equal(t, "failed", status, "terminal denial releases the reservation")
}

// TestNMIRefundDeclineReleasesReservation: a clean gateway decline is
// terminal — the reservation is released (failed) so the admin can see the
// refusal and retry deliberately.
func TestNMIRefundDeclineReleasesReservation(t *testing.T) {
	fx := seedRefundablePayment(t, 500)
	fake, client := newFakeNMIRefundGateway(t, fx.originalTxn)
	fake.refundBody.Store(`{"object":"transaction","id":"txn_refund_1","response":"2","response_text":"DECLINED","response_code":"300"}`)

	row, err := fx.refundRunner(client, fullModeConfig()).EnqueueAndExecute(context.Background(), fx.enqueueParams(500))
	require.NoError(t, err)
	assert.Equal(t, StatusFailedTerminal, row.Status)
	require.NotNil(t, row.LastFailureReason)
	assert.Contains(t, *row.LastFailureReason, "declined")

	status, _, _ := fx.reservation(t)
	assert.Equal(t, "failed", status, "terminal refusal releases the reservation")
}

// TestNMIRefundConflictReturnsDurableOutcome: re-enqueueing the same logical
// refund (same payment + amount) returns the executed intent untouched — the
// producer detects the foreign reservation and refuses to double-refund.
func TestNMIRefundConflictReturnsDurableOutcome(t *testing.T) {
	fx := seedRefundablePayment(t, 500)
	fake, client := newFakeNMIRefundGateway(t, fx.originalTxn)

	first, err := fx.refundRunner(client, fullModeConfig()).EnqueueAndExecute(context.Background(), fx.enqueueParams(500))
	require.NoError(t, err)
	require.Equal(t, StatusSucceeded, first.Status)

	// A second reservation for the identical refund content conflicts on the
	// content-addressed key and must NOT execute again.
	params := fx.enqueueParams(500)
	otherReservation := fx.payload(500)
	otherReservation.ReservationID = uuid.New()
	params.Payload = otherReservation

	second, err := fx.refundRunner(client, fullModeConfig()).EnqueueAndExecute(context.Background(), params)
	require.NoError(t, err)
	assert.Equal(t, first.ID, second.ID, "one intent per logical refund")
	assert.Equal(t, StatusSucceeded, second.Status)
	assert.EqualValues(t, 1, fake.refundCalls.Load(), "no second money movement")

	var payload RefundPayload
	require.NoError(t, json.Unmarshal(second.Payload, &payload))
	assert.Equal(t, fx.reservationID, payload.ReservationID,
		"the durable payload still references the ORIGINAL reservation — producers detect the conflict by this")
}

// ============================================================================
// Stripe
// ============================================================================

type fakeStripeServer struct {
	srv          *httptest.Server
	createStatus atomic.Int64 // 0 = 200
	creates      atomic.Int64
	lists        atomic.Int64
	created      atomic.Bool
	gotIdemKey   atomic.Value // string
	gotMetadata  atomic.Value // string
}

func newFakeStripeServer(t *testing.T) *fakeStripeServer {
	t.Helper()
	f := &fakeStripeServer{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/refunds":
			f.creates.Add(1)
			_ = r.ParseForm()
			f.gotIdemKey.Store(r.Header.Get("Idempotency-Key"))
			f.gotMetadata.Store(r.Form.Get("metadata[openrails_idempotency_key]"))
			if st := f.createStatus.Load(); st != 0 {
				w.WriteHeader(int(st))
				fmt.Fprint(w, `{"error": {"message": "boom"}}`)
				return
			}
			f.created.Store(true)
			fmt.Fprintf(w, `{"id": "re_1", "amount": 500, "status": "succeeded", "charge": "ch_1", "metadata": {"openrails_idempotency_key": %q}}`, f.gotMetadata.Load())
		case r.Method == http.MethodGet && r.URL.Path == "/v1/refunds":
			f.lists.Add(1)
			if f.created.Load() {
				fmt.Fprintf(w, `{"data": [{"id": "re_1", "amount": 500, "status": "succeeded", "charge": "ch_1", "metadata": {"openrails_idempotency_key": %q}}]}`, f.gotMetadata.Load())
			} else {
				fmt.Fprint(w, `{"data": []}`)
			}
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func stripeIntegrationConfig(mode string) *config.Config {
	return &config.Config{
		ProviderWriteMode: mode,
	}
}

func stripeIntegrationRails() railresolve.FixedSet {
	return railresolve.FixedSet{
		"stripe": {Rail: models.RailStripe, Stripe: &config.StripeRailConfig{SecretKey: "sk_test_123"}},
	}
}

func (fx refundFixture) stripeRunner(cfg *config.Config, baseURL string) *Runner {
	rails := stripeIntegrationRails()
	handler := NewStripeRefundHandler(fx.db, cfg, rails, nil)
	handler.Stripe = &subscriptions.StripeRefundService{Config: cfg, Rails: rails, BaseURL: baseURL}
	return &Runner{Store: fx.store, Registry: NewRegistry(handler), Config: cfg}
}

func (fx refundFixture) stripeEnqueueParams(t *testing.T, amountCents int64) EnqueueParams {
	params := fx.enqueueParams(amountCents)
	params.Provider = "stripe"
	params.PspID = dbtest.EnsureTestPSP(context.Background(), t, fx.db.Pool(), dbtest.TestMerchantID.UUID(), "stripe")
	params.IntentType = TypeStripeRefund
	payload := fx.payload(amountCents)
	payload.ProviderTarget = "ch_1"
	params.Payload = payload
	params.IdempotencyKey = RefundIdempotencyKey(fx.paymentID, "it-key")
	return params
}

// TestStripeRefundSynchronousSuccessCarriesIdempotencyKey: the create carries
// the intent's idempotency key as BOTH the Stripe Idempotency-Key header and
// the metadata mirror; success completes the reservation.
func TestStripeRefundSynchronousSuccessCarriesIdempotencyKey(t *testing.T) {
	fx := seedRefundablePayment(t, 500)
	stripe := newFakeStripeServer(t)
	cfg := stripeIntegrationConfig(config.ProviderWriteModeFull)

	row, err := fx.stripeRunner(cfg, stripe.srv.URL).EnqueueAndExecute(context.Background(), fx.stripeEnqueueParams(t, 500))
	require.NoError(t, err)
	assert.Equal(t, StatusSucceeded, row.Status)

	wantKey := RefundIdempotencyKey(fx.paymentID, "it-key")
	assert.Equal(t, wantKey, stripe.gotIdemKey.Load(), "Stripe Idempotency-Key = the intent idempotency_key")
	assert.Equal(t, wantKey, stripe.gotMetadata.Load(), "metadata mirror makes the refund re-findable by reads")

	status, txn, _ := fx.reservation(t)
	assert.Equal(t, "completed", status)
	assert.Equal(t, "re_1", txn)
}

// TestStripeRefundAmbiguousResolvedByVerifier: a 5xx from Stripe is
// ambiguous; the verifier re-finds the refund via GET /v1/refunds (metadata
// match) and completes the reservation without another POST.
func TestStripeRefundAmbiguousResolvedByVerifier(t *testing.T) {
	fx := seedRefundablePayment(t, 500)
	stripe := newFakeStripeServer(t)
	stripe.createStatus.Store(http.StatusInternalServerError)
	cfg := stripeIntegrationConfig(config.ProviderWriteModeFull)

	row, err := fx.stripeRunner(cfg, stripe.srv.URL).EnqueueAndExecute(context.Background(), fx.stripeEnqueueParams(t, 500))
	require.NoError(t, err)
	require.Equal(t, StatusUnknownNeedsVerify, row.Status)

	// The refund DID get created server-side despite the 500.
	stripe.created.Store(true)
	stripe.gotMetadata.Store(RefundIdempotencyKey(fx.paymentID, "it-key"))
	_, err = fx.db.Pool().Exec(context.Background(),
		"UPDATE openrails.rail_intents SET next_attempt_at = now() WHERE id = $1", row.ID)
	require.NoError(t, err)
	_, err = fx.stripeRunner(cfg, stripe.srv.URL).RunVerifyOnce(context.Background())
	require.NoError(t, err)

	assert.Equal(t, StatusSucceeded, fx.intentByID(t, row.ID).Status)
	assert.EqualValues(t, 1, stripe.creates.Load(), "verification resolves via reads, no second POST")
	assert.GreaterOrEqual(t, stripe.lists.Load(), int64(1))
	status, txn, _ := fx.reservation(t)
	assert.Equal(t, "completed", status)
	assert.Equal(t, "re_1", txn)
}

// TestStripeRefundRefusalReleasesReservation: a clean 4xx refusal is terminal
// and releases the reservation.
func TestStripeRefundRefusalReleasesReservation(t *testing.T) {
	fx := seedRefundablePayment(t, 500)
	stripe := newFakeStripeServer(t)
	stripe.createStatus.Store(http.StatusBadRequest)
	cfg := stripeIntegrationConfig(config.ProviderWriteModeFull)

	row, err := fx.stripeRunner(cfg, stripe.srv.URL).EnqueueAndExecute(context.Background(), fx.stripeEnqueueParams(t, 500))
	require.NoError(t, err)
	assert.Equal(t, StatusFailedTerminal, row.Status)
	status, _, _ := fx.reservation(t)
	assert.Equal(t, "failed", status)
}
