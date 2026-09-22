//go:build integration

package integrationharness

import (
	"context"
	"fmt"

	"github.com/open-rails/openrails"
)

func catalogPrices(ctx context.Context, client *openrails.Client, id openrails.ProductID, activeOnly bool) ([]openrails.Price, error) {
	filter := openrails.PriceListParams{ProductID: (id).String(), PageOptions: openrails.PageOptions{Limit: 100}}
	if activeOnly {
		archived := false
		filter.Archived = &archived
	}
	var out []openrails.Price
	for {
		page, err := client.Prices.List(ctx, &filter)
		if err != nil {
			return nil, err
		}
		out = append(out, page.Items...)
		if int64(len(out)) >= page.Total {
			return out, nil
		}
		if len(page.Items) == 0 {
			return nil, fmt.Errorf("catalog price page made no progress")
		}
		filter.Offset += len(page.Items)
	}
}
