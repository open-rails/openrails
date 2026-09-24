package catalog

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/shared/apperr"
)

// ErrProductTierGroupInUse requires an explicit product migration instead of regrouping live subscriptions.
var ErrProductTierGroupInUse = apperr.New(http.StatusConflict, "product_tier_group_in_use", "product tier group cannot change while subscriptions are live")

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
	mid, ownerCatalog, err := queryCatalogScope(ctx)
	if err != nil {
		return err
	}
	if product.MerchantID != uuid.Nil && product.MerchantID != mid.UUID() {
		return fmt.Errorf("product merchant does not match the authorized merchant")
	}
	if ownerCatalog != nil && (product.EntitlementsSpec != nil || product.TierGroup != nil || product.TierRank != 0) {
		return ErrOwnerOperation
	}
	var requested *uuid.UUID
	if product.CatalogID != uuid.Nil {
		requested = &product.CatalogID
	}
	catalogID, err := selectCatalogFilter(ctx, s.db, requested)
	if err != nil {
		return err
	}
	if catalogID == nil {
		defaultCatalog, err := NewCatalogRepo(s.db).Ensure(ctx, nil)
		if err != nil {
			return err
		}
		catalogID = &defaultCatalog.ID
	}
	product.MerchantID = mid.UUID()
	product.CatalogID = *catalogID
	entSpec, err := models.ToJSONB(product.EntitlementsSpec)
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
		CatalogID:        catalogID,
		Key:              product.Key,
		DisplayName:      product.DisplayName,
		Description:      desc,
		EntitlementsSpec: entSpec,
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
	queryMerchant, catalogID, queryScopeErr := queryCatalogScope(ctx)
	if queryScopeErr != nil {
		return nil, queryScopeErr
	}

	row, err := s.db.Gen(ctx).GetProductByID(ctx, gen.GetProductByIDParams{MerchantID: queryMerchant.UUID(), CatalogID: catalogID, ID: id})
	if err != nil {
		return nil, err
	}
	return models.ProductFromGen(row)
}

