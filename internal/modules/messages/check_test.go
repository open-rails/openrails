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
		{receipt(billing.MessageTemplate{Subject: "x", Text: "{{.Event.Total}}"}), `events.one_off_purchase_completed: text: template: body:1:8: executing "body" at <.Event.Total>: can't evaluate field Total`},
		{receipt(billing.MessageTemplate{Subject: "x", HTML: "{{money .Event.Amount}}"}), `events.one_off_purchase_completed: html: template: body:1: function "money" not defined`},
		{receipt(billing.MessageTemplate{Subject: "x"}), "events.one_off_purchase_completed: a text or html body is required"},
		{receipt(billing.MessageTemplate{Text: "x"}), "events.one_off_purchase_completed: subject is required"},
		{billing.MessageSettings{Events: map[string]billing.MessageEventSettings{"order.completed": {Enabled: &on}}}, "events.order.completed: not an event type"},
		{billing.MessageSettings{DefaultLanguage: "French"}, "default_language"},
		{billing.MessageSettings{Layouts: map[string]billing.MessageLayout{"en": {HTML: "<p>no body</p>"}}}, `layouts.en: html: layout: it never runs {{template "body" .}}`},
		{billing.MessageSettings{Events: map[string]billing.MessageEventSettings{"refund_completed": {Templates: map[string]billing.MessageTemplate{"de": {Subject: "{{.Nope}}", Text: "x"}}}}},
			`events.refund_completed (de): subject: template: subject:1:2: executing "subject" at <.Nope>`},
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
		if got := Enabled(s, kind); got != want {
			t.Errorf("%s: %v", kind, got)
		}
	}
}
