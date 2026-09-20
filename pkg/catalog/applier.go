package catalog

import (
	"context"

	"github.com/open-rails/openrails"
	billingservice "github.com/open-rails/openrails/internal/service"
)

// Applier is the narrow facade surface the plan/apply pipeline drives. It
// covers exactly the methods this package calls, so it can be satisfied by a
// fake in tests, by an in-process *service.Service adapter, or by a remote HTTP
// client — all decoupled from *service.Service.
type Applier interface {
	// GetProductByKey returns openrails.ErrNotFound (or a wrapping typed refusal)
	// only when the product is absent. Other failures stop planning.
	GetProductByKey(ctx context.Context, key string) (*billingservice.CatalogProduct, error)
	ListProducts(ctx context.Context, opts billingservice.ListProductsOptions) (billingservice.CatalogPage[billingservice.CatalogProduct], error)
	CreateProduct(ctx context.Context, req billingservice.CreateProductRequest) (*billingservice.CatalogProduct, error)
	UpdateProduct(ctx context.Context, id openrails.ProductID, req billingservice.UpdateProductRequest) (*billingservice.CatalogProduct, error)
	DeactivateProduct(ctx context.Context, id openrails.ProductID) (*billingservice.CatalogProduct, error)

	ListPricesByProduct(ctx context.Context, productID openrails.ProductID, activeOnly bool) ([]billingservice.CatalogPrice, error)
	CreatePrice(ctx context.Context, req billingservice.CreatePriceRequest) (*billingservice.CatalogPrice, error)
	ActivatePrice(ctx context.Context, id openrails.PriceID) (*billingservice.CatalogPrice, error)
	DeactivatePrice(ctx context.Context, id openrails.PriceID) (*billingservice.CatalogPrice, error)
	// SetPriceKey relabels a price's #774 key in place (a plain rename — never
	// used to create/activate/archive a row).
	// UpdatePrice rotates a matched price's PSP links (psp_links merge) —
	// the only mutation a substance-unchanged price can need from a manifest.
	UpdatePrice(ctx context.Context, id openrails.PriceID, req billingservice.UpdatePriceRequest) (*billingservice.CatalogPrice, error)
	SetPriceKey(ctx context.Context, id openrails.PriceID, key string) (*billingservice.CatalogPrice, error)
}
