package guide

import (
	"context"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
)

// SellPost puts one post on sale from code, creating its product with the
// price.
func SellPost(ctx context.Context, shop *openrails.Client, slug, title string) (*billing.Price, error) {
	// guide:create-price
	return shop.CreatePrice(ctx, billing.CreatePriceParams{
		ProductData: &billing.CreatePriceProduct{Key: "post-" + slug, DisplayName: title},
		Key:         "usd",     // the price key; the id becomes post-<slug>:usd:v1
		UnitAmount:  4_990_000, // $4.99 in USD micros
		Currency:    "USD",
	})
	// guide:end
}
