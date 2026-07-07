package intents

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/open-rails/openrails/internal/railresolve"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
)

func refundIntent(t *testing.T, intentType string, payload RefundPayload) gen.OpenrailsRailIntent {
	t.Helper()
	raw, err := json.Marshal(payload)
	require.NoError(t, err)
	return gen.OpenrailsRailIntent{
		ID:             uuid.New(),
		IntentType:     intentType,
		Rail:           "mobius",
		Payload:        raw,
		IdempotencyKey: "k",
		Origin:         string(OriginAdmin),
		Attempts:       1,
		Status:         StatusInFlight,
	}
}

func testRefundPayload() RefundPayload {
	return RefundPayload{
		OriginalPaymentID: uuid.New(),
		ReservationID:     uuid.New(),
		AmountCents:       500,
		ProviderTarget:    "txn_original",
	}
}

func TestRefundIdempotencyKeysAreContentAddressed(t *testing.T) {
	assert.Equal(t, "nmi_refund:txn_1:500", NMIRefundIdempotencyKey(" txn_1 ", 500))
	assert.Equal(t, NMIRefundIdempotencyKey("txn_1", 500), NMIRefundIdempotencyKey("txn_1", 500))
	assert.NotEqual(t, NMIRefundIdempotencyKey("txn_1", 500), NMIRefundIdempotencyKey("txn_1", 600),
		"a different amount is a different logical refund")

	// Stripe keys include the reason and MATCH the provider Idempotency-Key
	// derivation, so ledger dedupe and Stripe dedupe agree.
	assert.Equal(t, "openrails-refund:ch_1:500:unspecified", StripeRefundIntentIdempotencyKey("ch_1", 500, ""))
	assert.NotEqual(t,
		StripeRefundIntentIdempotencyKey("ch_1", 500, "duplicate"),
		StripeRefundIntentIdempotencyKey("ch_1", 500, "fraudulent"))
}

func TestRefundIntentForRoutesByRail(t *testing.T) {
	stripePayment := &models.Payment{Rail: models.RailStripe}
	typ, provider, key, err := RefundIntentFor(stripePayment, "ch_1", 500, "")
	require.NoError(t, err)
	assert.Equal(t, TypeStripeRefund, typ)
	assert.Equal(t, "stripe", provider)
	assert.Equal(t, StripeRefundIntentIdempotencyKey("ch_1", 500, ""), key)

	nmiPayment := &models.Payment{Rail: models.RailNMI}
	typ, provider, key, err = RefundIntentFor(nmiPayment, "txn_1", 500, "ignored for nmi")
	require.NoError(t, err)
	assert.Equal(t, TypeNMIRefund, typ)
	assert.Equal(t, "nmi", provider)
	assert.Equal(t, NMIRefundIdempotencyKey("txn_1", 500), key)

	_, _, _, err = RefundIntentFor(nil, "x", 1, "")
	require.Error(t, err)
}

// TestNMIRefundExecuteParksBeforeProviderTraffic: unconfigured / read-only
// clients park the intent (state, not error) before any wire or DB access —
// the handler has a nil DB on purpose.
func TestNMIRefundExecuteParksBeforeProviderTraffic(t *testing.T) {
	intent := refundIntent(t, TypeNMIRefund, testRefundPayload())

	t.Run("unarmed rail parks", func(t *testing.T) {
		h := NewNMIRefundHandler(nil, fakeNMIResolver{}, nil)
		out := h.Execute(context.Background(), intent)
		assert.Equal(t, OutcomeParked, out.Class)
		assert.Contains(t, out.Reason, "not armed")
	})

	t.Run("read-only client", func(t *testing.T) {
		client := newTestNMIClient(t, "")
		client.ReadOnly = true
		h := NewNMIRefundHandler(nil, fakeNMIResolver{client: client}, nil)
		out := h.Execute(context.Background(), intent)
		assert.Equal(t, OutcomeParked, out.Class)
		assert.Contains(t, out.Reason, "read-only")
	})
}

// TestNMIRefundExecuteTransportFailureIsAmbiguous: a transport error after
// the send MAY have moved money — never retried blindly, parked for the
// verifier.
func TestNMIRefundExecuteTransportFailureIsAmbiguous(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	t.Cleanup(srv.Close)
	h := NewNMIRefundHandler(nil, fakeNMIResolver{client: newTestNMIClient(t, srv.URL)}, nil)

	out := h.Execute(context.Background(), refundIntent(t, TypeNMIRefund, testRefundPayload()))
	assert.Equal(t, OutcomeAmbiguous, out.Class)
	assert.Contains(t, out.Reason, "outcome unknown")
}

