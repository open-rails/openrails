//go:build integration

package integrationharness

import (
	"context"
	"fmt"

	"github.com/open-rails/openrails"
	billingservice "github.com/open-rails/openrails/internal/service"
)

// The planner's older Applier shape differs only in pagination and lifecycle
// conveniences. All requests still use the actual shared Client; this owns no
// HTTP paths, JSON encoding, authentication or in-memory catalog state.
type catalogClientApplier struct{ *openrails.Client }

func (a catalogClientApplier) ListProducts(ctx context.Context, opts billingservice.ListProductsOptions) (billingservice.CatalogPage[billingservice.CatalogProduct], error) {
	page, err := a.Client.ListProducts(ctx, openrails.ProductFilter{Archived: opts.Archived, TierGroup: opts.TierGroup, PageOptions: openrails.PageOptions{Limit: opts.Limit, Offset: opts.Offset}})
	if err != nil {
		return billingservice.CatalogPage[billingservice.CatalogProduct]{}, err
	}
	return billingservice.CatalogPage[billingservice.CatalogProduct]{Items: page.Items, Total: page.Total, Limit: page.Limit, Offset: page.Offset}, nil
}
func (a catalogClientApplier) ListPricesByProduct(ctx context.Context, id openrails.ProductID, activeOnly bool) ([]billingservice.CatalogPrice, error) {
	filter := openrails.PriceFilter{ProductID: id, PageOptions: openrails.PageOptions{Limit: 100}}
	if activeOnly {
		archived := false
		filter.Archived = &archived
	}
	var out []billingservice.CatalogPrice
	for {
		page, err := a.Client.ListPrices(ctx, filter)
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
func (a catalogClientApplier) DeactivateProduct(ctx context.Context, id openrails.ProductID) (*billingservice.CatalogProduct, error) {
	archived := true
	return a.Client.UpdateProduct(ctx, id, openrails.UpdateProductRequest{Archived: &archived})
}
func (a catalogClientApplier) ActivatePrice(ctx context.Context, id openrails.PriceID) (*billingservice.CatalogPrice, error) {
	archived := false
	return a.Client.UpdatePrice(ctx, id, openrails.UpdatePriceRequest{Archived: &archived})
}
func (a catalogClientApplier) DeactivatePrice(ctx context.Context, id openrails.PriceID) (*billingservice.CatalogPrice, error) {
	archived := true
	return a.Client.UpdatePrice(ctx, id, openrails.UpdatePriceRequest{Archived: &archived})
}
