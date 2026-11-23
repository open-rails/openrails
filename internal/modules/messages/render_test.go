package messages

import (
	"strings"
	"testing"
	"time"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db/models"
)

func TestPlainRendersEveryTypeSentByDefault(t *testing.T) {
	for kind, on := range defaults {
		_, rendered := plain[kind]
		if on != rendered {
			t.Errorf("%s: sent by default %v, rendered %v", kind, on, rendered)
		}
	}
}

func TestPlain(t *testing.T) {
	at := time.Date(2026, 11, 23, 15, 4, 0, 0, time.UTC)
	later := at.AddDate(0, 1, 0)
	amount := func(v int64) *int64 { return &v }
	merchant := Merchant{Name: "Alpha", SupportURL: "https://alpha.example/help", BillingURL: "https://alpha.example/billing"}
	cases := []struct {
		kind    models.NotificationEventType
		data    billing.NotificationData
		subject string
		text    []string
	}{
		{models.NotificationOneOffPurchaseCompleted, billing.NotificationData{Amount: amount(12_500_000), Currency: "USD", ProductName: "Guide", OrderNumber: 7},
			"Your receipt from Alpha", []string{"Hi Ada,", "Purchased: Guide\nAmount paid: 12.50 USD\nOrder: 7\nDate: November 23, 2026", "Questions? https://alpha.example/help", "\n\nAlpha\n"}},
		{models.NotificationPremiumStarted, billing.NotificationData{},
			"Your subscription to Alpha has started", []string{"Manage your subscription: https://alpha.example/billing"}},
		{models.NotificationPaymentMethodFailed, billing.NotificationData{Amount: amount(5_000_000), Currency: "JPY", NextAttemptAt: &later},
			"Your payment to Alpha did not go through", []string{"payment of 500 JPY. We will try again on December 23, 2026.", "at https://alpha.example/billing"}},
		{models.NotificationPaymentAuthenticationRequired, billing.NotificationData{Amount: amount(9_990_000), Currency: "EUR", ProductName: "Pro"},
			"Confirm your payment to Alpha", []string{"payment of 9.99 EUR for Pro before it goes through."}},
		{models.NotificationPaymentMethodUpdateRequired, billing.NotificationData{},
			"Update your payment method for Alpha", []string{"will not work for future payments"}},
		{models.NotificationSubscriptionCanceled, billing.NotificationData{Reason: "user_cancel", EndedAt: &later},
			"Your subscription to Alpha is canceled", []string{"You canceled your subscription. You keep access until December 23, 2026."}},
		{models.NotificationSubscriptionCanceled, billing.NotificationData{Reason: "admin"},
			"Your subscription to Alpha is canceled", []string{"Your subscription was canceled by Alpha. No further payments will be taken."}},
		{models.NotificationSubscriptionCanceled, billing.NotificationData{Reason: "expired"},
			"Your subscription to Alpha is canceled", []string{"Your subscription was canceled. No further payments will be taken."}},
		{models.NotificationOrderCollectionStopped, billing.NotificationData{AmountDue: amount(3_000_000), Currency: "USD"},
			"We stopped collecting your payment to Alpha", []string{"of 3.00 USD and have stopped trying"}},
		{models.NotificationSubscriptionRepriceScheduled, billing.NotificationData{OldAmount: amount(10_000_000), NewAmount: amount(12_000_000), Currency: "USD", EffectiveAt: &later},
			"Your subscription to Alpha is changing price", []string{"changing on December 23, 2026, from 10.00 USD to 12.00 USD."}},
	}
	for _, c := range cases {
		got, ok, err := Plain{}.Render(t.Context(), Message{EventType: string(c.kind), Data: c.data, OccurredAt: at, Customer: "Ada", Merchant: merchant})
		if err != nil || !ok {
			t.Fatalf("%s: ok %v, %v", c.kind, ok, err)
		}
		if got.Subject != c.subject {
			t.Errorf("%s: subject %q", c.kind, got.Subject)
		}
		for _, want := range c.text {
			if !strings.Contains(got.Text, want) {
				t.Errorf("%s: text lacks %q:\n%s", c.kind, want, got.Text)
			}
		}
	}
}

func TestPlainWithoutAProfile(t *testing.T) {
	got, ok, err := Plain{}.Render(t.Context(), Message{EventType: string(models.NotificationPaymentMethodUpdateRequired)})
	if err != nil || !ok {
		t.Fatalf("ok %v, %v", ok, err)
	}
	want := "Hello,\n\nYour bank declined the card on file, and it will not work for future payments.\n\nUpdate your payment method in your account to keep your access.\n"
	if got.Subject != "Update your payment method" || got.Text != want {
		t.Errorf("subject %q, text %q", got.Subject, got.Text)
	}
}

func TestPlainRendersNoOtherType(t *testing.T) {
	if _, ok, err := (Plain{}).Render(t.Context(), Message{EventType: string(models.NotificationPremiumRenewed)}); ok || err != nil {
		t.Errorf("ok %v, %v", ok, err)
	}
}
