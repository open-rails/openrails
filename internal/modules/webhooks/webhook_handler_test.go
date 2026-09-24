package webhooks

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/railresolve"
)

// Every rail normalizes onto the same rail-agnostic WebhookEvent (#296).
func TestWebhookNormalize(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		handler WebhookHandler
		msg     WebhookMessage
		want    WebhookEvent
	}{
		{"stripe invoice paid", StripeWebhookHandler{},
			WebhookMessage{Rail: "stripe", Payload: []byte(`{"id":"evt_1","type":"invoice.paid","created":1700000000,"data":{"object":{"id":"in_1","subscription":"sub_1","currency":"usd","amount_paid":1999,"amount":5}}}`)},
			WebhookEvent{Rail: "stripe", Type: WebhookEventPaymentSucceeded, RawType: "invoice.paid", RailRef: "evt_1", SubscriptionRef: "sub_1", Amount: 1999, Currency: "USD"}},
		{"stripe subscription deleted uses object id", StripeWebhookHandler{},
			WebhookMessage{Payload: []byte(`{"id":"evt_2","type":"customer.subscription.deleted","data":{"object":{"id":"sub_9","currency":"eur"}}}`)},
			WebhookEvent{Rail: "stripe", Type: WebhookEventSubscriptionCanceled, RawType: "customer.subscription.deleted", RailRef: "evt_2", SubscriptionRef: "sub_9", Currency: "EUR"}},
		{"stripe checkout total", StripeWebhookHandler{},
			WebhookMessage{Payload: []byte(`{"id":"evt_3","type":"checkout.session.completed","data":{"object":{"id":"cs_1","amount_total":500,"amount":7}}}`)},
			WebhookEvent{Rail: "stripe", Type: WebhookEventCheckoutCompleted, RawType: "checkout.session.completed", RailRef: "evt_3", Amount: 500}},
		{"stripe dispute", StripeWebhookHandler{},
			WebhookMessage{Payload: []byte(`{"id":"evt_4","type":"charge.dispute.closed","data":{"object":{"id":"dp_1","amount":100}}}`)},
			WebhookEvent{Rail: "stripe", Type: WebhookEventChargeback, RawType: "charge.dispute.closed", RailRef: "evt_4", Amount: 100}},
		{"stripe unmapped", StripeWebhookHandler{},
			WebhookMessage{Payload: []byte(`{"id":"evt_5","type":"customer.created","data":{"object":{"id":"cus_1"}}}`)},
			WebhookEvent{Rail: "stripe", Type: WebhookEventUnknown, RawType: "customer.created", RailRef: "evt_5"}},
		{"nmi recurring add keeps alias rail", NMIWebhookHandler{},
			WebhookMessage{Rail: "mobius", Payload: []byte(`{"event_id":"n1","event_type":"recurring.subscription.add","event_body":{"subscription_id":123456789012345678}}`)},
			WebhookEvent{Rail: "mobius", Type: WebhookEventSubscriptionCreated, RawType: "recurring.subscription.add", RailRef: "n1", SubscriptionRef: "123456789012345678"}},
		{"nmi sale", NMIWebhookHandler{},
			WebhookMessage{Rail: "nmi", Payload: []byte(`{"event_id":"n2","event_type":"transaction.sale.success","event_body":{"order_id":"ord_2","currency":"usd","amount":"19.99"}}`)},
			WebhookEvent{Rail: "nmi", Type: WebhookEventPaymentSucceeded, RawType: "transaction.sale.success", RailRef: "n2", SubscriptionRef: "ord_2", Currency: "USD"}},
		{"nmi refund failure is unknown", NMIWebhookHandler{},
			WebhookMessage{Rail: "nmi", Payload: []byte(`{"event_id":"n3","event_type":"transaction.refund.failure","event_body":{}}`)},
			WebhookEvent{Rail: "nmi", Type: WebhookEventUnknown, RawType: "transaction.refund.failure", RailRef: "n3"}},
		{"nmi chargeback", NMIWebhookHandler{},
			WebhookMessage{Rail: "nmi", Payload: []byte(`{"event_id":"n4","event_type":"chargeback.batch.complete","event_body":{}}`)},
			WebhookEvent{Rail: "nmi", Type: WebhookEventChargeback, RawType: "chargeback.batch.complete", RailRef: "n4"}},
		{"ccbill renewal", CCBillWebhookHandler{},
			WebhookMessage{EventID: "cc_1", EventType: EventTypeRenewalSuccess, Payload: []byte(`{"subscriptionId":"ccs_1","transactionId":"cct_1"}`)},
			WebhookEvent{Rail: "ccbill", Type: WebhookEventSubscriptionRenewed, RawType: EventTypeRenewalSuccess, RailRef: "cc_1", SubscriptionRef: "ccs_1"}},
		{"ccbill falls back to transactionId", CCBillWebhookHandler{},
			WebhookMessage{EventType: EventTypeChargeback, Payload: []byte(`{"subscriptionId":"ccs_2","transactionId":"cct_2"}`)},
			WebhookEvent{Rail: "ccbill", Type: WebhookEventChargeback, RawType: EventTypeChargeback, RailRef: "cct_2", SubscriptionRef: "ccs_2"}},
		{"basistheory token", BasisTheoryWebhookHandler{},
			WebhookMessage{Payload: []byte(`{"id":"bt_1","type":"token.updated"}`)},
			WebhookEvent{Rail: string(models.EventSourceBasisTheory), Type: WebhookEventCustomerUpdated, RawType: "token.updated", RailRef: "bt_1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.handler.Normalize(&tc.msg)
			require.NoError(t, err)
			require.Equal(t, tc.msg.Payload, got.Raw, "raw payload preserved verbatim")
			if tc.name == "stripe invoice paid" {
				require.EqualValues(t, 1700000000, got.OccurredAt.Unix())
			}
			got.Raw, got.OccurredAt = nil, tc.want.OccurredAt
			require.Equal(t, tc.want, got)
		})
	}
	for _, h := range []WebhookHandler{StripeWebhookHandler{}, NMIWebhookHandler{}} {
		_, err := h.Normalize(&WebhookMessage{Payload: []byte(`{`)})
		require.Error(t, err, h.Rail())
	}
}

