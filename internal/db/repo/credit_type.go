package repo

import (
	"context"
	"errors"
	"math"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
)

type CreditTypeRepo struct {
	db *db.DB
}

func NewCreditTypeRepo(d *db.DB) *CreditTypeRepo { return &CreditTypeRepo{db: d} }

func creditTypeFromGen(ct gen.BillingCreditType) *models.CreditType {
	return &models.CreditType{
		ID:            ct.ID,
		Name:          ct.Name,
		DisplayName:   ct.DisplayName,
		Unit:          ct.Unit,
		DecimalPlaces: int(ct.DecimalPlaces),
		IsActive:      ct.IsActive,
		CreatedAt:     ct.CreatedAt,
	}
}

func (r *CreditTypeRepo) Create(ctx context.Context, ct *models.CreditType) error {
	rows, err := r.db.Gen(ctx).CreateCreditType(ctx, gen.CreateCreditTypeParams{
		ID:            ct.ID,
		Name:          ct.Name,
		DisplayName:   ct.DisplayName,
		Unit:          ct.Unit,
		DecimalPlaces: int32(min(ct.DecimalPlaces, math.MaxInt32)),
		IsActive:      ct.IsActive,
		CreatedAt:     ct.CreatedAt,
	})
	if err != nil {
		return err
	}
	if rows < 1 {
		return errors.New("no rows affected")
	}
	return nil
}

func (r *CreditTypeRepo) GetByID(ctx context.Context, id uuid.UUID) (*models.CreditType, error) {
	row, err := r.db.Gen(ctx).GetCreditTypeByID(ctx, id)
	if err != nil {
		return nil, err
	}
	return creditTypeFromGen(row), nil
}

func (r *CreditTypeRepo) GetByName(ctx context.Context, name string) (*models.CreditType, error) {
	row, err := r.db.Gen(ctx).GetCreditTypeByName(ctx, name)
	if err != nil {
		return nil, err
	}
	return creditTypeFromGen(row), nil
}

func (r *CreditTypeRepo) List(ctx context.Context, activeOnly bool) ([]*models.CreditType, error) {
	rows, err := r.db.Gen(ctx).ListCreditTypes(ctx, activeOnly)
	if err != nil {
		return nil, err
	}
	items := make([]*models.CreditType, 0, len(rows))
	for _, row := range rows {
		items = append(items, creditTypeFromGen(row))
	}
	return items, nil
}

func (r *CreditTypeRepo) Update(ctx context.Context, ct *models.CreditType) error {
	rows, err := r.db.Gen(ctx).UpdateCreditType(ctx, gen.UpdateCreditTypeParams{
		ID:            ct.ID,
		Name:          ct.Name,
		DisplayName:   ct.DisplayName,
		Unit:          ct.Unit,
		DecimalPlaces: int32(min(ct.DecimalPlaces, math.MaxInt32)),
		IsActive:      ct.IsActive,
	})
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
