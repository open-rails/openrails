package catalog

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"

	billingservice "github.com/open-rails/openrails/pkg/service"
)

// Status values mirror the OpenRails CatalogStatus enum (issue #210). We keep
// them as plain strings inside the catalog package so it is decoupled from the
// internal models package; the in-process adapter casts to models.CatalogStatus
// at the boundary.
const (
	StatusDraft    = "draft"
	StatusActive   = "active"
	StatusArchived = "archived"
)

func normalizeStatus(s string) (string, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	switch s {
	case "":
		return "", nil // caller-defined default (active for products/prices)
	case StatusDraft, StatusActive, StatusArchived:
		return s, nil
	default:
		return "", fmt.Errorf("invalid status %q (want draft|active|archived)", s)
	}
}

// Applier is the narrow facade surface the plan/apply pipeline drives. It
// covers exactly the methods this package calls, so it can be satisfied by a
// fake in tests, by an in-process *service.Service adapter, or by a remote HTTP
// client — all decoupled from *service.Service.
type Applier interface {
	GetProductBySlug(ctx context.Context, slug string) (*billingservice.CatalogProduct, error)
	ListProducts(ctx context.Context, opts billingservice.ListProductsOptions) ([]billingservice.CatalogProduct, int64, error)
	CreateProduct(ctx context.Context, req billingservice.CreateProductRequest) (*billingservice.CatalogProduct, error)
	UpdateProduct(ctx context.Context, id uuid.UUID, req billingservice.UpdateProductRequest) (*billingservice.CatalogProduct, error)
	DeactivateProduct(ctx context.Context, id uuid.UUID) (*billingservice.CatalogProduct, error)

	ListPricesByProduct(ctx context.Context, productID uuid.UUID, activeOnly bool) ([]billingservice.CatalogPrice, error)
	CreatePrice(ctx context.Context, req billingservice.CreatePriceRequest) (*billingservice.CatalogPrice, error)
	ActivatePrice(ctx context.Context, id uuid.UUID) (*billingservice.CatalogPrice, error)
	DeactivatePrice(ctx context.Context, id uuid.UUID) (*billingservice.CatalogPrice, error)
}