func TestWebhookHandlerRegistry(t *testing.T) {
	t.Parallel()
	reg := (&WebhookDispatcher{}).webhookRegistry()
	bt := string(models.EventSourceBasisTheory)
	for rail, want := range map[string]string{"stripe": "stripe", " STRIPE ": "stripe", "ccbill": "ccbill", "nmi": "nmi", bt: bt} {
		h, ok := reg.Handler(rail)
		require.True(t, ok, rail)
		require.Equal(t, want, h.Rail())
	}
	// "mobius" is folded onto "nmi" at the HTTP boundary; the registry never sees it.
	for _, rail := range []string{"mobius", "unknown", ""} {
		_, ok := reg.Handler(rail)
		require.False(t, ok, rail)
	}
	var nilReg *WebhookHandlerRegistry
	_, ok := nilReg.Handler("stripe")
	require.False(t, ok)
}

type stripePaymentStateStub struct{}

func (stripePaymentStateStub) PaymentMethod(context.Context, string) (*payments.StripePaymentMethodState, error) {
	return nil, nil
}

func (stripePaymentStateStub) CustomerPaymentState(context.Context, string) (*payments.StripeCustomerPaymentState, error) {
	return nil, nil
}

// Queued jobs process only what ingestion verified; credentials come from the routed account.
func TestWebhookDispatcherGates(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	verified, unverified := true, false
	d := &WebhookDispatcher{}

	for name, msg := range map[string]*WebhookMessage{
		"stripe unsigned":      {Rail: "stripe", Payload: []byte(`{"id":"evt_1","type":"invoice.paid","data":{"object":{}}}`)},
		"stripe unverified":    {Rail: "stripe", SignatureValid: &unverified, Payload: []byte(`{"id":"evt_1","type":"invoice.paid","data":{"object":{}}}`)},
		"nmi unsigned":         {Rail: "nmi", Payload: []byte(`{"event_id":"n","event_type":"transaction.sale.success","event_body":{}}`)},
		"nmi bad queued hmac":  {Rail: "nmi", SignatureValid: &verified, SigningSecret: "s", Signature: "t=1,s=00", Payload: []byte(`{}`)},
		"basistheory unsigned": {Rail: string(models.EventSourceBasisTheory), Payload: []byte(`{"id":"bt_1","type":"token.updated"}`)},
	} {
		err := d.Process(ctx, msg)
		require.Error(t, err, name)
		require.True(t, IsWebhookErrorNonRetryable(err), name)
	}

	require.ErrorContains(t, d.Process(ctx, &WebhookMessage{Rail: "paypal"}), "unsupported webhook rail")
	require.Error(t, d.Process(ctx, nil))
	require.NoError(t, d.Process(ctx, &WebhookMessage{Rail: "stripe", SignatureValid: &verified, EventType: "product.updated", Payload: []byte(`{"id":"evt_1","type":"product.updated","data":{"object":{}}}`)}),
		"a verified event that needs no provider state resolves no credentials")

	// CCBill: an unarmed rail is retryable (redelivered once armed), never default-allow.
	err := d.Process(ctx, &WebhookMessage{Rail: "ccbill", EventType: EventTypeRenewalSuccess, Payload: []byte(`{}`)})
	require.ErrorContains(t, err, "rail resolution is not configured")
	require.False(t, IsWebhookErrorNonRetryable(err))

	var selected string
	routed := &WebhookDispatcher{
		RailConfigs: railresolve.FixedSet{
			"stripe_a": {Rail: models.RailStripe, AccountID: "acct_a", Stripe: &config.StripeRailConfig{SecretKey: "sk_test_a"}},
			"stripe_b": {Rail: models.RailStripe, AccountID: "acct_b", Stripe: &config.StripeRailConfig{SecretKey: "sk_test_b"}},
		},
		StripePaymentStateReaderFactory: func(secretKey string) payments.StripePaymentStateReader {
			selected = secretKey
			return stripePaymentStateStub{}
		},
	}
	err = routed.Process(ctx, &WebhookMessage{Rail: "stripe", EventType: "customer.updated", PspID: "acct_b", SignatureValid: &verified,
		Payload: []byte(`{"id":"evt_1","type":"customer.updated","data":{"object":{"id":"cus_1"}}}`)})
	require.ErrorContains(t, err, "database is not configured")
	require.Equal(t, "sk_test_b", selected)

	err = routed.Process(ctx, &WebhookMessage{Rail: "stripe", EventType: "customer.updated", PspID: "acct_unknown", SignatureValid: &verified,
		Payload: []byte(`{"id":"evt_1","type":"customer.updated","data":{"object":{"id":"cus_1"}}}`)})
	require.ErrorContains(t, err, "stripe webhook rejected")
}
