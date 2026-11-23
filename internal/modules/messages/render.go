package messages

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
)

// Message is one fact a customer is told about, and who tells them.
type Message struct {
	EventType  string
	Data       billing.NotificationData
	OccurredAt time.Time
	// Customer is the name the message greets; empty greets no one by name.
	Customer string
	Merchant Merchant
}

// Merchant is the sender as a message names it; an empty field is left out.
type Merchant struct {
	Name         string
	SupportURL   string
	BillingURL   string
	SubscribeURL string
}

// Content is a rendered message: a subject and a text or HTML body.
type Content struct {
	Subject string
	Text    string
	HTML    string
}

// Renderer renders a message; ok is false for an event type it has nothing
// for.
type Renderer interface {
	Render(ctx context.Context, m Message) (c Content, ok bool, err error)
}

// Plain is the built-in renderer: a subject and plain text for each event
// type that sends by default.
type Plain struct{}

func (Plain) Render(_ context.Context, m Message) (Content, bool, error) {
	render, ok := plain[models.NotificationEventType(m.EventType)]
	if !ok {
		return Content{}, false, nil
	}
	subject, body := render(m)
	return Content{Subject: subject, Text: letter(m, body...)}, true, nil
}

var plain = map[models.NotificationEventType]func(Message) (string, []string){
	models.NotificationOneOffPurchaseCompleted: func(m Message) (string, []string) {
		lines := []string{}
		if d := m.Data; d.ProductName != "" {
			lines = append(lines, "Purchased: "+d.ProductName)
		}
		if a := amount(m.Data.Amount, m.Data.Currency); a != "" {
			lines = append(lines, "Amount paid: "+a)
		}
		if m.Data.OrderNumber > 0 {
			lines = append(lines, "Order: "+strconv.FormatInt(m.Data.OrderNumber, 10))
		}
		lines = append(lines, "Date: "+day(m.OccurredAt))
		return "Your receipt" + from(m), []string{"Thank you for your purchase.", strings.Join(lines, "\n")}
	},
	models.NotificationPremiumStarted: func(m Message) (string, []string) {
		return "Your subscription" + to(m) + " has started", []string{"Thank you for subscribing. Your subscription is active.", manage(m)}
	},
	models.NotificationPaymentMethodFailed: func(m Message) (string, []string) {
		first := "We could not take your payment"
		if a := amount(m.Data.Amount, m.Data.Currency); a != "" {
			first += " of " + a
		}
		first += "."
		if m.Data.NextAttemptAt != nil {
			first += " We will try again on " + day(*m.Data.NextAttemptAt) + "."
		}
		return "Your payment" + to(m) + " did not go through", []string{first, update(m)}
	},
	models.NotificationPaymentAuthenticationRequired: func(m Message) (string, []string) {
		first := "Your bank asks you to confirm a payment"
		if a := amount(m.Data.Amount, m.Data.Currency); a != "" {
			first = "Your bank asks you to confirm your payment of " + a
		}
		if m.Data.ProductName != "" {
			first += " for " + m.Data.ProductName
		}
		first += " before it goes through."
		return "Confirm your payment" + to(m), []string{first, "Confirm it from your account" + at(m.Merchant.BillingURL) + "."}
	},
	models.NotificationPaymentMethodUpdateRequired: func(m Message) (string, []string) {
		return "Update your payment method" + forMerchant(m), []string{
			"Your bank declined the card on file, and it will not work for future payments.", update(m)}
	},
	models.NotificationSubscriptionCanceled: func(m Message) (string, []string) {
		first := "Your subscription was canceled."
		switch m.Data.Reason {
		case "user_cancel":
			first = "You canceled your subscription."
		case "admin":
			first = "Your subscription was canceled by " + name(m, "us") + "."
		}
		lines := []string{first}
		if m.Data.EndedAt != nil {
			lines = append(lines, "You keep access until "+day(*m.Data.EndedAt)+".")
		}
		lines = append(lines, "No further payments will be taken.")
		return "Your subscription" + to(m) + " is canceled", []string{strings.Join(lines, " "), manage(m)}
	},
	models.NotificationOrderCollectionStopped: func(m Message) (string, []string) {
		first := "We could not collect your payment"
		if a := amount(m.Data.AmountDue, m.Data.Currency); a != "" {
			first += " of " + a
		}
		first += " and have stopped trying. The amount is still owed."
		return "We stopped collecting your payment" + to(m), []string{first, "Pay it or update your payment method" + at(m.Merchant.BillingURL) + "."}
	},
	models.NotificationSubscriptionRepriceScheduled: func(m Message) (string, []string) {
		first := "The price of your subscription is changing"
		if m.Data.EffectiveAt != nil {
			first += " on " + day(*m.Data.EffectiveAt)
		}
		old, next := amount(m.Data.OldAmount, m.Data.Currency), amount(m.Data.NewAmount, m.Data.Currency)
		if old != "" && next != "" {
			first += ", from " + old + " to " + next
		}
		first += "."
		return "Your subscription" + to(m) + " is changing price", []string{first, "To cancel before then, visit your account" + at(m.Merchant.BillingURL) + "."}
	},
}

// letter frames a body: the greeting, its paragraphs, the merchant's name
// and where to ask.
func letter(m Message, paragraphs ...string) string {
	greeting := "Hello,"
	if m.Customer != "" {
		greeting = "Hi " + m.Customer + ","
	}
	parts := []string{greeting}
	for _, p := range paragraphs {
		if p != "" {
			parts = append(parts, p)
		}
	}
	if m.Merchant.SupportURL != "" {
		parts = append(parts, "Questions? "+m.Merchant.SupportURL)
	}
	if m.Merchant.Name != "" {
		parts = append(parts, m.Merchant.Name)
	}
	return strings.Join(parts, "\n\n") + "\n"
}

func manage(m Message) string {
	if m.Merchant.BillingURL == "" {
		return ""
	}
	return "Manage your subscription: " + m.Merchant.BillingURL
}

func update(m Message) string {
	return "Update your payment method" + at(m.Merchant.BillingURL) + " to keep your access."
}

func at(url string) string {
	if url == "" {
		return " in your account"
	}
	return " at " + url
}

func name(m Message, otherwise string) string {
	if m.Merchant.Name == "" {
		return otherwise
	}
	return m.Merchant.Name
}

func from(m Message) string        { return phrase(" from ", m) }
func to(m Message) string          { return phrase(" to ", m) }
func forMerchant(m Message) string { return phrase(" for ", m) }

func phrase(prefix string, m Message) string {
	if m.Merchant.Name == "" {
		return ""
	}
	return prefix + m.Merchant.Name
}

func amount(v *int64, currency string) string {
	if v == nil || currency == "" {
		return ""
	}
	return moneyutil.FormatAmount(*v, currency)
}

func day(t time.Time) string { return t.UTC().Format("January 2, 2006") }
