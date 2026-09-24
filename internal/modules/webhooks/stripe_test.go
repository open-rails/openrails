package webhooks

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/pkg/merchant"
)

func TestRedactStripeClientSecretsPreservesExactNumbers(t *testing.T) {
	t.Parallel()
	raw := []byte(`{"client_secret":"top","id":"evt_fixture","data":{"object":{"amount":9007199254740993,"decimal":12345678901234567890.012300,"exponent":1.234567890123456789e+40,"client_secret_hint":"visible","description":"client_secret is a field","nested":[{"client_secret":"nested","amount":-9007199254740993},[{"client_secret":"deep"}]]},"previous_attributes":{"client_secret":"previous","amount":9007199254740995}}}`)
	scrubbed, err := redactStripeClientSecrets(raw)
	require.NoError(t, err)
	require.True(t, json.Valid(scrubbed))
	require.NotContains(t, string(scrubbed), `"client_secret":`)
	for _, exact := range []string{`"amount":9007199254740993`, `"amount":-9007199254740993`, `"amount":9007199254740995`, `"decimal":12345678901234567890.012300`, `"exponent":1.234567890123456789e+40`, `"client_secret_hint":"visible"`, `"description":"client_secret is a field"`, `"id":"evt_fixture"`} {
		require.Contains(t, string(scrubbed), exact)
	}
	again, err := redactStripeClientSecrets(scrubbed)
	require.NoError(t, err)
	require.Equal(t, scrubbed, again, "idempotent")
	for _, invalid := range []string{`{"client_secret":`, `{} {}`, ``} {
		_, err := redactStripeClientSecrets([]byte(invalid))
		require.Error(t, err, invalid)
	}
}

// Wire pin: Stripe invoice cents become exact micros; success records amount_paid, failure amount_due.
func TestStripeInvoiceWireShapes(t *testing.T) {
	t.Parallel()
	parse := func(raw string) stripeInvoice {
		var inv stripeInvoice
		require.NoError(t, json.Unmarshal([]byte(raw), &inv))
		return inv
	}
	for _, tc := range []struct {
		json         string
		paid, failed int64
	}{
		{`{"amount_paid":1999,"amount_due":2500}`, 19_990_000, 25_000_000},
		{`{"amount_paid":1,"amount_due":0}`, 10_000, 0},
		{`{"amount_paid":0,"amount_due":1200}`, 0, 12_000_000},
		{`{"amount_paid":9999,"amount_due":1200}`, 99_990_000, 12_000_000},
	} {
		inv := parse(tc.json)
		require.Equal(t, tc.paid, stripeInvoicePaidAmountMicros(inv), tc.json)
		require.Equal(t, tc.failed, stripeInvoiceFailedAmountMicros(inv), tc.json)
	}

	// 2026-04-22.preview: subscription + metadata under parent, price under pricing.price_details.
	preview := parse(`{"id":"in_1","lines":{"data":[{"pricing":{"price_details":{"price":"price_preview"}}}]},
		"parent":{"subscription_details":{"subscription":"sub_preview","metadata":{"user_id":"u1"}}}}`)
	require.Equal(t, "sub_preview", preview.railSubscriptionID())
	require.Equal(t, "u1", preview.Parent.SubscriptionDetails.Metadata["user_id"])
	require.Equal(t, "price_preview", preview.Lines.Data[0].priceID())

	classic := parse(`{"subscription":" sub_top ","lines":{"data":[{"price":{"id":"price_classic"},"pricing":{"price_details":{"price":"price_preview"}}}]},
		"parent":{"subscription_details":{"subscription":"sub_parent"}}}`)
	require.Equal(t, "sub_top", classic.railSubscriptionID(), "top-level wins")
	require.Equal(t, "price_classic", classic.Lines.Data[0].priceID())
	require.Empty(t, parse(`{}`).railSubscriptionID())

	var ip stripeInvoicePayment
	require.NoError(t, json.Unmarshal([]byte(`{"id":"inpay_1","invoice":"in_1","payment":{"type":"payment_intent","payment_intent":"pi_1"}}`), &ip))
	require.Equal(t, "in_1", ip.Invoice)
	require.Equal(t, "pi_1", ip.Payment.PaymentIntent)
}

