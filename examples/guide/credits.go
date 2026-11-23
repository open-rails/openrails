package guide

import (
	"context"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
)

// Goodwill funds a customer's balance once per support ticket.
func Goodwill(ctx context.Context, shop *openrails.Client, customer billing.CustomerID, ticket string) error {
	// guide:credit-grant
	_, err := shop.CreateCreditGrants(ctx, []billing.CreateCreditGrantParams{{
		CustomerID: customer,
		Currency:   "USD",
		Amount:     5_000_000, // $5
		Source:     "support", // with SourceID, the grant's identity: a retry grants nothing again
		SourceID:   ticket,
	}})
	return err
	// guide:end
}
