package repo

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
)

type CreditTypeRepo struct {
	db *db.DB
}

func NewCreditTypeRepo(d *db.DB) *CreditTypeRepo { return &CreditTypeRepo{db: d} }

func (r *CreditTypeRepo) Create(ctx context.Context, ct *models.CreditType) error {
	res, err := r.db.GetDB().NewInsert().Model(ct).Exec(ctx)
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

func (r *CreditTypeRepo) GetByID(ctx context.Context, id uuid.UUID) (*models.CreditType, error) {
	ct := new(models.CreditType)
	if err := r.db.GetDB().NewSelect().Model(ct).Where("ct.id = ?", id).Scan(ctx); err != nil {
		return nil, err
	}
	return ct, nil
}

func (r *CreditTypeRepo) GetByName(ctx context.Context, name string) (*models.CreditType, error) {
	ct := new(models.CreditType)
	if err := r.db.GetDB().NewSelect().Model(ct).Where("ct.name = ?", name).Limit(1).Scan(ctx); err != nil {
		return nil, err
	}
	return ct, nil
}

func (r *CreditTypeRepo) List(ctx context.Context, activeOnly bool) ([]*models.CreditType, error) {
	items := []*models.CreditType{}
	q := r.db.GetDB().NewSelect().Model(&items).OrderExpr("ct.created_at ASC")
	if activeOnly {
		q = q.Where("ct.is_active = true")
	}
	if err := q.Scan(ctx); err != nil {
		return nil, err
	}
	return items, nil
}

func (r *CreditTypeRepo) Update(ctx context.Context, ct *models.CreditType) error {
	res, err := r.db.GetDB().NewUpdate().Model(ct).WherePK().Exec(ctx)
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

func (r *CreditTypeRepo) Activate(ctx context.Context, id uuid.UUID) error {
	ct, err := r.GetByID(ctx, id)
	if err != nil {
		return err
	}
	ct.IsActive = true
	return r.Update(ctx, ct)
}

func (r *CreditTypeRepo) Deactivate(ctx context.Context, id uuid.UUID) error {
	ct, err := r.GetByID(ctx, id)
	if err != nil {
		return err
	}
	ct.IsActive = false
	return r.Update(ctx, ct)
}
