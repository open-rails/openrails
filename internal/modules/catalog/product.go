package catalog

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/db/repo"
)

type ProductService struct {
	repo *repo.ProductRepo
}

func NewProductService(db *db.DB) *ProductService {
	return &ProductService{repo: repo.NewProductRepo(db)}
}

func (s *ProductService) Create(ctx context.Context, product *models.Product) error {
	return s.repo.Create(ctx, product)
}

func (s *ProductService) GetByID(ctx context.Context, id uuid.UUID) (*models.Product, error) {
	return s.repo.GetByID(ctx, id)
}

func (s *ProductService) GetActive(ctx context.Context) ([]*models.Product, error) {
	return s.repo.GetActive(ctx)
}

func (s *ProductService) GetAll(ctx context.Context) ([]*models.Product, error) {
	return s.repo.GetAll(ctx)
}

// GetActivePaginated returns active products with pagination
func (s *ProductService) GetActivePaginated(ctx context.Context, limit, offset int) ([]*models.Product, int64, error) {
	return s.repo.GetActivePaginated(ctx, limit, offset)
}

// GetAllPaginated returns all products with pagination
func (s *ProductService) GetAllPaginated(ctx context.Context, limit, offset int) ([]*models.Product, int64, error) {
	return s.repo.GetAllPaginated(ctx, limit, offset)
}

// Update is not supported for arbitrary changes - products should be treated as mostly immutable.
// Use UpdateDisplayName(), UpdateDescription(), or Deactivate() for allowed changes.
func (s *ProductService) Update(ctx context.Context, product *models.Product) error {
	return errors.New("products are mostly immutable; use UpdateDisplayName(), UpdateDescription(), or Deactivate() for allowed changes")
}

// Delete is not supported - products cannot be deleted to preserve historical data integrity.
// Use Deactivate() instead to hide a product from listings.
func (s *ProductService) Delete(ctx context.Context, id uuid.UUID) error {
	return errors.New("products cannot be deleted; use Deactivate() instead to preserve historical data")
}

func (s *ProductService) GetBySlug(ctx context.Context, slug string) (*models.Product, error) {
	return s.repo.GetBySlug(ctx, slug)
}

// Deactivate marks a product as inactive so it won't appear in product listings.
// Existing subscriptions and payments referencing this product's prices are unaffected.
func (s *ProductService) Deactivate(ctx context.Context, id uuid.UUID) error {
	product, err := s.repo.GetByID(ctx, id)
	if err != nil {
		return err
	}
	product.IsActive = false
	return s.repo.Update(ctx, product)
}

// Activate marks a product as active so it appears in product listings.
func (s *ProductService) Activate(ctx context.Context, id uuid.UUID) error {
	product, err := s.repo.GetByID(ctx, id)
	if err != nil {
		return err
	}
	product.IsActive = true
	return s.repo.Update(ctx, product)
}

// UpdateDisplayName updates only the display name (cosmetic, does not affect historical data).
func (s *ProductService) UpdateDisplayName(ctx context.Context, id uuid.UUID, displayName string) error {
	product, err := s.repo.GetByID(ctx, id)
	if err != nil {
		return err
	}
	product.DisplayName = displayName
	return s.repo.Update(ctx, product)
}

// UpdateDescription updates only the description (cosmetic, does not affect historical data).
func (s *ProductService) UpdateDescription(ctx context.Context, id uuid.UUID, description string) error {
	product, err := s.repo.GetByID(ctx, id)
	if err != nil {
		return err
	}
	product.Description = description
	return s.repo.Update(ctx, product)
}

type ProductDefinitionUpdateParams struct {
	DisplayName      *string
	Description      *string
	EntitlementsSpec map[string]*int
	SetEntitlements  bool
	CreditsSpec      models.CreditsSpec
	SetCredits       bool
	TierGroup        *string
	SetTierGroup     bool
	TierRank         *int
	IsActive         *bool
}

// UpdateDefinition updates host-configurable fields on a product (catalog definition surface).
//
// This is intentionally separate from Update(), which forbids arbitrary changes for safety.
func (s *ProductService) UpdateDefinition(ctx context.Context, id uuid.UUID, params ProductDefinitionUpdateParams) (*models.Product, error) {
	product, err := s.repo.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if params.DisplayName != nil {
		product.DisplayName = *params.DisplayName
	}
	if params.Description != nil {
		product.Description = *params.Description
	}
	if params.SetEntitlements {
		product.EntitlementsSpec = params.EntitlementsSpec
	}
	if params.SetTierGroup {
		product.TierGroup = params.TierGroup
	}
	if params.TierRank != nil {
		product.TierRank = *params.TierRank
	}
	if params.IsActive != nil {
		product.IsActive = *params.IsActive
	}
	if params.SetCredits {
		product.CreditsSpec = params.CreditsSpec
	}

	if err := s.repo.Update(ctx, product); err != nil {
		return nil, err
	}
	return product, nil
}
