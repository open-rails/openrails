package handlers

import (
	"time"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
)

// subscriptionView projects a merchant-side subscription read onto the shared
// Client DTO, with its recovery payment history.
func subscriptionView(in *subscriptions.AdminSubscriptionResponse, now time.Time) openrails.Subscription {
	out := subscriptions.SubscriptionView(in.Subscription, in.Price, now)
	for _, p := range in.Payments {
		out.Payments = append(out.Payments, openrails.SubscriptionPayment{ID: openrails.PaymentID(p.ID), Status: p.Status, Amount: p.Amount, Currency: p.Currency, Rail: string(p.Rail), TransactionID: p.TransactionID, PurchasedAt: p.PurchasedAt})
	}
	return out
}
