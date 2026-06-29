package webhooks

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"testing"
)

// Each rail's representative sample payloads must normalize to the right
// unified WebhookEvent (issue #296, task 3).

func TestStripeHandlerNormalize(t *testing.T) {
	h := StripeWebhookHandler{}

	cases := []struct {
		name        string
		payload     string
		wantType    WebhookEventType
		wantRef     string
		wantSubRef  string
		wantAmount  int64
		wantCcy     string
		wantOccured int64
	}{
		{
			name:        "invoice paid -> payment succeeded",
			payload:     `{"id":"evt_1","type":"invoice.paid","created":1700000000,"data":{"object":{"id":"in_1","subscription":"sub_123","currency":"usd","amount_paid":1999}}}`,
			wantType:    WebhookEventPaymentSucceeded,
			wantRef:     "evt_1",
			wantSubRef:  "sub_123",
			wantAmount:  1999,
			wantCcy:     "USD",
			wantOccured: 1700000000,
		},
		{
			name:       "subscription deleted -> canceled, sub ref from object id",
			payload:    `{"id":"evt_2","type":"customer.subscription.deleted","data":{"object":{"id":"sub_999","currency":"eur"}}}`,
			wantType:   WebhookEventSubscriptionCanceled,
			wantRef:    "evt_2",
			wantSubRef: "sub_999",
			wantCcy:    "EUR",
		},
		{
			name:     "payment failed",
			payload:  `{"id":"evt_3","type":"invoice.payment_failed","data":{"object":{"id":"in_3","subscription":"sub_3","amount_paid":0}}}`,
			wantType: WebhookEventPaymentFailed,
			wantRef:  "evt_3", wantSubRef: "sub_3",
		},
		{
			name:     "dispute -> chargeback",
			payload:  `{"id":"evt_4","type":"charge.dispute.created","data":{"object":{"id":"dp_1"}}}`,
			wantType: WebhookEventChargeback, wantRef: "evt_4",
		},
		{
			name:     "unmapped -> unknown",
			payload:  `{"id":"evt_5","type":"customer.created","data":{"object":{"id":"cus_1"}}}`,
			wantType: WebhookEventUnknown, wantRef: "evt_5",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ev, err := h.Normalize(&WebhookMessage{Rail: "stripe", Payload: []byte(tc.payload)})
			if err != nil {
				t.Fatalf("Normalize: %v", err)
			}
			if ev.Type != tc.wantType {
				t.Errorf("Type = %q, want %q", ev.Type, tc.wantType)
			}
			if ev.RailRef != tc.wantRef {
				t.Errorf("RailRef = %q, want %q", ev.RailRef, tc.wantRef)
			}
			if tc.wantSubRef != "" && ev.SubscriptionRef != tc.wantSubRef {
				t.Errorf("SubscriptionRef = %q, want %q", ev.SubscriptionRef, tc.wantSubRef)
			}
			if tc.wantAmount != 0 && ev.Amount != tc.wantAmount {
				t.Errorf("Amount = %d, want %d", ev.Amount, tc.wantAmount)
			}
			if tc.wantCcy != "" && ev.Currency != tc.wantCcy {
				t.Errorf("Currency = %q, want %q", ev.Currency, tc.wantCcy)
			}
			if tc.wantOccured != 0 && ev.OccurredAt.Unix() != tc.wantOccured {
				t.Errorf("OccurredAt = %d, want %d", ev.OccurredAt.Unix(), tc.wantOccured)
			}
			if ev.Rail != "stripe" {
				t.Errorf("Rail = %q, want stripe", ev.Rail)
			}
			if string(ev.Raw) != tc.payload {
				t.Errorf("Raw not preserved")
			}
		})
	}
}