func TestNMIRefundUnusablePayloadIsTerminal(t *testing.T) {
	h := NewNMIRefundHandler(nil, fakeNMIResolver{client: newTestNMIClient(t, "")}, nil)
	intent := refundIntent(t, TypeNMIRefund, testRefundPayload())
	intent.Payload = []byte(`{"amount_cents": 0}`)
	out := h.Execute(context.Background(), intent)
	assert.Equal(t, OutcomeTerminal, out.Class)
}

// TestNMIRefundFindRefundParsesQueryShapes: the verifier read recognizes a
// successful refund action on the original transaction (GET /v5/payments/{id}),
// matched on amount; failed refund actions never count. The action id (the
// refund's own transaction id) wins when NMI reports one.
func TestNMIRefundFindRefundParsesQueryShapes(t *testing.T) {
	payload := testRefundPayload() // 500 cents
	cases := []struct {
		name      string
		status    int
		body      string
		wantFound bool
		wantTxnID string
		wantErr   bool
	}{
		{
			name: "refund action with its own transaction id",
			body: `{"object":"transaction","id":"txn_original","actions":[
				{"id":"txn_original","type":"sale","success":true,"amount":"10.00"},
				{"id":"txn_refund","type":"refund","success":true,"amount":"5.00"}
			]}`,
			wantFound: true,
			wantTxnID: "txn_refund",
		},
		{
			name: "refund action without an id falls back to the original",
			body: `{"object":"transaction","id":"txn_original","actions":[
				{"type":"sale","success":true,"amount":"10.00"},
				{"type":"refund","success":true,"amount":"-5.00"}
			]}`,
			wantFound: true,
			wantTxnID: "txn_original",
		},
		{
			name: "amount mismatch is not our refund",
			body: `{"object":"transaction","id":"txn_original","actions":[
				{"type":"refund","success":true,"amount":"3.00"}
			]}`,
			wantFound: false,
		},
		{
			name: "failed refund action does not count",
			body: `{"object":"transaction","id":"txn_original","actions":[
				{"type":"refund","success":false,"amount":"5.00"}
			]}`,
			wantFound: false,
		},
		{
			name:      "no refund anywhere",
			body:      `{"object":"transaction","id":"txn_original","actions":[{"type":"sale","success":true,"amount":"10.00"}]}`,
			wantFound: false,
		},
		{
			name:    "transaction unknown at provider is an error, not a silent no",
			status:  http.StatusNotFound,
			body:    `{"type":"notFound","error_code":"E_NOT_FOUND","message":"payment not found"}`,
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.status != 0 {
					w.WriteHeader(tc.status)
				}
				fmt.Fprint(w, tc.body)
			}))
			t.Cleanup(srv.Close)
			client := newTestNMIClient(t, srv.URL)
			h := NewNMIRefundHandler(nil, fakeNMIResolver{client: client}, nil)

			txnID, found, err := h.findRefund(client, payload)
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.wantFound, found)
			if tc.wantFound {
				assert.Equal(t, tc.wantTxnID, txnID)
			}
		})
	}
}

// fakeStripeRefundAPI scripts the Stripe surface for handler unit tests.
type fakeStripeRefundAPI struct {
	createResult *subscriptions.RefundResult
	createErr    error
	findResult   *subscriptions.RefundResult
	findFound    bool
	findErr      error
	creates      int
	gotKey       string
	gotAmount    moneyutil.Cents
}

func (f *fakeStripeRefundAPI) CreateRefund(_ context.Context, params subscriptions.RefundParams) (*subscriptions.RefundResult, error) {
	f.creates++
	f.gotKey = params.IdempotencyKey
	f.gotAmount = params.Amount
	return f.createResult, f.createErr
}

func (f *fakeStripeRefundAPI) FindRefundByIdempotencyKey(context.Context, string, string) (*subscriptions.RefundResult, bool, error) {
	return f.findResult, f.findFound, f.findErr
}

func stripeTestConfig() *config.Config {
	return &config.Config{}
}

func stripeTestRails() railresolve.FixedSet {
	return railresolve.FixedSet{
		"stripe": {Rail: models.RailStripe, Stripe: &config.StripeRailConfig{SecretKey: "sk_test_123"}},
	}
}

