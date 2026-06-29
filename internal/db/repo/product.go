package repo

import (
	"context"
	"errors"
	"fmt"
	"math"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
)

type ProductRepo struct {
	db *db.DB
}

func NewProductRepo(d *db.DB) *ProductRepo { return &ProductRepo{db: d} }

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
		p, err := productFromGen(r)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
}

func (r *ProductRepo) Create(ctx context.Context, product *models.Product) error {
	entSpec, err := toJSONB(product.EntitlementsSpec)
	if err != nil {
		return err
	}
	credSpec, err := toJSONB(product.CreditsSpec)
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
	rows, err := r.db.Gen(ctx).CreateProduct(ctx, gen.CreateProductParams{
		ID:               product.ID,
		MerchantID:       product.MerchantID,
		Key:              product.Key,
		DisplayName:      product.DisplayName,
		Description:      desc,
		EntitlementsSpec: entSpec,
		CreditsSpec:      credSpec,
		TierGroup:        product.TierGroup,
		TierRank:         tierRank32,
		Status:           string(product.Status),
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

func (r *ProductRepo) GetByID(ctx context.Context, id uuid.UUID) (*models.Product, error) {
	row, err := r.db.Gen(ctx).GetProductByID(ctx, id)
	if err != nil {
		return nil, err
	}
	return productFromGen(row)
}

func (r *ProductRepo) GetActive(ctx context.Context) ([]*models.Product, error) {
	rows, err := r.db.Gen(ctx).ListActiveProducts(ctx)
	if err != nil {
		return nil, err
	}
	return productsFromGen(rows)
}

func (r *ProductRepo) GetAll(ctx context.Context) ([]*models.Product, error) {
	rows, err := r.db.Gen(ctx).ListAllProducts(ctx)
	if err != nil {
		return nil, err
	}
	return productsFromGen(rows)
}

// GetActivePaginated returns active products with pagination
func (r *ProductRepo) GetActivePaginated(ctx context.Context, limit, offset int) ([]*models.Product, int64, error) {
	q := r.db.Gen(ctx)
	total, err := q.CountActiveProducts(ctx)
	if err != nil {
		return nil, 0, err
	}
	rows, err := q.ListActiveProductsPaged(ctx, gen.ListActiveProductsPagedParams{
		PageLimit:  productPageInt32(limit),
		PageOffset: productPageInt32(offset),
	})
	if err != nil {
		return nil, 0, err
	}
	products, err := productsFromGen(rows)
	if err != nil {
		return nil, 0, err
	}
	return products, total, nil
}

// GetAllPaginated returns all products with pagination
func (r *ProductRepo) GetAllPaginated(ctx context.Context, limit, offset int) ([]*models.Product, int64, error) {
	q := r.db.Gen(ctx)
	total, err := q.CountAllProducts(ctx)
	if err != nil {
		return nil, 0, err
	}
	rows, err := q.ListAllProductsPaged(ctx, gen.ListAllProductsPagedParams{
		PageLimit:  productPageInt32(limit),
		PageOffset: productPageInt32(offset),
	})
	if err != nil {
		return nil, 0, err
	}
	products, err := productsFromGen(rows)
	if err != nil {
		return nil, 0, err
	}
	return products, total, nil
}

func (r *ProductRepo) Update(ctx context.Context, product *models.Product) error {
	entSpec, err := toJSONB(product.EntitlementsSpec)
	if err != nil {
		return err
	}
	credSpec, err := toJSONB(product.CreditsSpec)
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
	rows, err := r.db.Gen(ctx).UpdateProduct(ctx, gen.UpdateProductParams{
		ID:               product.ID,
		Key:              product.Key,
		DisplayName:      product.DisplayName,
		Description:      desc,
		EntitlementsSpec: entSpec,
		CreditsSpec:      credSpec,
		TierGroup:        product.TierGroup,
		TierRank:         tierRank32,
		Status:           string(product.Status),
		UpdatedAt:        updateTimestamp(product.UpdatedAt),
	})
	if err != nil {
		return err
	}
	if rows < 1 {
		return errors.New("no rows affected")
	}
	return nil
}

func (r *ProductRepo) Delete(ctx context.Context, id uuid.UUID) error {
	rows, err := r.db.Gen(ctx).DeleteProduct(ctx, id)
	if err != nil {
		return err
	}
	if rows < 1 {
		return errors.New("no rows affected")
	}
	return nil
}

func (r *ProductRepo) GetByKey(ctx context.Context, key string) (*models.Product, error) {
	row, err := r.db.Gen(ctx).GetProductByKey(ctx, key)
	if err != nil {
		return nil, err
	}
	return productFromGen(row)
}
