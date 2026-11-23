package guide

import (
	"context"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
)

// RaiseLimits gives one customer more room than its plan: a credit line and
// a larger weekly limit.
func RaiseLimits(ctx context.Context, shop *openrails.Client, customer billing.CustomerID) error {
	// guide:customers
	c, err := shop.GetCustomer(ctx, customer) // settings, balances, contact; 404 if it never billed
	if err != nil {
		return err
	}
	_, err = shop.UpdateCustomer(ctx, c.ID, billing.UpdateCustomerParams{
		CreditLimits: []billing.CreditLimit{{Currency: "USD", Amount: 75_000_000}}, // $75 of arrears
		Limits:       []billing.LimitOverride{{Limit: "weekly", Capacity: 8000}},   // replaces the plan's
	})
	return err
	// guide:end
}
