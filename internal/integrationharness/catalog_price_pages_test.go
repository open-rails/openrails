//go:build integration

package integrationharness

import (
	"context"
	"fmt"

	"github.com/open-rails/openrails"
	billingservice "github.com/open-rails/openrails/internal/service"
)

func catalogPrices(ctx context.Context, client *openrails.Client, id openrails.ProductID, activeOnly bool) ([]billingservice.CatalogPrice, error) {
	filter := openrails.PriceFilter{ProductID: id, PageOptions: openrails.PageOptions{Limit: 100}}
	if activeOnly {
		archived := false
		filter.Archived = &archived
	}
	var out []billingservice.CatalogPrice
	for {
		page, err := client.ListPrices(ctx, filter)
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
