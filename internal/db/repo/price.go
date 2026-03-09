package repo

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
)

type PriceRepo struct {
	db *db.DB
}

func NewPriceRepo(d *db.DB) *PriceRepo { return &PriceRepo{db: d} }

func (r *PriceRepo) Create(ctx context.Context, price *models.Price) error {
	res, err := r.db.GetDB().NewInsert().Model(price).Exec(ctx)
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

func (r *PriceRepo) GetByID(ctx context.Context, id uuid.UUID) (*models.Price, error) {
	price := new(models.Price)
	if err := r.db.GetDB().NewSelect().Model(price).Where("price.id = ?", id).Scan(ctx); err != nil {
		return nil, err
	}
	return price, nil
}

func (r *PriceRepo) GetByProductID(ctx context.Context, productID uuid.UUID) ([]*models.Price, error) {
	prices := []*models.Price{}
	if err := r.db.GetDB().NewSelect().Model(&prices).Where("price.product_id = ?", productID).Where("price.is_active = ?", true).Scan(ctx); err != nil {
		return nil, err
	}
	return prices, nil
}

func (r *PriceRepo) GetActiveByProductID(ctx context.Context, productID uuid.UUID) ([]*models.Price, error) {
	prices := []*models.Price{}
	if err := r.db.GetDB().NewSelect().Model(&prices).Where("price.product_id = ?", productID).Where("price.is_active = ?", true).OrderExpr("price.amount ASC").Scan(ctx); err != nil {
		return nil, err
	}
	return prices, nil
}

func (r *PriceRepo) GetAllActive(ctx context.Context) ([]*models.Price, error) {
	prices := []*models.Price{}
	if err := r.db.GetDB().NewSelect().Model(&prices).Relation("Product").Where("price.is_active = ?", true).OrderExpr("price.amount ASC").Scan(ctx); err != nil {
		return nil, err
	}
	return prices, nil
}

func (r *PriceRepo) GetAll(ctx context.Context) ([]*models.Price, error) {
	prices := []*models.Price{}
	if err := r.db.GetDB().NewSelect().Model(&prices).Relation("Product").OrderExpr("price.amount ASC").Scan(ctx); err != nil {
		return nil, err
	}
	return prices, nil
}

// PriceFilter contains optional filters for listing prices
type PriceFilter struct {
	Active    *bool      // Filter by active status
	Currency  string     // Filter by currency (e.g., "usd")
	ProductID *uuid.UUID // Filter by product ID
	Type      string     // Filter by "recurring" or "one_time"
}

// ListPaginated returns prices with pagination and optional filters
func (r *PriceRepo) ListPaginated(ctx context.Context, filter PriceFilter, limit, offset int) ([]*models.Price, int64, error) {
	prices := []*models.Price{}
	query := r.db.GetDB().NewSelect().
		Model(&prices).
		Relation("Product").
		Order("price.created_at DESC")

	// Apply filters
	if filter.Active != nil {
		query = query.Where("price.is_active = ?", *filter.Active)
	}
	if filter.Currency != "" {
		query = query.Where("LOWER(price.currency) = LOWER(?)", filter.Currency)
	}
	if filter.ProductID != nil {
		query = query.Where("price.product_id = ?", *filter.ProductID)
	}
	if filter.Type == "recurring" {
		query = query.Where("price.billing_cycle_days IS NOT NULL AND price.billing_cycle_days > 0")
	} else if filter.Type == "one_time" {
		query = query.Where("price.billing_cycle_days IS NULL OR price.billing_cycle_days = 0")
	}

	count, err := query.Limit(limit).Offset(offset).ScanAndCount(ctx)
	if err != nil {
		return nil, 0, err
	}
	return prices, int64(count), nil
}

func (r *PriceRepo) GetByNMIPlan(ctx context.Context, provider, nmiPlanID string) (*models.Price, error) {
	price := new(models.Price)

	// The provider parameter determines which processor key to look up directly (e.g., "mobius", "acme").
	query := r.db.GetDB().NewSelect().Model(price).
		Where("price.processors->'"+provider+"'->>'plan_id' = ?", nmiPlanID).
		Where("price.is_active = ?", true)

	if err := query.Scan(ctx); err != nil {
		return nil, err
	}
	return price, nil
}

func (r *PriceRepo) GetByCCBillPriceID(ctx context.Context, ccbillPriceID string) (*models.Price, error) {
	price := new(models.Price)
	if err := r.db.GetDB().NewSelect().Model(price).Relation("Product").
		Where("price.processors->'ccbill'->>'flex_id' = ?", ccbillPriceID).
		Where("price.is_active = ?", true).
		Scan(ctx); err != nil {
		return nil, err
	}
	return price, nil
}

func (r *PriceRepo) GetByStripePriceID(ctx context.Context, stripePriceID string) (*models.Price, error) {
	price := new(models.Price)
	if err := r.db.GetDB().NewSelect().Model(price).Relation("Product").
		Where("price.processors->'stripe'->>'price_id' = ?", stripePriceID).
		Where("price.is_active = ?", true).
		Scan(ctx); err != nil {
		return nil, err
	}
	return price, nil
}

func (r *PriceRepo) Update(ctx context.Context, price *models.Price) error {
	res, err := r.db.GetDB().NewUpdate().Model(price).WherePK().Exec(ctx)
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

func (r *PriceRepo) Delete(ctx context.Context, id uuid.UUID) error {
	res, err := r.db.GetDB().NewDelete().Model((*models.Price)(nil)).Where("price.id = ?", id).Exec(ctx)
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
