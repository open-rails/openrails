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
		out.Payments = append(out.Payments, PaymentToAPI(p, nil))
	}
	return out
}