func TestStripeRefundExecuteClassification(t *testing.T) {
	payload := testRefundPayload()
	payload.ProviderTarget = "ch_1"

	t.Run("unconfigured stripe parks", func(t *testing.T) {
		h := NewStripeRefundHandler(nil, &config.Config{}, nil, nil)
		out := h.Execute(context.Background(), refundIntent(t, TypeStripeRefund, payload))
		assert.Equal(t, OutcomeParked, out.Class)
		assert.Contains(t, out.Reason, "stripe not configured")
	})

	t.Run("wire-pins the payload cents amount (#671)", func(t *testing.T) {
		// The intent payload carries CENTS (converted from admin-request micros
		// at prepare time, 400 on sub-cent). Execute must hand that EXACT value
		// to Stripe: 5_000_000 micros ⇒ payload 500 ⇒ RefundParams.Amount 500.
		// The create errors retryably so the (DB-backed) finalize is never reached.
		fake := &fakeStripeRefundAPI{createErr: &subscriptions.StripeAPICallError{StatusCode: 429, Message: "slow down"}}
		h := NewStripeRefundHandler(nil, stripeTestConfig(), stripeTestRails(), nil)
		h.Stripe = fake
		_ = h.Execute(context.Background(), refundIntent(t, TypeStripeRefund, payload))
		assert.Equal(t, moneyutil.Cents(500), fake.gotAmount, "stripe refund amount must be the payload's literal cents")
	})

	t.Run("sends the intent idempotency key", func(t *testing.T) {
		// The create errors retryably so the (DB-backed) finalize is never
		// reached; the provider call must still have carried the key.
		fake := &fakeStripeRefundAPI{createErr: &subscriptions.StripeAPICallError{StatusCode: 429, Message: "slow down"}}
		h := NewStripeRefundHandler(nil, stripeTestConfig(), stripeTestRails(), nil)
		h.Stripe = fake
		intent := refundIntent(t, TypeStripeRefund, payload)
		intent.IdempotencyKey = "the-intent-key"
		_ = h.Execute(context.Background(), intent)
		assert.Equal(t, "the-intent-key", fake.gotKey, "stripe mutations must carry the intent idempotency key")
	})

	t.Run("rate limit is retryable", func(t *testing.T) {
		fake := &fakeStripeRefundAPI{createErr: &subscriptions.StripeAPICallError{StatusCode: 429, Message: "slow down"}}
		h := NewStripeRefundHandler(nil, stripeTestConfig(), stripeTestRails(), nil)
		h.Stripe = fake
		out := h.Execute(context.Background(), refundIntent(t, TypeStripeRefund, payload))
		assert.Equal(t, OutcomeRetryable, out.Class)
	})

	t.Run("server error is ambiguous", func(t *testing.T) {
		fake := &fakeStripeRefundAPI{createErr: &subscriptions.StripeAPICallError{StatusCode: 500, Message: "boom"}}
		h := NewStripeRefundHandler(nil, stripeTestConfig(), stripeTestRails(), nil)
		h.Stripe = fake
		out := h.Execute(context.Background(), refundIntent(t, TypeStripeRefund, payload))
		assert.Equal(t, OutcomeAmbiguous, out.Class, "a 5xx MAY have created the refund; verify, never blind-retry")
	})

	t.Run("transport error is ambiguous", func(t *testing.T) {
		fake := &fakeStripeRefundAPI{createErr: assert.AnError}
		h := NewStripeRefundHandler(nil, stripeTestConfig(), stripeTestRails(), nil)
		h.Stripe = fake
		out := h.Execute(context.Background(), refundIntent(t, TypeStripeRefund, payload))
		assert.Equal(t, OutcomeAmbiguous, out.Class)
	})
}

func TestStripeRefundVerifyNotFoundMeansNotExecuted(t *testing.T) {
	fake := &fakeStripeRefundAPI{findFound: false}
	h := NewStripeRefundHandler(nil, stripeTestConfig(), stripeTestRails(), nil)
	h.Stripe = fake
	out := h.Verify(context.Background(), refundIntent(t, TypeStripeRefund, testRefundPayload()))
	assert.Equal(t, OutcomeRetryable, out.Class, "a clean miss proves the refund never executed; the executor may retry")
}

func TestStripeRefundVerifyReadFailureStaysAmbiguous(t *testing.T) {
	fake := &fakeStripeRefundAPI{findErr: assert.AnError}
	h := NewStripeRefundHandler(nil, stripeTestConfig(), stripeTestRails(), nil)
	h.Stripe = fake
	out := h.Verify(context.Background(), refundIntent(t, TypeStripeRefund, testRefundPayload()))
	assert.Equal(t, OutcomeAmbiguous, out.Class)
}
