package messages

import (
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db/models"
)

func TestBuiltinTemplatesCoverEveryEventType(t *testing.T) {
	if got := slices.Sorted(maps.Keys(builtin.Events)); !slices.Equal(got, EventTypes()) {
		t.Fatalf("built-in templates %v, event types %v", got, EventTypes())
	}
	for kind, e := range builtin.Events {
		tmpl := e.Templates[builtinLanguage]
		if tmpl.Subject == "" || tmpl.Text == "" || tmpl.HTML == "" {
			t.Errorf("%s: subject, text and html are each required", kind)
		}
	}
	if l := builtin.Layouts[builtinLanguage]; l.HTML == "" || l.Text == "" {
		t.Fatal("the built-in layout has a text and an html part")
	}
}

func TestEveryBuiltinRendersItsSample(t *testing.T) {
	for _, kind := range EventTypes() {
		got, err := Render(billing.MessageSettings{}, "", Sample(kind))
		if err != nil {
			t.Fatalf("%s: %v", kind, err)
		}
		if got.Subject == "" || !strings.HasPrefix(got.Text, "Hi Ada Lovelace,\n\n") || !strings.Contains(got.HTML, "Hi Ada Lovelace,") {
			t.Errorf("%s: subject %q, text %q", kind, got.Subject, got.Text)
		}
		for _, part := range []string{got.Subject, got.Text, got.HTML} {
			if strings.Contains(part, "<no value>") || strings.Contains(part, "{{") {
				t.Errorf("%s: unrendered field in %q", kind, part)
			}
		}
		if !strings.Contains(got.Text, "Example Store") || !strings.Contains(got.HTML, `lang="en"`) {
			t.Errorf("%s: no merchant footer or language:\n%s", kind, got.Text)
		}
	}
}

func TestEveryBuiltinRendersWithoutAProfile(t *testing.T) {
	for _, kind := range EventTypes() {
		data := Sample(kind)
		data.Customer, data.Merchant, data.Links = billing.MessageCustomer{}, billing.MessageMerchant{}, billing.MessageLinks{}
		got, err := Render(billing.MessageSettings{}, "", data)
		if err != nil {
			t.Fatalf("%s: %v", kind, err)
		}
		if !strings.HasPrefix(got.Text, "Hello,\n\n") || strings.Contains(got.Subject, " to .") || strings.HasSuffix(got.Subject, " ") {
			t.Errorf("%s: subject %q, text %q", kind, got.Subject, got.Text)
		}
	}
}

// alpha renders kind's built-in message for Ada from Alpha.
func alpha(t *testing.T, kind models.NotificationEventType, d billing.NotificationData) billing.MessageContent {
	t.Helper()
	got, err := Render(billing.MessageSettings{}, "", messageData(fact{
		EventType: string(kind), Data: d, OccurredAt: time.Date(2026, 11, 23, 15, 4, 0, 0, time.UTC), CustomerID: uuid.New(), Customer: "Ada",
		Profile: models.MerchantProfileConfiguration{DisplayName: "Alpha", SupportURL: "https://alpha.example/help", BillingURL: "https://alpha.example/billing", SubscribeURL: "https://alpha.example/plans"},
	}))
	if err != nil {
		t.Fatalf("%s: %v", kind, err)
	}
	return got
}

func requireText(t *testing.T, got billing.MessageContent, subject string, text ...string) {
	t.Helper()
	if got.Subject != subject {
		t.Errorf("subject %q, want %q", got.Subject, subject)
	}
	for _, want := range text {
		if !strings.Contains(got.Text, want) {
			t.Errorf("text lacks %q:\n%s", want, got.Text)
		}
	}
}

func amount(v int64) *int64 { return &v }

func TestReceipts(t *testing.T) {
	purchase := alpha(t, models.NotificationOneOffPurchaseCompleted, billing.NotificationData{Amount: amount(12_500_000), Currency: "USD", ProductName: "Guide", OrderNumber: 1001})
	requireText(t, purchase, "Your receipt from Alpha", "Purchased: Guide\nAmount paid: 12.50 USD\nOrder: 1001\nDate: November 23, 2026", "Questions? https://alpha.example/help\nAlpha\n")
	jpy := alpha(t, models.NotificationOneOffPurchaseCompleted, billing.NotificationData{Amount: amount(5_000_000), Currency: "JPY", ProductName: "Guide"})
	requireText(t, jpy, "Your receipt from Alpha", "Amount paid: 500 JPY")
	refund := alpha(t, models.NotificationRefundCompleted, billing.NotificationData{Amount: amount(4_000_000), Currency: "USD", ProductName: "Guide", OrderNumber: 1001})
	requireText(t, refund, "Your refund from Alpha", "We refunded 4.00 USD for Guide.", "Order: 1001\nRefunded: 4.00 USD\nDate: November 23, 2026")
}

