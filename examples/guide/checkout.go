package guide

import (
	"context"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
)

// Checkout creates the customer's invoice with a pay URL and returns where to
// send them.
func Checkout(ctx context.Context, shop *openrails.Client, customer billing.CustomerID, price billing.PriceID, key string) (string, error) {
	// guide:checkout
	// key is made when the buyer decides, stored, and resent on every retry.
	invoice, err := shop.CreateInvoice(ctx, key, billing.CreateInvoiceParams{
		CustomerID: customer,
		Lines:      []billing.InvoiceLineParams{{PriceID: price}},
		PayURL: &billing.InvoicePayURLParams{
			SuccessURL: "https://shop.example/thanks?invoice={INVOICE_ID}",
			CancelURL:  new("https://shop.example/cart"),
		},
	})
	if err != nil {
		return "", err
	}
	return *invoice.PayURL, nil // redirect the buyer there
	// guide:end
}
