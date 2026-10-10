package intents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/integrations/stripeapi"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/railresolve"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
)

type fakeNMIResolver struct {
	client *nmi.NMIClient
	err    error
}

func (f fakeNMIResolver) ResolveNMIClient(context.Context, uuid.UUID, *uuid.UUID) (*nmi.NMIClient, bool, error) {
	return f.client, f.client != nil, f.err
}

func testNMIClient(t *testing.T, url string, readOnly bool) *nmi.NMIClient {
	t.Helper()
	client, err := nmi.NewAccountClient(uuid.New(), uuid.New(), "nmi", &config.NMIProviderSettings{SecurityKey: "test_security_key", WebhookSecret: "test_secret"}, true)
	require.NoError(t, err)
	client.LoopbackFixture, client.ReadOnly = true, readOnly
	if url != "" {
		client.DirectPostURL, client.QueryURL, client.V5BaseURL = url, url, url
	}
	return client
}

func refundIntent(t *testing.T, typ string, mutate func(*RefundPayload)) gen.BillingProviderIntent {
	p := RefundPayload{Currency: "USD", OriginalPaymentID: uuid.New(), ReservationID: uuid.New(), AmountCents: 500, ProviderTarget: "txn_original"}
	if mutate != nil {
		mutate(&p)
	}
	raw, err := json.Marshal(p)
	require.NoError(t, err)
	return gen.BillingProviderIntent{ID: uuid.New(), IntentType: typ, Rail: "nmi", Payload: raw, IdempotencyKey: "the-intent-key", Origin: string(OriginAdmin), Attempts: 1, Status: StatusInFlight}
}

func TestNMIHandlersParkBeforeProviderTraffic(t *testing.T) {
	sub, psp := uuid.New(), uuid.New()
	deleteIntent := gen.BillingProviderIntent{ID: uuid.New(), IntentType: TypeNMIDeleteSubscription, Rail: "nmi", SubscriptionID: &sub, PspID: &psp,
		Payload: []byte(`{"rail_subscription_id":"sub-target"}`), Origin: string(OriginUser), Attempts: 1, Status: StatusInFlight}
	unaddressed := deleteIntent
	unaddressed.PspID = nil
	readOnly := fakeNMIResolver{client: testNMIClient(t, "", true)}
	for _, tc := range []struct {
		name   string
		h      Handler
		intent gen.BillingProviderIntent
		reason string
	}{
		{"delete: unarmed", NewNMIDeleteHandler(nil, nil, fakeNMIResolver{}, nil), deleteIntent, "not armed"},
		{"delete: resolver error fails closed", NewNMIDeleteHandler(nil, nil, fakeNMIResolver{err: errors.New("vault down")}, nil), deleteIntent, "fail closed"},
		{"delete: no resolver", NewNMIDeleteHandler(nil, nil, nil, nil), deleteIntent, "fail closed"},
		{"delete: read-only client", NewNMIDeleteHandler(nil, nil, readOnly, nil), deleteIntent, "read-only"},
		{"delete: unaccepted target", NewNMIDeleteHandler(nil, nil, readOnly, nil), unaddressed, "no accepted subscription/provider address"},
		{"refund: unarmed", NewNMIRefundHandler(nil, fakeNMIResolver{}, nil), refundIntent(t, TypeNMIRefund, nil), "not armed"},
		{"refund: read-only client", NewNMIRefundHandler(nil, readOnly, nil), refundIntent(t, TypeNMIRefund, nil), "read-only"},
	} {
		out := tc.h.Execute(context.Background(), tc.intent)
		assert.Equal(t, OutcomeParked, out.Class, tc.name)
		assert.Contains(t, out.Reason, tc.reason, tc.name)
	}
	assert.Equal(t, OutcomeAmbiguous, NewNMIDeleteHandler(nil, nil, fakeNMIResolver{}, nil).Verify(context.Background(), deleteIntent).Class,
		"cannot verify: stay unknown")
}

// A money mover never blind-retries: a transport failure after the send is ambiguous.
func TestNMIRefundOutcomeClassification(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusBadGateway) }))
	t.Cleanup(srv.Close)
	h := NewNMIRefundHandler(nil, fakeNMIResolver{client: testNMIClient(t, srv.URL, false)}, nil)
	out := h.Execute(context.Background(), refundIntent(t, TypeNMIRefund, nil))
	assert.Equal(t, OutcomeAmbiguous, out.Class)
	assert.Contains(t, out.Reason, "outcome unknown")

	bad := refundIntent(t, TypeNMIRefund, nil)
	bad.Payload = []byte(`{"amount_cents": 0}`)
	assert.Equal(t, OutcomeTerminal, h.Execute(context.Background(), bad).Class)
}

