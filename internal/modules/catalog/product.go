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
	"github.com/open-rails/openrails/billing"
	catalogwire "github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/pagination"
	"github.com/open-rails/openrails/internal/shared/apperr"
)

// ErrProductTierGroupInUse requires an explicit product migration instead of regrouping live subscriptions.
var ErrProductTierGroupInUse = apperr.New(http.StatusConflict, "product_tier_group_in_use", "product tier group cannot change while a live subscription has a plan change in flight")

// ErrProductTierGroupConflict refuses joining products into a group in which
// a customer would hold two live memberships.
var ErrProductTierGroupConflict = apperr.New(http.StatusConflict, "product_tier_group_conflict", "a customer holds live subscriptions to more than one product of this tier group")

type ProductService struct {
	db *db.DB
}

func NewProductService(db *db.DB) *ProductService {
	return &ProductService{db: db}
}

func productTierRankInt32(v int) (int32, error) {
	if v < math.MinInt32 || v > math.MaxInt32 {
		return 0, fmt.Errorf("product tier_rank %d outside int32 range", v)
	}
	return int32(v), nil
}

func productsFromGen(rows []gen.BillingProduct) ([]*models.Product, error) {
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
	mid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	if product.MerchantID != uuid.Nil && product.MerchantID != mid.UUID() {
		return fmt.Errorf("product merchant does not match the authorized merchant")
	}
	product.MerchantID = mid.UUID()
	entSpec, err := models.ToJSONB(product.EntitlementsSpec)
	if err != nil {
		return err
	}
	credit, err := models.PointerToJSONB(product.CreditGrant)
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
		EntitlementsSpec: entSpec, CreditGrant: credit,
		TierGroup: product.TierGroup,
		TierRank:  tierRank32,
		Archived:  product.Archived,
		CreatedAt: product.CreatedAt,
		UpdatedAt: product.UpdatedAt,
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
	queryMerchant, queryScopeErr := merchant.Require(ctx)
	if queryScopeErr != nil {
		return nil, queryScopeErr
	}

	row, err := s.db.Gen(ctx).GetProductByID(ctx, gen.GetProductByIDParams{MerchantID: queryMerchant.UUID(), ID: id})
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
	queryMerchant, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.Gen(ctx).ListProductsByIDs(ctx, gen.ListProductsByIDsParams{MerchantID: queryMerchant.UUID(), Ids: ids})
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
	queryMerchant, queryScopeErr := merchant.Require(ctx)
	if queryScopeErr != nil {
		return nil, queryScopeErr
	}

	rows, err := s.db.Gen(ctx).ListActiveProducts(ctx, queryMerchant.UUID())
	if err != nil {
		return nil, err
	}
	return productsFromGen(rows)
}

func (s *ProductService) GetAll(ctx context.Context) ([]*models.Product, error) {
	queryMerchant, queryScopeErr := merchant.Require(ctx)
	if queryScopeErr != nil {
		return nil, queryScopeErr
	}

	rows, err := s.db.Gen(ctx).ListAllProducts(ctx, queryMerchant.UUID())
	if err != nil {
		return nil, err
	}
	return productsFromGen(rows)
}

// ProductFilter selects products. Archived nil lists every product; false
// lists live products only; true lists archived products only.
type ProductFilter struct {
	Archived  *bool
	TierGroup string
}

// List returns one keyset page of products, newest first.
func (s *ProductService) List(ctx context.Context, filter ProductFilter, page billing.PageRequest) (billing.ListPage[*models.Product], error) {
	queryMerchant, queryScopeErr := merchant.Require(ctx)
	if queryScopeErr != nil {
		return billing.ListPage[*models.Product]{}, queryScopeErr
	}
	limit, err := pagination.Limit(page)
	if err != nil {
		return billing.ListPage[*models.Product]{}, err
	}
	afterAt, afterID, err := pagination.After(page.Cursor)
	if err != nil {
		return billing.ListPage[*models.Product]{}, err
	}
	rows, err := s.db.Gen(ctx).ListProductsFiltered(ctx, gen.ListProductsFilteredParams{MerchantID: queryMerchant.UUID(), Archived: filter.Archived,
		TierGroup: strings.TrimSpace(filter.TierGroup), AfterAt: afterAt, AfterID: afterID, FetchLimit: pagination.Fetch(limit)})
	if err != nil {
		return billing.ListPage[*models.Product]{}, err
	}
	products, err := productsFromGen(rows)
	if err != nil {
		return billing.ListPage[*models.Product]{}, err
	}
	return pagination.Cut(products, limit, func(p *models.Product) any { return pagination.TimeID{At: p.CreatedAt, ID: p.ID} }), nil
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
	queryMerchant, queryScopeErr := merchant.Require(ctx)
	if queryScopeErr != nil {
		return nil, queryScopeErr
	}

	row, err := s.db.Gen(ctx).GetProductByKey(ctx, gen.GetProductByKeyParams{MerchantID: queryMerchant.UUID(), Key: key})
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
	CreditGrant      *catalogwire.CreditGrantSpec
	SetCreditGrant   bool
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
	queryMerchant, queryScopeErr := merchant.Require(ctx)
	if queryScopeErr != nil {
		return nil, queryScopeErr
	}
	entSpec, err := models.ToJSONB(params.EntitlementsSpec)
	if err != nil {
		return nil, err
	}
	credit, err := models.PointerToJSONB(params.CreditGrant)
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
	row, err := s.db.Gen(ctx).PatchProduct(ctx, gen.PatchProductParams{MerchantID: queryMerchant.UUID(),
		ID: id, DisplayName: params.DisplayName,
		Description: params.Description, SetDescription: params.Description != nil,
		EntitlementsSpec: entSpec, SetEntitlements: params.SetEntitlements, CreditGrant: credit, SetCreditGrant: params.SetCreditGrant,
		TierGroup: params.TierGroup, SetTierGroup: params.SetTierGroup,
		TierRank: rank, Archived: params.Archived,
	})
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.ConstraintName == "products_live_subscription_tier_group" {
			return nil, ErrProductTierGroupInUse
		}
		if errors.As(err, &pgErr) && pgErr.ConstraintName == "subscriptions_customer_id_tier_group_key" {
			return nil, ErrProductTierGroupConflict
		}
		return nil, err
	}
	return models.ProductFromGen(row)
}
