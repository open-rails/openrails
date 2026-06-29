package catalog

import (
	"context"

	"github.com/google/uuid"

	billingservice "github.com/open-rails/openrails/pkg/service"
)

// Status values mirror the OpenRails CatalogStatus enum at the service boundary.
const (
	StatusActive   = "active"
	StatusArchived = "archived"
)

func statusFromActive(active *bool) string {
	if active != nil && !*active {
		return StatusArchived
	}
	return StatusActive
}

// Applier is the narrow facade surface the plan/apply pipeline drives. It
// covers exactly the methods this package calls, so it can be satisfied by a
// fake in tests, by an in-process *service.Service adapter, or by a remote HTTP
// client — all decoupled from *service.Service.
type Applier interface {
	GetProductByKey(ctx context.Context, key string) (*billingservice.CatalogProduct, error)
	ListProducts(ctx context.Context, opts billingservice.ListProductsOptions) ([]billingservice.CatalogProduct, int64, error)
	CreateProduct(ctx context.Context, req billingservice.CreateProductRequest) (*billingservice.CatalogProduct, error)
	UpdateProduct(ctx context.Context, id uuid.UUID, req billingservice.UpdateProductRequest) (*billingservice.CatalogProduct, error)
	DeactivateProduct(ctx context.Context, id uuid.UUID) (*billingservice.CatalogProduct, error)

	ListPricesByProduct(ctx context.Context, productID uuid.UUID, activeOnly bool) ([]billingservice.CatalogPrice, error)
	CreatePrice(ctx context.Context, req billingservice.CreatePriceRequest) (*billingservice.CatalogPrice, error)
	ActivatePrice(ctx context.Context, id uuid.UUID) (*billingservice.CatalogPrice, error)
	DeactivatePrice(ctx context.Context, id uuid.UUID) (*billingservice.CatalogPrice, error)
}