// NMI delete's pre-write read: 200 active = present; 200 inactive tombstone or 404 = gone.
func TestNMISubscriptionPresence(t *testing.T) {
	for _, tc := range []struct {
		name    string
		status  int
		body    string
		present bool
		wantErr bool
	}{
		{"present", http.StatusOK, `{"object":"subscription","id":"sub-1","delayed_condition":"active"}`, true, false},
		{"inactive tombstone", http.StatusOK, `{"object":"subscription","id":"sub-1","delayed_condition":"inactive"}`, false, false},
		{"not found", http.StatusNotFound, `{"type":"notFound","error_code":"E_NOT_FOUND","message":"subscription not found"}`, false, false},
		{"auth error", http.StatusUnauthorized, `{"type":"authenticationError","error_code":"E_AUTH","message":"invalid security key"}`, false, true},
		{"garbage", http.StatusOK, `not json at all`, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, "/subscriptions/sub-1", r.URL.Path)
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			present, err := (&NMIDeleteHandler{}).subscriptionPresent(context.Background(), testNMIClient(t, srv.URL, false), "sub-1")
			if tc.wantErr {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.present, present)
		})
	}
}

type fakeStripeRefunds struct {
	result    *subscriptions.RefundResult
	createErr error
	found     bool
	findErr   error
	gotParams subscriptions.RefundParams
}

func (f *fakeStripeRefunds) CreateRefund(_ context.Context, p subscriptions.RefundParams) (*subscriptions.RefundResult, error) {
	f.gotParams = p
	return f.result, f.createErr
}
func (f *fakeStripeRefunds) FindRefundByIdempotencyKey(context.Context, string, string) (*subscriptions.RefundResult, bool, error) {
	return f.result, f.found, f.findErr
}

// Stripe refunds carry the intent key and classify so that only a
// provably-unexecuted refund is retried.
func TestStripeRefundClassification(t *testing.T) {
	rails := railresolve.FixedSet{"stripe": {Rail: models.RailStripe, Stripe: &config.StripeRailConfig{SecretKey: "sk_test_123"}}}
	apiErr := func(code int) error { return &subscriptions.StripeAPICallError{StatusCode: code, Message: "x"} }
	pending := &subscriptions.RefundResult{ID: "re_pending", Status: "pending"}
	intent := refundIntent(t, TypeStripeRefund, func(p *RefundPayload) { p.ProviderTarget = "ch_1" })

	unconfigured := NewStripeRefundHandler(nil, &config.Config{}, nil, nil, nil).Execute(context.Background(), intent)
	assert.Equal(t, OutcomeParked, unconfigured.Class)
	assert.Contains(t, unconfigured.Reason, "stripe not configured")

	for _, tc := range []struct {
		name   string
		fake   fakeStripeRefunds
		verify bool
		want   OutcomeClass
	}{
		{"rate limit is a clean retry", fakeStripeRefunds{createErr: apiErr(429)}, false, OutcomeRetryable},
		{"5xx may have created it", fakeStripeRefunds{createErr: apiErr(500)}, false, OutcomeAmbiguous},
		{"transport error may have created it", fakeStripeRefunds{createErr: errors.New("reset")}, false, OutcomeAmbiguous},
		{"readonly choke parks", fakeStripeRefunds{createErr: fmt.Errorf("post: %w", stripeapi.ErrProviderReadOnly)}, false, OutcomeParked},
		{"pending is not completion", fakeStripeRefunds{result: pending}, false, OutcomeAmbiguous},
		{"verify: clean miss proves not executed", fakeStripeRefunds{}, true, OutcomeRetryable},
		{"verify: read failure stays unknown", fakeStripeRefunds{findErr: errors.New("down")}, true, OutcomeAmbiguous},
		{"verify: pending is not completion", fakeStripeRefunds{result: pending, found: true}, true, OutcomeAmbiguous},
	} {
		h := NewStripeRefundHandler(nil, &config.Config{}, rails, nil, nil)
		fake := tc.fake
		h.Stripe = &fake
		var out Outcome
		if tc.verify {
			out = h.Verify(context.Background(), intent)
		} else {
			out = h.Execute(context.Background(), intent)
			assert.Equal(t, "the-intent-key", fake.gotParams.IdempotencyKey, tc.name)
			assert.Equal(t, moneyutil.Cents(500), fake.gotParams.Amount, "%s: payload cents reach Stripe verbatim (#671)", tc.name)
			assert.Equal(t, "ch_1", fake.gotParams.ChargeID, tc.name)
		}
		assert.Equal(t, tc.want, out.Class, tc.name)
	}
}

func TestCCBillRefundAlwaysRequiresOperatorVerification(t *testing.T) {
	h := NewCCBillRefundHandler(nil, nil)
	for _, attempts := range []int32{1, 2, 4} {
		row := refundIntent(t, TypeCCBillRefund, func(p *RefundPayload) { p.ProviderTarget, p.ProviderTransactionID = "sub_123", "requested_charge" })
		row.Attempts = attempts
		for _, out := range []Outcome{h.Execute(context.Background(), row), h.Verify(context.Background(), row)} {
			assert.Equal(t, OutcomeAmbiguous, out.Class)
			assert.Contains(t, out.Reason, "operator must verify")
		}
	}
	keepPayload, keepEvidence := h.PrunePolicy()
	assert.True(t, keepPayload && keepEvidence)
}
