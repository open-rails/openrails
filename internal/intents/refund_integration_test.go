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
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
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
}

// seedRefundablePayment inserts product/price/payment (completed nmi
// charge) plus the open admin refund reservation the intent finalizes.
func seedRefundablePayment(t *testing.T, amountCents int64) refundFixture {
	t.Helper()
	ctx := dbtest.WithTestMerchant(context.Background())
	dsn := dbtest.SharedPostgresDSN(t)
	dbi := dbtest.OpenAppDB(t, dsn)
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
	exec(`INSERT INTO openrails.products (id, key, display_name, merchant_id) VALUES ($1, $2, $2, $3)`,
		productID, "refund-prod-"+suffix, tenantID)
	exec(`INSERT INTO openrails.prices (id, product_id, amount, currency, merchant_id) VALUES ($1, $2, 1000, 'usd', $3)`,
		priceID, productID, tenantID)
	exec(`INSERT INTO openrails.payments (id, price_id, rail, transaction_id, amount, list_amount, currency, status, customer_id, merchant_id)
	      VALUES ($1, $2, 'nmi', $3, 1000, 1000, 'usd', 'completed', $4, $5)`,
		fx.paymentID, priceID, fx.originalTxn, userID, tenantID)

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
		IntentType:     TypeNMIRefund,
		PaymentID:      &paymentID,
		Payload:        fx.payload(amountCents),
		IdempotencyKey: NMIRefundIdempotencyKey(fx.originalTxn, amountCents),
		NextAttemptAt:  time.Now().UTC(),
		Origin:         OriginAdmin,
		OriginReason:   "integration test",
	}
}