func TestNMIHandlerNormalize(t *testing.T) {
	h := NMIWebhookHandler{}
	cases := []struct {
		name       string
		payload    string
		wantType   WebhookEventType
		wantRef    string
		wantSubRef string
	}{
		{
			name:       "recurring add -> subscription created",
			payload:    `{"event_id":"nmi_1","event_type":"recurring.subscription.add","event_body":{"subscription_id":"subn_1"}}`,
			wantType:   WebhookEventSubscriptionCreated,
			wantRef:    "nmi_1",
			wantSubRef: "subn_1",
		},
		{
			name:       "transaction sale success -> payment succeeded",
			payload:    `{"event_id":"nmi_2","event_type":"transaction.sale.success","event_body":{"transaction_id":"t2","order_id":"ord_2","currency":"usd","amount":"19.99"}}`,
			wantType:   WebhookEventPaymentSucceeded,
			wantRef:    "nmi_2",
			wantSubRef: "ord_2",
		},
		{
			name:     "delete -> canceled",
			payload:  `{"event_id":"nmi_3","event_type":"recurring.subscription.delete","event_body":{"subscription_id":"subn_3"}}`,
			wantType: WebhookEventSubscriptionCanceled, wantRef: "nmi_3", wantSubRef: "subn_3",
		},
		{
			name:     "chargeback batch -> chargeback",
			payload:  `{"event_id":"nmi_4","event_type":"chargeback.batch.complete","event_body":{}}`,
			wantType: WebhookEventChargeback, wantRef: "nmi_4",
		},
		{
			name:     "acu updated -> customer updated",
			payload:  `{"event_id":"nmi_5","event_type":"acu.summary.automaticallyupdated","event_body":{}}`,
			wantType: WebhookEventCustomerUpdated, wantRef: "nmi_5",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ev, err := h.Normalize(&WebhookMessage{Rail: "mobius", Payload: []byte(tc.payload)})
			if err != nil {
				t.Fatalf("Normalize: %v", err)
			}
			if ev.Type != tc.wantType {
				t.Errorf("Type = %q, want %q", ev.Type, tc.wantType)
			}
			if ev.RailRef != tc.wantRef {
				t.Errorf("RailRef = %q, want %q", ev.RailRef, tc.wantRef)
			}
			if tc.wantSubRef != "" && ev.SubscriptionRef != tc.wantSubRef {
				t.Errorf("SubscriptionRef = %q, want %q", ev.SubscriptionRef, tc.wantSubRef)
			}
			if ev.Rail != "mobius" {
				t.Errorf("Rail = %q, want mobius", ev.Rail)
			}
		})
	}
}

func TestCCBillHandlerNormalize(t *testing.T) {
	h := CCBillWebhookHandler{}
	cases := []struct {
		name       string
		eventID    string
		eventType  string
		payload    string
		wantType   WebhookEventType
		wantRef    string
		wantSubRef string
	}{
		{
			name:    "new sale success -> payment succeeded",
			eventID: "cc_1", eventType: EventTypeNewSaleSuccess,
			payload:    `{"subscriptionId":"ccs_1","transactionId":"cct_1"}`,
			wantType:   WebhookEventPaymentSucceeded,
			wantRef:    "cc_1",
			wantSubRef: "ccs_1",
		},
		{
			name:    "renewal success -> renewed",
			eventID: "cc_2", eventType: EventTypeRenewalSuccess,
			payload:  `{"subscriptionId":"ccs_2"}`,
			wantType: WebhookEventSubscriptionRenewed, wantRef: "cc_2", wantSubRef: "ccs_2",
		},
		{
			name:    "cancellation -> canceled",
			eventID: "cc_3", eventType: EventTypeCancellation,
			payload:  `{"subscriptionId":"ccs_3"}`,
			wantType: WebhookEventSubscriptionCanceled, wantRef: "cc_3", wantSubRef: "ccs_3",
		},
		{
			name:    "chargeback -> chargeback",
			eventID: "cc_4", eventType: EventTypeChargeback,
			payload:  `{"subscriptionId":"ccs_4"}`,
			wantType: WebhookEventChargeback, wantRef: "cc_4", wantSubRef: "ccs_4",
		},
		{
			name:    "empty event id falls back to transactionId",
			eventID: "", eventType: EventTypeNewSaleSuccess,
			payload:  `{"subscriptionId":"ccs_5","transactionId":"cct_5"}`,
			wantType: WebhookEventPaymentSucceeded, wantRef: "cct_5", wantSubRef: "ccs_5",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ev, err := h.Normalize(&WebhookMessage{
				Rail: "ccbill", EventID: tc.eventID, EventType: tc.eventType, Payload: []byte(tc.payload),
			})
			if err != nil {
				t.Fatalf("Normalize: %v", err)
			}
			if ev.Type != tc.wantType {
				t.Errorf("Type = %q, want %q", ev.Type, tc.wantType)
			}
			if ev.RailRef != tc.wantRef {
				t.Errorf("RailRef = %q, want %q", ev.RailRef, tc.wantRef)
			}
			if ev.SubscriptionRef != tc.wantSubRef {
				t.Errorf("SubscriptionRef = %q, want %q", ev.SubscriptionRef, tc.wantSubRef)
			}
		})
	}
}