func TestStripeCardSnapshotAndLockKeys(t *testing.T) {
	t.Parallel()
	require.Equal(t, []string{"ch_1", "pi_1", "in_1"}, stripeChargeSnapshotTransactionIDs(stripeCharge{ID: " ch_1 ", PaymentIntent: " pi_1 ", Invoice: " in_1 "}))
	require.Equal(t, []string{"in_1"}, stripeChargeSnapshotTransactionIDs(stripeCharge{ID: " ", Invoice: "in_1"}))
	require.Empty(t, stripeChargeSnapshotTransactionIDs(stripeCharge{}))

	m, p := uuid.MustParse("5b8a5905-4231-4e94-9fcc-e8dbf6eb0367"), uuid.MustParse("7319679b-ffdc-4e69-be1d-538458ee2cdb")
	require.Equal(t, "payments:stripe:"+m.String()+":"+p.String()+":customer:cus_1", stripePaymentStateLockKey(m, p, stripeCustomerLockSubject, " cus_1 "))
	require.Equal(t, "payments:stripe:"+m.String()+":"+p.String()+":method:pm_1", stripePaymentStateLockKey(m, p, stripePaymentMethodLockSubject, "pm_1"))
}

func TestStripeRefundAndDisputeDecisions(t *testing.T) {
	t.Parallel()
	for status, want := range map[string]bool{"succeeded": true, " Succeeded ": true, "pending": false, "failed": false, "": false} {
		require.Equal(t, want, stripeRefundSucceeded(status), status)
	}
	for _, tc := range []struct {
		eventType, status string
		reverse, won      bool
	}{
		{"charge.dispute.created", "needs_response", true, false},
		{"charge.dispute.created", "", true, false},
		{"charge.dispute.created", "won", false, false},
		{"charge.dispute.closed", "lost", true, false},
		{"charge.dispute.closed", " Lost ", true, false},
		{"charge.dispute.closed", "won", false, true},
		{"charge.dispute.closed", "under_review", false, false},
		{"charge.dispute.updated", "lost", false, false},
	} {
		require.Equal(t, tc.reverse, stripeDisputeShouldReverse(tc.eventType, tc.status), "%s/%s", tc.eventType, tc.status)
		require.Equal(t, tc.won, stripeDisputeWon(tc.eventType, tc.status), "%s/%s", tc.eventType, tc.status)
	}
}

type recordingCheckoutSessionStore struct {
	closedID     uuid.UUID
	closedStatus models.CheckoutSessionStatus
}

func (*recordingCheckoutSessionStore) FindOpenCCBillReservation(context.Context, string, string, uuid.UUID) (*models.CheckoutSession, error) {
	return nil, nil
}

func (*recordingCheckoutSessionStore) FindOpenByUserPriceRail(context.Context, string, uuid.UUID, models.Rail) (*models.CheckoutSession, error) {
	return nil, nil
}

func (*recordingCheckoutSessionStore) MarkSucceeded(context.Context, uuid.UUID, uuid.UUID, string) error {
	return nil
}

func (*recordingCheckoutSessionStore) MarkSucceededWithSubscription(context.Context, uuid.UUID, uuid.UUID, string, uuid.UUID) error {
	return nil
}

func (*recordingCheckoutSessionStore) MarkFailed(context.Context, uuid.UUID, string, string) error {
	return nil
}

func (s *recordingCheckoutSessionStore) MarkProviderCheckoutClosed(_ context.Context, id uuid.UUID, status models.CheckoutSessionStatus) error {
	s.closedID, s.closedStatus = id, status
	return nil
}

func stripeEventJSON(t *testing.T, eventType string, object any) []byte {
	t.Helper()
	payload, err := json.Marshal(map[string]any{"id": "evt_" + uuid.NewString(), "type": eventType, "created": 1700000000, "data": map[string]any{"object": object}})
	require.NoError(t, err)
	return payload
}