func (fx refundFixture) refundRunner(client *nmi.NMIClient, cfg *config.Config) *Runner {
	return &Runner{
		Store:    fx.store,
		Registry: NewRegistry(NewNMIRefundHandler(fx.db, map[string]*nmi.NMIClient{"nmi": client}, nil)),
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

// TestNMIRefundAmbiguousResolvedByVerifier: a transport failure mid-refund
// parks as unknown_needs_verify (never blind-retried); the verifier reads the
// transaction report and finalizes off what actually happened.
func TestNMIRefundAmbiguousResolvedByVerifier(t *testing.T) {
	t.Run("refund landed -> succeeded", func(t *testing.T) {
		fx := seedRefundablePayment(t, 500)
		fake, client := newFakeNMIRefundGateway(t, fx.originalTxn)
		fake.refundStatus.Store(http.StatusBadGateway)

		row, err := fx.refundRunner(client, fullModeConfig()).EnqueueAndExecute(context.Background(), fx.enqueueParams(500))
		require.NoError(t, err)
		require.Equal(t, StatusUnknownNeedsVerify, row.Status, "a possibly-sent refund must verify, not retry")
		status, _, _ := fx.reservation(t)
		assert.Equal(t, "pending", status)

		fake.refunded.Store(true) // the refund actually landed
		_, err = fx.db.Pool().Exec(context.Background(),
			"UPDATE openrails.rail_intents SET next_attempt_at = now() WHERE id = $1", row.ID)
		require.NoError(t, err)
		_, err = fx.refundRunner(client, fullModeConfig()).RunVerifyOnce(context.Background())
		require.NoError(t, err)

		got := fx.intentByID(t, row.ID)
		assert.Equal(t, StatusSucceeded, got.Status)
		assert.NotEmpty(t, got.Payload, "#607: refund payload retained on the succeeded tombstone")
		status, txn, _ := fx.reservation(t)
		assert.Equal(t, "completed", status)
		assert.Equal(t, "txn_refund_1", txn)
		assert.EqualValues(t, 1, fake.refundCalls.Load(), "verification never re-sends the refund")
	})

	t.Run("refund verified absent -> retried with verify-first guard", func(t *testing.T) {
		fx := seedRefundablePayment(t, 500)
		fake, client := newFakeNMIRefundGateway(t, fx.originalTxn)
		fake.refundStatus.Store(http.StatusBadGateway)

		row, err := fx.refundRunner(client, fullModeConfig()).EnqueueAndExecute(context.Background(), fx.enqueueParams(500))
		require.NoError(t, err)
		require.Equal(t, StatusUnknownNeedsVerify, row.Status)

		// Verifier: no refund at the provider -> verified not executed.
		_, err = fx.db.Pool().Exec(context.Background(),
			"UPDATE openrails.rail_intents SET next_attempt_at = now() WHERE id = $1", row.ID)
		require.NoError(t, err)
		_, err = fx.refundRunner(client, fullModeConfig()).RunVerifyOnce(context.Background())
		require.NoError(t, err)
		require.Equal(t, StatusFailedRetryable, fx.intentByID(t, row.ID).Status)

		// Gateway recovers; the retry re-verifies before sending (attempts>1).
		fake.refundStatus.Store(0)
		queryCallsBefore := fake.queryCalls.Load()
		_, err = fx.db.Pool().Exec(context.Background(),
			"UPDATE openrails.rail_intents SET next_attempt_at = now() WHERE id = $1", row.ID)
		require.NoError(t, err)
		_, err = fx.refundRunner(client, fullModeConfig()).RunExecuteOnce(context.Background())
		require.NoError(t, err)

		assert.Equal(t, StatusSucceeded, fx.intentByID(t, row.ID).Status)
		assert.Greater(t, fake.queryCalls.Load(), queryCallsBefore, "re-execution of a money mover verifies by reading first")
		// Two wire calls total: the original 502'd send and the post-verify
		// retry — but only ONE refund was ever processed by the gateway.
		assert.EqualValues(t, 2, fake.refundCalls.Load())
		status, _, _ := fx.reservation(t)
		assert.Equal(t, "completed", status)
	})
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

func stripeIntegrationRails() config.RailMerchantAccountSet {
	return config.RailMerchantAccountSet{
		"stripe": {Rail: models.RailStripe, Stripe: &config.StripeRailConfig{SecretKey: "sk_test_123"}},
	}
}

func (fx refundFixture) stripeRunner(cfg *config.Config, baseURL string) *Runner {
	rails := stripeIntegrationRails()
	handler := NewStripeRefundHandler(fx.db, cfg, rails, nil)
	handler.Stripe = &subscriptions.StripeRefundService{Config: cfg, Rails: rails, BaseURL: baseURL}
	return &Runner{Store: fx.store, Registry: NewRegistry(handler), Config: cfg}
}

func (fx refundFixture) stripeEnqueueParams(amountCents int64) EnqueueParams {
	params := fx.enqueueParams(amountCents)
	params.Provider = "stripe"
	params.IntentType = TypeStripeRefund
	payload := fx.payload(amountCents)
	payload.ProviderTarget = "ch_1"
	params.Payload = payload
	params.IdempotencyKey = StripeRefundIntentIdempotencyKey("ch_1", amountCents, "")
	return params
}

// TestStripeRefundSynchronousSuccessCarriesIdempotencyKey: the create carries
// the intent's idempotency key as BOTH the Stripe Idempotency-Key header and
// the metadata mirror; success completes the reservation.
func TestStripeRefundSynchronousSuccessCarriesIdempotencyKey(t *testing.T) {
	fx := seedRefundablePayment(t, 500)
	stripe := newFakeStripeServer(t)
	cfg := stripeIntegrationConfig(config.ProviderWriteModeFull)

	row, err := fx.stripeRunner(cfg, stripe.srv.URL).EnqueueAndExecute(context.Background(), fx.stripeEnqueueParams(500))
	require.NoError(t, err)
	assert.Equal(t, StatusSucceeded, row.Status)

	wantKey := StripeRefundIntentIdempotencyKey("ch_1", 500, "")
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

	row, err := fx.stripeRunner(cfg, stripe.srv.URL).EnqueueAndExecute(context.Background(), fx.stripeEnqueueParams(500))
	require.NoError(t, err)
	require.Equal(t, StatusUnknownNeedsVerify, row.Status)

	// The refund DID get created server-side despite the 500.
	stripe.created.Store(true)
	stripe.gotMetadata.Store(StripeRefundIntentIdempotencyKey("ch_1", 500, ""))
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

	row, err := fx.stripeRunner(cfg, stripe.srv.URL).EnqueueAndExecute(context.Background(), fx.stripeEnqueueParams(500))
	require.NoError(t, err)
	assert.Equal(t, StatusFailedTerminal, row.Status)
	status, _, _ := fx.reservation(t)
	assert.Equal(t, "failed", status)
}
