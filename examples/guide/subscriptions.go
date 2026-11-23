package guide

import (
	"context"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
)

// CancelAll stops renewal of a customer's active subscriptions; each keeps
// access to the end of its paid period.
func CancelAll(ctx context.Context, shop *openrails.Client, customer billing.CustomerID) error {
	// guide:subscriptions
	page, err := shop.ListSubscriptions(ctx, billing.SubscriptionListParams{
		CustomerID: customer, Status: billing.SubscriptionActive,
	})
	if err != nil {
		return err
	}
	for _, sub := range page.Items {
		_, err := shop.CancelSubscription(ctx, sub.ID, billing.CancelSubscriptionParams{
			Reason: "customer request", // RevokeAccess: true would also end access now
		})
		if err != nil {
			return err
		}
	}
	return nil
	// guide:end
}