func TestWebhookHandlerRegistry(t *testing.T) {
	reg := NewWebhookHandlerRegistry()
	reg.Register(StripeWebhookHandler{})
	reg.Register(NMIWebhookHandler{})
	reg.Register(CCBillWebhookHandler{})

	// The registry is keyed by canonical rail; the legacy "mobius" webhook path
	// is folded onto "nmi" at the HTTP boundary (webhookutil.CanonicalRail), so
	// the registry only ever sees "nmi".
	for _, rail := range []string{"stripe", "STRIPE", "ccbill", "nmi"} {
		if _, ok := reg.Handler(rail); !ok {
			t.Errorf("Handler(%q) = not found, want found", rail)
		}
	}
	if h, ok := reg.Handler("nmi"); !ok || h.Rail() != "nmi" {
		t.Errorf("Handler(nmi) should resolve to the nmi handler")
	}
	if _, ok := reg.Handler("paypal"); ok {
		t.Errorf("Handler(paypal) = found, want not found")
	}
}

func TestStripeHandlerVerify(t *testing.T) {
	secret := "whsec_test"
	payload := []byte(`{"id":"evt_v","type":"invoice.paid","data":{"object":{}}}`)
	ts := "1700000000"
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(ts + "." + string(payload)))
	sig := hex.EncodeToString(mac.Sum(nil))
	header := fmt.Sprintf("t=%s,v1=%s", ts, sig)

	// tolerance 0 disables the timestamp window so the fixed ts stays valid.
	h := StripeWebhookHandler{Secret: secret, Tolerance: 0}

	if err := h.Verify(&WebhookMessage{Rail: "stripe", Signature: header, Payload: payload}); err != nil {
		t.Fatalf("Verify(valid) = %v, want nil", err)
	}
	if err := h.Verify(&WebhookMessage{Rail: "stripe", Signature: header, Payload: []byte(`{"tampered":true}`)}); err == nil {
		t.Fatalf("Verify(tampered) = nil, want error")
	}
}

func TestNMIHandlerVerify(t *testing.T) {
	secret := "nmi_secret"
	payload := []byte(`{"event_id":"n","event_type":"transaction.sale.success","event_body":{}}`)
	ts := "1700000000"
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(ts + "." + string(payload)))
	sig := hex.EncodeToString(mac.Sum(nil))
	header := fmt.Sprintf("t=%s,s=%s", ts, sig)

	h := NMIWebhookHandler{SecretFor: func(string) string { return secret }}
	if err := h.Verify(&WebhookMessage{Rail: "mobius", Signature: header, Payload: payload}); err != nil {
		t.Fatalf("Verify(valid) = %v, want nil", err)
	}
	if err := h.Verify(&WebhookMessage{Rail: "mobius", Signature: header, Payload: []byte(`x`)}); err == nil {
		t.Fatalf("Verify(tampered) = nil, want error")
	}
}
