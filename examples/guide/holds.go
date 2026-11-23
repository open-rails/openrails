package guide

import (
	"context"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
)

// ClearReviewed clears every open hold after the incident was reviewed. The
// handle must act for a person other than the one held.
func ClearReviewed(ctx context.Context, reviewer *openrails.Client) error {
	// guide:destructive-holds
	page, err := reviewer.ListDestructiveHolds(ctx, billing.DestructiveHoldListParams{})
	if err != nil {
		return err
	}
	for _, hold := range page.Items {
		_, err := reviewer.ClearDestructiveHold(ctx, hold.ID, billing.ClearDestructiveHoldParams{
			Reason: "credential revoked; pending effects reviewed",
		})
		if err != nil {
			return err
		}
	}
	return nil
	// guide:end
}
