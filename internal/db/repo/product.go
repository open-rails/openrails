package repo

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
)

type ProductRepo struct {
	db *db.DB
}

func NewProductRepo(d *db.DB) *ProductRepo { return &ProductRepo{db: d} }

func (r *ProductRepo) Create(ctx context.Context, product *models.Product) error {
	res, err := r.db.Q(ctx).NewInsert().Model(product).Exec(ctx)
	if err != nil {
		return err
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if rows < 1 {
		return errors.New("no rows affected")
	}
	return nil
}

func (r *ProductRepo) GetByID(ctx context.Context, id uuid.UUID) (*models.Product, error) {
	product := new(models.Product)
	if err := r.db.Q(ctx).NewSelect().Model(product).Where("prod.id = ?", id).Scan(ctx); err != nil {
		return nil, err
	}
	return product, nil
}

func (r *ProductRepo) GetActive(ctx context.Context) ([]*models.Product, error) {
	products := []*models.Product{}
	if err := r.db.Q(ctx).NewSelect().Model(&products).Where("prod.status = ?", models.CatalogStatusActive).Scan(ctx); err != nil {
		return nil, err
	}
	return products, nil
}

func (r *ProductRepo) GetAll(ctx context.Context) ([]*models.Product, error) {
	products := []*models.Product{}
	if err := r.db.Q(ctx).NewSelect().Model(&products).Scan(ctx); err != nil {
		return nil, err
	}
	return products, nil
}

// GetActivePaginated returns active products with pagination
func (r *ProductRepo) GetActivePaginated(ctx context.Context, limit, offset int) ([]*models.Product, int64, error) {
	products := []*models.Product{}
	count, err := r.db.Q(ctx).NewSelect().
		Model(&products).
		Where("prod.status = ?", models.CatalogStatusActive).
		Order("prod.created_at DESC").
		Limit(limit).
		Offset(offset).
		ScanAndCount(ctx)
	if err != nil {
		return nil, 0, err
	}
	return products, int64(count), nil
}

// GetAllPaginated returns all products with pagination
func (r *ProductRepo) GetAllPaginated(ctx context.Context, limit, offset int) ([]*models.Product, int64, error) {
	products := []*models.Product{}
	count, err := r.db.Q(ctx).NewSelect().
		Model(&products).
		Order("prod.created_at DESC").
		Limit(limit).
		Offset(offset).
		ScanAndCount(ctx)
	if err != nil {
		return nil, 0, err
	}
	return products, int64(count), nil
}

func (r *ProductRepo) Update(ctx context.Context, product *models.Product) error {
	res, err := r.db.Q(ctx).NewUpdate().Model(product).WherePK().Exec(ctx)
	if err != nil {
		return err
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if rows < 1 {
		return errors.New("no rows affected")
	}
	return nil
}

func (r *ProductRepo) Delete(ctx context.Context, id uuid.UUID) error {
	res, err := r.db.Q(ctx).NewDelete().Model((*models.Product)(nil)).Where("prod.id = ?", id).Exec(ctx)
	if err != nil {
		return err
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if rows < 1 {
		return errors.New("no rows affected")
	}
	return nil
}

func (r *ProductRepo) GetBySlug(ctx context.Context, slug string) (*models.Product, error) {
	product := new(models.Product)
	if err := r.db.Q(ctx).NewSelect().Model(product).Where("prod.slug = ?", slug).Scan(ctx); err != nil {
		return nil, err
	}
	return product, nil
}
