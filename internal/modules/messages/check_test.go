package messages

import (
	"strings"
	"testing"

	"github.com/open-rails/openrails/billing"
)

func TestCheck(t *testing.T) {
	on := true
	receipt := func(tmpl billing.MessageTemplate) billing.MessageSettings {
		return billing.MessageSettings{Events: map[string]billing.MessageEventSettings{
			"one_off_purchase_completed": {Enabled: &on, Templates: map[string]billing.MessageTemplate{"en": tmpl}},
		}}
	}
	valid := []billing.MessageSettings{
		{},
		{DefaultLanguage: "pt-BR", Events: map[string]billing.MessageEventSettings{"premium_renewed": {Enabled: &on}}},
		receipt(billing.MessageTemplate{Subject: "Thanks {{default \"friend\" .Customer.Name}}", Text: "{{.Event.Amount}}"}),
		receipt(billing.MessageTemplate{Subject: "Thanks", HTML: "<p>{{.Event.ProductName}}</p>"}),
		{Layouts: map[string]billing.MessageLayout{"en": {Text: "{{with .Merchant.Name}}{{.}}{{end}}\n{{template \"body\" .}}"}}},
	}
	for i, s := range valid {
		if err := Check(s); err != nil {
			t.Errorf("valid %d: %v", i, err)
		}
	}
	for _, c := range []struct {
		s    billing.MessageSettings
		want string
	}{
		{receipt(billing.MessageTemplate{Subject: "{{.Event.Amount", Text: "x"}), "events.one_off_purchase_completed: subject: template: subject:1: unclosed action"},
		{receipt(billing.MessageTemplate{Subject: "x", Text: "{{.Event.Total}}"}), `events.one_off_purchase_completed: text: template: body:1:8: at <.Event.Total>: can't evaluate field Total in type billing.MessageEvent`},
		{receipt(billing.MessageTemplate{Subject: "x", HTML: "{{money .Event.Amount}}"}), `events.one_off_purchase_completed: html: template: body:1: function "money" not defined`},
		{receipt(billing.MessageTemplate{Subject: "x"}), "events.one_off_purchase_completed: a text or html body is required"},
		{receipt(billing.MessageTemplate{Text: "x"}), "events.one_off_purchase_completed: subject is required"},
		{billing.MessageSettings{Events: map[string]billing.MessageEventSettings{"order.completed": {Enabled: &on}}}, "events.order.completed: not an event type"},
		{billing.MessageSettings{DefaultLanguage: "French"}, "default_language"},
		{billing.MessageSettings{Layouts: map[string]billing.MessageLayout{"en": {HTML: "<p>no body</p>"}}}, `layouts.en: html: layout: it never runs {{template "body" .}}`},
		{billing.MessageSettings{Events: map[string]billing.MessageEventSettings{"refund_completed": {Templates: map[string]billing.MessageTemplate{"de": {Subject: "{{.Nope}}", Text: "x"}}}}},
			`events.refund_completed (de): subject: template: subject:1:2: at <.Nope>: can't evaluate field Nope`},
	} {
		err := Check(c.s)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("want %q, got %v", c.want, err)
		}
	}
}

func TestEnabled(t *testing.T) {
	off, on := false, true
	s := billing.MessageSettings{Events: map[string]billing.MessageEventSettings{
		"one_off_purchase_completed": {Enabled: &off}, "premium_renewed": {Enabled: &on}, "refund_completed": {},
	}}
	for kind, want := range map[string]bool{
		"one_off_purchase_completed": false, "premium_renewed": true, "refund_completed": true,
		"subscription_canceled": true, "usage_charged": false, "not_a_type": false,
	} {
		if got := Enabled(s, kind, billing.NotificationData{}); got != want {
			t.Errorf("%s: %v", kind, got)
		}
	}
}

func TestAnEndingConfirmsTheCancellationsItWasCausedBy(t *testing.T) {
	for reason, want := range map[string]bool{
		"user_cancel": true, "admin": true, "expired": true, "non_recoverable": true,
		"refund": false, "chargeback": false, "rail_cancel": false, "access_ended": false, "unknown": false, "": false,
	} {
		if got := Enabled(billing.MessageSettings{}, "premium_ended", billing.NotificationData{Reason: reason}); got != want {
			t.Errorf("premium_ended %q: %v", reason, got)
		}
	}
	off, on := false, true
	refund := billing.NotificationData{Reason: "refund"}
	if !Enabled(billing.MessageSettings{Events: map[string]billing.MessageEventSettings{"premium_ended": {Enabled: &on}}}, "premium_ended", refund) {
		t.Error("the merchant's enabled overrides the default")
	}
	if Enabled(billing.MessageSettings{Events: map[string]billing.MessageEventSettings{"premium_ended": {Enabled: &off}}}, "premium_ended", billing.NotificationData{Reason: "admin"}) {
		t.Error("the merchant's enabled overrides the default")
	}
	ended := billing.NotificationData{RetryStatus: billing.NotificationRetryStopped, RecoveryAction: billing.NotificationRecoveryResubscribe}
	if Enabled(billing.MessageSettings{}, "payment_method_failed", ended) {
		t.Error("the failure that ended a subscription is confirmed by the ending")
	}
	if !Enabled(billing.MessageSettings{}, "payment_method_failed", billing.NotificationData{RecoveryAction: billing.NotificationRecoveryUpdatePaymentMethod}) {
		t.Error("a failure the customer can fix is sent")
	}
}

func TestACheckoutDeclineSendsNoneByDefault(t *testing.T) {
	on := true
	for _, source := range []string{"customer_checkout", "customer_subscription_change"} {
		decline := billing.NotificationData{Source: source, RecoveryAction: billing.NotificationRecoveryUpdatePaymentMethod}
		for _, kind := range []string{"payment_method_failed", "payment_method_update_required", "payment_authentication_required", "order_collection_stopped"} {
			if Enabled(billing.MessageSettings{}, kind, decline) {
				t.Errorf("%s from %s: the checkout page told the customer", kind, source)
			}
		}
		if !Enabled(billing.MessageSettings{Events: map[string]billing.MessageEventSettings{"payment_method_failed": {Enabled: &on}}}, "payment_method_failed", decline) {
			t.Errorf("%s: the merchant's enabled overrides the default", source)
		}
	}
}