// #684: subscription-state events only identify the dirty subscription; the rest route by type.
func TestStripeWebhookRouting(t *testing.T) {
	t.Parallel()
	merchantID, pspID := uuid.New(), uuid.New()
	ctx := db.WithPSPID(merchant.WithID(context.Background(), merchant.ID(merchantID)), pspID)

	for _, tc := range []struct {
		eventType string
		object    any
		ref       string
	}{
		{"invoice.paid", map[string]any{"id": "in_1", "subscription": "sub_classic", "amount_paid": 1999}, "sub_classic"},
		{"invoice.payment_failed", map[string]any{"id": "in_2", "parent": map[string]any{"subscription_details": map[string]any{"subscription": "sub_preview"}}}, "sub_preview"},
		{"customer.subscription.deleted", map[string]any{"id": "sub_deleted"}, "sub_deleted"},
		{"invoice.paid", map[string]any{"id": "in_one_off"}, ""},
	} {
		enq := &recordingConvergeEnqueuer{}
		require.NoError(t, (&StripeWebhookService{ConvergeEnqueuer: enq}).HandleStripeWebhook(ctx, stripeEventJSON(t, tc.eventType, tc.object)), tc.eventType)
		if tc.ref == "" {
			require.Empty(t, enq.requests, "one-off invoices carry no subscription state")
			continue
		}
		require.Equal(t, []ConvergeRequest{{MerchantID: merchantID, PSPID: pspID, Rail: "stripe", SubscriptionReference: tc.ref, EventType: tc.eventType, EventCreated: 1700000000}}, enq.requests)
	}

	bare := &StripeWebhookService{}
	for _, tc := range []struct {
		eventType string
		object    any
		errText   string
	}{
		{"checkout.session.completed", map[string]any{}, "stripe checkout missing user_id metadata"},
		{"checkout.session.async_payment_succeeded", map[string]any{}, "stripe checkout missing user_id metadata"},
		{"charge.dispute.created", map[string]any{"id": "dp_1", "amount": 100, "status": "needs_response"}, "payment service is required for stripe dispute"},
		{"charge.dispute.closed", map[string]any{"id": "dp_1", "amount": 100, "status": "lost"}, "payment service is required for stripe dispute"},
		{"charge.dispute.closed", map[string]any{"id": "dp_1", "amount": 100, "status": "won"}, ""},
		{"charge.dispute.closed", map[string]any{"id": "dp_1", "amount": 100, "status": "under_review"}, ""},
		{"invoice_payment.paid", map[string]any{"invoice": "in_1", "payment": map[string]any{"payment_intent": "pi_1"}}, ""},
		{"payment_intent.succeeded", map[string]any{"id": "pi_1"}, ""},
		{"payment_intent.succeeded", map[string]any{"id": "pi_1", "metadata": map[string]any{"openrails_engine_operation": "not-a-uuid"}}, "no valid local operation"},
		{"product.updated", map[string]any{"id": "prod_1"}, ""},
	} {
		err := bare.HandleStripeWebhook(ctx, stripeEventJSON(t, tc.eventType, tc.object))
		if tc.errText == "" {
			require.NoError(t, err, "%s %v", tc.eventType, tc.object)
		} else {
			require.ErrorContains(t, err, tc.errText, tc.eventType)
		}
	}

	require.ErrorContains(t, bare.HandleStripeWebhook(ctx, []byte(`{"type":"invoice.paid","data":{"object":{}}}`)), "missing id or type")
	require.Error(t, bare.HandleStripeWebhook(ctx, []byte(`{"id":"evt_1","type":"invoice.paid"} {}`)), "trailing data")

	sessionID := uuid.New()
	store := &recordingCheckoutSessionStore{}
	svc := &StripeWebhookService{CheckoutSessionService: store}
	meta := map[string]any{"metadata": map[string]string{"checkout_session_id": openrails.CheckoutSessionID(sessionID).String()}}
	require.NoError(t, svc.HandleStripeWebhook(ctx, stripeEventJSON(t, "checkout.session.async_payment_failed", meta)))
	require.Equal(t, sessionID, store.closedID)
	require.Equal(t, models.CheckoutSessionStatusFailed, store.closedStatus)
	*store = recordingCheckoutSessionStore{}
	require.NoError(t, svc.HandleStripeWebhook(ctx, stripeEventJSON(t, "checkout.session.expired", meta)))
	require.Equal(t, sessionID, store.closedID)
	require.Equal(t, models.CheckoutSessionStatusExpired, store.closedStatus)
	*store = recordingCheckoutSessionStore{}
	require.NoError(t, svc.HandleStripeWebhook(ctx, stripeEventJSON(t, "checkout.session.expired", map[string]any{"metadata": map[string]string{"checkout_session_id": "garbage"}})))
	require.Equal(t, uuid.Nil, store.closedID, "an unparseable session id closes nothing")
}
