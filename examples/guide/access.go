package guide

import (
	"context"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
)

// Comp gives a customer a product free for a week.
func Comp(ctx context.Context, shop *openrails.Client, customer billing.CustomerID, product billing.ProductID, key string) error {
	// guide:grant-access
	_, err := shop.CreateProductAccess(ctx, billing.CreateProductAccessBatchParams{
		IdempotencyKey: key,
		Items: []billing.CreateProductAccessParams{{
			CustomerID: customer,
			ProductID:  product,
			Hours:      new(7 * 24), // after its latest live window; nil with no EndsAt is indefinite
			Reason:     billing.GrantReasonComp,
		}},
	})
	return err
	// guide:end
}