// GetByIDs loads the scoped products among ids in one query; absent IDs are
// omitted from the result.
func (s *ProductService) GetByIDs(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]*models.Product, error) {
	out := make(map[uuid.UUID]*models.Product, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	queryMerchant, catalogID, err := queryCatalogScope(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.Gen(ctx).ListProductsByIDs(ctx, gen.ListProductsByIDsParams{MerchantID: queryMerchant.UUID(), CatalogID: catalogID, Ids: ids})
	if err != nil {
		return nil, err
	}
	products, err := productsFromGen(rows)
	if err != nil {
		return nil, err
	}
	for _, product := range products {
		out[product.ID] = product
	}
	return out, nil
}

func (s *ProductService) GetActive(ctx context.Context) ([]*models.Product, error) {
	queryMerchant, catalogID, queryScopeErr := queryCatalogScope(ctx)
	if queryScopeErr != nil {
		return nil, queryScopeErr
	}

	rows, err := s.db.Gen(ctx).ListActiveProducts(ctx, gen.ListActiveProductsParams{MerchantID: queryMerchant.UUID(), CatalogID: catalogID})
	if err != nil {
		return nil, err
	}
	return productsFromGen(rows)
}

func (s *ProductService) GetAll(ctx context.Context) ([]*models.Product, error) {
	queryMerchant, catalogID, queryScopeErr := queryCatalogScope(ctx)
	if queryScopeErr != nil {
		return nil, queryScopeErr
	}

	rows, err := s.db.Gen(ctx).ListAllProducts(ctx, gen.ListAllProductsParams{MerchantID: queryMerchant.UUID(), CatalogID: catalogID})
	if err != nil {
		return nil, err
	}
	return productsFromGen(rows)
}

// ProductFilter selects products. Archived nil lists every product; false
// lists live products only; true lists archived products only.
type ProductFilter struct {
	CatalogID *uuid.UUID
	Archived  *bool
	TierGroup string
}

func (s *ProductService) GetActivePaginated(ctx context.Context, limit, offset int) ([]*models.Product, int64, error) {
	live := false
	return s.GetPaginated(ctx, ProductFilter{Archived: &live}, limit, offset)
}

func (s *ProductService) GetAllPaginated(ctx context.Context, limit, offset int) ([]*models.Product, int64, error) {
	return s.GetPaginated(ctx, ProductFilter{}, limit, offset)
}

// GetPaginated applies identical filters to the count and page before slicing.
func (s *ProductService) GetPaginated(ctx context.Context, filter ProductFilter, limit, offset int) ([]*models.Product, int64, error) {
	queryMerchant, _, queryScopeErr := queryCatalogScope(ctx)
	if queryScopeErr != nil {
		return nil, 0, queryScopeErr
	}
	catalogID, err := selectCatalogFilter(ctx, s.db, filter.CatalogID)
	if err != nil {
		return nil, 0, err
	}

	q := s.db.Gen(ctx)
	tierGroup := strings.TrimSpace(filter.TierGroup)
	total, err := q.CountProductsFiltered(ctx, gen.CountProductsFilteredParams{MerchantID: queryMerchant.UUID(), CatalogID: catalogID, Archived: filter.Archived, TierGroup: tierGroup})
	if err != nil {
		return nil, 0, err
	}
	rows, err := q.ListProductsFiltered(ctx, gen.ListProductsFilteredParams{MerchantID: queryMerchant.UUID(), CatalogID: catalogID, Archived: filter.Archived, TierGroup: tierGroup, PageLimit: productPageInt32(limit), PageOffset: productPageInt32(offset)})
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

func (s *ProductService) GetByKey(ctx context.Context, key string) (*models.Product, error) {
	queryMerchant, catalogID, queryScopeErr := queryCatalogScope(ctx)
	if queryScopeErr != nil {
		return nil, queryScopeErr
	}

	row, err := s.db.Gen(ctx).GetProductByKey(ctx, gen.GetProductByKeyParams{MerchantID: queryMerchant.UUID(), CatalogID: catalogID, Key: key})
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
	_, err := s.UpdateDefinition(ctx, id, ProductDefinitionUpdateParams{Archived: &archived})
	return err
}

// UpdateDisplayName changes only the display name.
func (s *ProductService) UpdateDisplayName(ctx context.Context, id uuid.UUID, displayName string) error {
	_, err := s.UpdateDefinition(ctx, id, ProductDefinitionUpdateParams{DisplayName: &displayName})
	return err
}

// UpdateDescription changes only the description; empty clears it.
func (s *ProductService) UpdateDescription(ctx context.Context, id uuid.UUID, description string) error {
	_, err := s.UpdateDefinition(ctx, id, ProductDefinitionUpdateParams{Description: &description})
	return err
}

type ProductDefinitionUpdateParams struct {
	DisplayName      *string
	Description      *string
	EntitlementsSpec map[string]*int
	SetEntitlements  bool
	TierGroup        *string
	SetTierGroup     bool
	TierRank         *int
	Archived         *bool
}

// UpdateDefinition atomically applies only the supplied fields. Set flags
// distinguish omission from clearing nullable definitions; nil scalar pointers
// leave their columns unchanged. A description of "" clears it.
func (s *ProductService) UpdateDefinition(ctx context.Context, id uuid.UUID, params ProductDefinitionUpdateParams) (*models.Product, error) {
	queryMerchant, catalogID, queryScopeErr := queryCatalogScope(ctx)
	if queryScopeErr != nil {
		return nil, queryScopeErr
	}
	if catalogID != nil && (params.EntitlementsSpec != nil || params.SetEntitlements || params.TierGroup != nil || params.SetTierGroup || params.TierRank != nil) {
		return nil, ErrOwnerOperation
	}

	entSpec, err := models.ToJSONB(params.EntitlementsSpec)
	if err != nil {
		return nil, err
	}
	var rank *int32
	if params.TierRank != nil {
		value, err := productTierRankInt32(*params.TierRank)
		if err != nil {
			return nil, err
		}
		rank = &value
	}
	row, err := s.db.Gen(ctx).PatchProduct(ctx, gen.PatchProductParams{MerchantID: queryMerchant.UUID(), CatalogID: catalogID,
		ID: id, DisplayName: params.DisplayName,
		Description: params.Description, SetDescription: params.Description != nil,
		EntitlementsSpec: entSpec, SetEntitlements: params.SetEntitlements,
		TierGroup: params.TierGroup, SetTierGroup: params.SetTierGroup,
		TierRank: rank, Archived: params.Archived,
	})
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.ConstraintName == "products_live_subscription_tier_group" {
			return nil, ErrProductTierGroupInUse
		}
		return nil, err
	}
	return models.ProductFromGen(row)
}
