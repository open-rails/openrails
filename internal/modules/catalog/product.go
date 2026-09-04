package catalog

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
)

type ProductService struct {
	db *db.DB
}

func NewProductService(db *db.DB) *ProductService {
	return &ProductService{db: db}
}

func productPageInt32(v int) int32 {
	if v < 0 {
		return 0
	}
	if v > math.MaxInt32 {
		return math.MaxInt32
	}
	return int32(v)
}

func productTierRankInt32(v int) (int32, error) {
	if v < math.MinInt32 || v > math.MaxInt32 {
		return 0, fmt.Errorf("product tier_rank %d outside int32 range", v)
	}
	return int32(v), nil
}

func productsFromGen(rows []gen.OpenrailsProduct) ([]*models.Product, error) {
	out := make([]*models.Product, 0, len(rows))
	for _, r := range rows {
		p, err := models.ProductFromGen(r)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
}

func (s *ProductService) Create(ctx context.Context, product *models.Product) error {
	entSpec, err := models.ToJSONB(product.EntitlementsSpec)
	if err != nil {
		return err
	}
	credSpec, err := models.ToJSONB(product.CreditsSpec)
	if err != nil {
		return err
	}
	var desc *string
	if product.Description != "" {
		desc = &product.Description
	}
	tierRank32, err := productTierRankInt32(product.TierRank)
	if err != nil {
		return err
	}
	rows, err := s.db.Gen(ctx).CreateProduct(ctx, gen.CreateProductParams{
		ID:               product.ID,
		MerchantID:       product.MerchantID,
		Key:              product.Key,
		DisplayName:      product.DisplayName,
		Description:      desc,
		EntitlementsSpec: entSpec,
		CreditsSpec:      credSpec,
		TierGroup:        product.TierGroup,
		TierRank:         tierRank32,
		Archived:         product.Archived,
		CreatedAt:        product.CreatedAt,
		UpdatedAt:        product.UpdatedAt,
	})
	if err != nil {
		return err
	}
	if rows < 1 {
		return errors.New("no rows affected")
	}
	return nil
}

func (s *ProductService) GetByID(ctx context.Context, id uuid.UUID) (*models.Product, error) {
	row, err := s.db.Gen(ctx).GetProductByID(ctx, id)
	if err != nil {
		return nil, err
	}
	return models.ProductFromGen(row)
}

func (s *ProductService) GetActive(ctx context.Context) ([]*models.Product, error) {
	rows, err := s.db.Gen(ctx).ListActiveProducts(ctx)
	if err != nil {
		return nil, err
	}
	return productsFromGen(rows)
}

func (s *ProductService) GetAll(ctx context.Context) ([]*models.Product, error) {
	rows, err := s.db.Gen(ctx).ListAllProducts(ctx)
	if err != nil {
		return nil, err
	}
	return productsFromGen(rows)
}

type ProductFilter struct {
	ActiveOnly bool
	TierGroup  string
}

func (s *ProductService) GetActivePaginated(ctx context.Context, limit, offset int) ([]*models.Product, int64, error) {
	return s.GetPaginated(ctx, ProductFilter{ActiveOnly: true}, limit, offset)
}

func (s *ProductService) GetAllPaginated(ctx context.Context, limit, offset int) ([]*models.Product, int64, error) {
	return s.GetPaginated(ctx, ProductFilter{}, limit, offset)
}

// GetPaginated applies identical filters to the count and page before slicing.
func (s *ProductService) GetPaginated(ctx context.Context, filter ProductFilter, limit, offset int) ([]*models.Product, int64, error) {
	q := s.db.Gen(ctx)
	tierGroup := strings.TrimSpace(filter.TierGroup)
	total, err := q.CountProductsFiltered(ctx, gen.CountProductsFilteredParams{ActiveOnly: filter.ActiveOnly, TierGroup: tierGroup})
	if err != nil {
		return nil, 0, err
	}
	rows, err := q.ListProductsFiltered(ctx, gen.ListProductsFilteredParams{ActiveOnly: filter.ActiveOnly, TierGroup: tierGroup, PageLimit: productPageInt32(limit), PageOffset: productPageInt32(offset)})
	if err != nil {
		return nil, 0, err
	}
	products, err := productsFromGen(rows)
	return products, total, err
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

// updateRow writes the full product row (the raw persistence Update, formerly
// repo.ProductRepo.Update). Unexported on purpose: the public Update() forbids
// arbitrary changes; the allowed mutations funnel here.
func (s *ProductService) updateRow(ctx context.Context, product *models.Product) error {
	entSpec, err := models.ToJSONB(product.EntitlementsSpec)
	if err != nil {
		return err
	}
	credSpec, err := models.ToJSONB(product.CreditsSpec)
	if err != nil {
		return err
	}
	var desc *string
	if product.Description != "" {
		desc = &product.Description
	}
	tierRank32, err := productTierRankInt32(product.TierRank)
	if err != nil {
		return err
	}
	rows, err := s.db.Gen(ctx).UpdateProduct(ctx, gen.UpdateProductParams{
		ID:               product.ID,
		Key:              product.Key,
		DisplayName:      product.DisplayName,
		Description:      desc,
		EntitlementsSpec: entSpec,
		CreditsSpec:      credSpec,
		TierGroup:        product.TierGroup,
		TierRank:         tierRank32,
		Archived:         product.Archived,
		UpdatedAt:        models.UpdateTimestamp(product.UpdatedAt),
	})
	if err != nil {
		return err
	}
	if rows < 1 {
		return errors.New("no rows affected")
	}
	return nil
}

func (s *ProductService) GetByKey(ctx context.Context, key string) (*models.Product, error) {
	row, err := s.db.Gen(ctx).GetProductByKey(ctx, key)
	if err != nil {
		return nil, err
	}
	return models.ProductFromGen(row)
}

// Deactivate archives a product so it won't appear in product listings and
// cannot be purchased. Existing subscriptions referencing this product's
// prices are grandfathered and keep billing.
func (s *ProductService) Deactivate(ctx context.Context, id uuid.UUID) error {
	return s.SetArchived(ctx, id, true)
}

// Activate un-archives a product so it appears in product listings.
func (s *ProductService) Activate(ctx context.Context, id uuid.UUID) error {
	return s.SetArchived(ctx, id, false)
}

// SetArchived sets the archived lifecycle flag on a product.
func (s *ProductService) SetArchived(ctx context.Context, id uuid.UUID, archived bool) error {
	product, err := s.GetByID(ctx, id)
	if err != nil {
		return err
	}
	product.Archived = archived
	return s.updateRow(ctx, product)
}

// UpdateDisplayName updates only the display name (cosmetic, does not affect historical data).
func (s *ProductService) UpdateDisplayName(ctx context.Context, id uuid.UUID, displayName string) error {
	product, err := s.GetByID(ctx, id)
	if err != nil {
		return err
	}
	product.DisplayName = displayName
	return s.updateRow(ctx, product)
}

// UpdateDescription updates only the description (cosmetic, does not affect historical data).
func (s *ProductService) UpdateDescription(ctx context.Context, id uuid.UUID, description string) error {
	product, err := s.GetByID(ctx, id)
	if err != nil {
		return err
	}
	product.Description = description
	return s.updateRow(ctx, product)
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
	Archived         *bool
}

// UpdateDefinition updates host-configurable fields on a product (catalog definition surface).
//
// This is intentionally separate from Update(), which forbids arbitrary changes for safety.
func (s *ProductService) UpdateDefinition(ctx context.Context, id uuid.UUID, params ProductDefinitionUpdateParams) (*models.Product, error) {
	product, err := s.GetByID(ctx, id)
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
	if params.Archived != nil {
		product.Archived = *params.Archived
	}
	if params.SetCredits {
		product.CreditsSpec = params.CreditsSpec
	}

	if err := s.updateRow(ctx, product); err != nil {
		return nil, err
	}
	return product, nil
}
