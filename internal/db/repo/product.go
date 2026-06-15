package repo

import (
	"context"
	"errors"

	safecast "github.com/ccoveille/go-safecast/v2"
	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
)

type ProductRepo struct {
	db *db.DB
}

func NewProductRepo(d *db.DB) *ProductRepo { return &ProductRepo{db: d} }

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
	tierRank32, _ := safecast.Convert[int32](product.TierRank)
	rows, err := r.db.Gen(ctx).CreateProduct(ctx, gen.CreateProductParams{
		ID:               product.ID,
		MerchantID:       product.MerchantID,
		Slug:             product.Slug,
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
	limit32, _ := safecast.Convert[int32](limit)
	offset32, _ := safecast.Convert[int32](offset)
	rows, err := q.ListActiveProductsPaged(ctx, gen.ListActiveProductsPagedParams{
		PageLimit:  limit32,
		PageOffset: offset32,
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
	limit32, _ := safecast.Convert[int32](limit)
	offset32, _ := safecast.Convert[int32](offset)
	rows, err := q.ListAllProductsPaged(ctx, gen.ListAllProductsPagedParams{
		PageLimit:  limit32,
		PageOffset: offset32,
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
	tierRank32, _ := safecast.Convert[int32](product.TierRank)
	rows, err := r.db.Gen(ctx).UpdateProduct(ctx, gen.UpdateProductParams{
		ID:               product.ID,
		Slug:             product.Slug,
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

func (r *ProductRepo) GetBySlug(ctx context.Context, slug string) (*models.Product, error) {
	row, err := r.db.Gen(ctx).GetProductBySlug(ctx, slug)
	if err != nil {
		return nil, err
	}
	return productFromGen(row)
}