func TestCancellationsSayTheirCause(t *testing.T) {
	ends := time.Date(2026, 12, 23, 0, 0, 0, 0, time.UTC)
	customer := alpha(t, models.NotificationSubscriptionCanceled, billing.NotificationData{Reason: "user_cancel", EndedAt: &ends, ProductName: "Pro"})
	requireText(t, customer, "You canceled your subscription to Alpha",
		"This confirms that you canceled your subscription to Pro.", "You keep your access until December 23, 2026. You won't be charged again.",
		"You can resume it before it ends: https://alpha.example/billing")
	staff := alpha(t, models.NotificationSubscriptionCanceled, billing.NotificationData{Reason: "admin", EndedAt: &ends, ProductName: "Pro"})
	requireText(t, staff, "Your subscription to Alpha was canceled",
		"Your subscription to Pro was canceled by Alpha.", "If you didn't expect this, please contact us: https://alpha.example/help")
	other := alpha(t, models.NotificationSubscriptionCanceled, billing.NotificationData{Reason: "rail_cancel"})
	requireText(t, other, "Your subscription to Alpha is canceled", "Your subscription is canceled.", "You won't be charged again.", "You can subscribe again any time: https://alpha.example/plans")
	failed := alpha(t, models.NotificationOrderCollectionStopped, billing.NotificationData{Reason: "schedule_exhausted", AmountDue: amount(30_000_000), Currency: "USD"})
	requireText(t, failed, "We stopped charging your card for Alpha",
		"Your payments failed several times, so we have stopped trying to collect the 30.00 USD you owe.", "Pay it or add another payment method: https://alpha.example/billing")
	dead := alpha(t, models.NotificationOrderCollectionStopped, billing.NotificationData{Reason: "non_recoverable", AmountDue: amount(30_000_000), Currency: "USD"})
	requireText(t, dead, "We stopped charging your card for Alpha", "Your bank told us your card can no longer be charged")
	if customer.Text == staff.Text || staff.Text == failed.Text || customer.Text == failed.Text {
		t.Error("each cause has its own wording")
	}
}

func TestPriceChangeNotice(t *testing.T) {
	from := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	got := alpha(t, models.NotificationSubscriptionRepriceScheduled, billing.NotificationData{OldAmount: amount(10_000_000), NewAmount: amount(12_000_000), Currency: "USD", EffectiveAt: &from})
	requireText(t, got, "The price of your subscription to Alpha is changing",
		"is changing from 10.00 USD to 12.00 USD, starting January 1, 2027.", "you can cancel before then: https://alpha.example/billing")
	if !strings.Contains(got.HTML, "<strong>10.00 USD</strong> to <strong>12.00 USD</strong>") || !strings.Contains(got.HTML, `href="https://alpha.example/billing"`) {
		t.Errorf("html:\n%s", got.HTML)
	}
}

func TestHTMLEscapesWhatItIsGiven(t *testing.T) {
	data := Sample(string(models.NotificationOneOffPurchaseCompleted))
	data.Customer.Name, data.Event.ProductName, data.Links.ManageBilling = "<b>Ada</b>", `"><script>x()</script>`, "javascript:alert(1)"
	got, err := Render(billing.MessageSettings{}, "", data)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got.HTML, "<script>") || strings.Contains(got.HTML, "<b>Ada") || strings.Contains(got.HTML, "javascript:") {
		t.Fatalf("html:\n%s", got.HTML)
	}
	if !strings.Contains(got.Text, "Hi <b>Ada</b>,") {
		t.Fatalf("text is plain:\n%s", got.Text)
	}
}

func TestMerchantTemplatesComeFirstByLanguage(t *testing.T) {
	receipt := string(models.NotificationOneOffPurchaseCompleted)
	s := billing.MessageSettings{
		DefaultLanguage: "fr",
		Events: map[string]billing.MessageEventSettings{
			receipt: {Templates: map[string]billing.MessageTemplate{"fr": {Subject: "Votre reçu {{.Merchant.Name}}", Text: "Merci {{upper .Customer.Name}} ({{.Event.Amount}})."}}},
		},
		Layouts: map[string]billing.MessageLayout{"fr": {Text: "Bonjour,\n{{template \"body\" .}}\n-- {{.Merchant.Name}}"}},
	}
	got, err := Render(s, "", Sample(receipt))
	if err != nil {
		t.Fatal(err)
	}
	if got.Subject != "Votre reçu Example Store" || got.Text != "Bonjour,\nMerci ADA LOVELACE (12.50 USD).\n-- Example Store\n" || got.HTML != "" {
		t.Fatalf("%+v", got)
	}
	// An event type with no French template is the built-in English one, in
	// the merchant's French layout; its HTML layout is the built-in one.
	refund := Sample(string(models.NotificationRefundCompleted))
	got, err = Render(s, "", refund)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(got.Text, "Bonjour,\nWe refunded 12.50 USD") || !strings.Contains(got.HTML, `lang="en"`) {
		t.Fatalf("%+v", got)
	}
}

func TestTidy(t *testing.T) {
	if got := tidy("a  \r\n\n\n\nb\t\n\n"); got != "a\n\nb\n" {
		t.Fatalf("%q", got)
	}
	if got := tidy(" \n\n "); got != "" {
		t.Fatalf("%q", got)
	}
}
