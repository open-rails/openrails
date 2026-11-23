package guide

import (
	"context"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
)

// Checkout creates the customer's order with a pay URL and returns where to
// send them.
func Checkout(ctx context.Context, shop *openrails.Client, customer billing.CustomerID, price billing.PriceID, key string) (string, error) {
	// guide:checkout
	// key is made when the buyer decides, stored, and resent on every retry.
	order, err := shop.CreateOrder(ctx, key, billing.CreateOrderParams{
		CustomerID: customer,
		Lines:      []billing.OrderLineParams{{PriceID: price}},
		PayURL: &billing.OrderPayURLParams{
			SuccessURL: "https://shop.example/thanks?order={ORDER_ID}",
			CancelURL:  new("https://shop.example/cart"),
		},
	})
	if err != nil {
		return "", err
	}
	return *order.PayURL, nil // redirect the buyer there
	// guide:end
}
