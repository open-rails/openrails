package repo

import (
	"context"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
)

type AdminGrantRepo struct {
	db *db.DB
}

func NewAdminGrantRepo(db *db.DB) *AdminGrantRepo {
	return &AdminGrantRepo{db: db}
}

// Create inserts a new admin grant record
func (r *AdminGrantRepo) Create(ctx context.Context, grant *models.AdminGrant) error {
	if err := ensureTenantSubjectRow(ctx, r.db.Q(ctx), uuid.Nil, grant.TenantSubjectID); err != nil {
		return err
	}
	_, err := r.db.Q(ctx).NewInsert().Model(grant).Exec(ctx)
	return err
}

// GetByID retrieves an admin grant by ID
func (r *AdminGrantRepo) GetByID(ctx context.Context, id uuid.UUID) (*models.AdminGrant, error) {
	grant := &models.AdminGrant{}
	err := r.db.Q(ctx).NewSelect().
		Model(grant).
		Where("ag.id = ?", id).
		Relation("Price").
		Relation("Payment").
		Scan(ctx)
	if err != nil {
		return nil, err
	}
	return grant, nil
}

// ListByUserID retrieves all admin grants for a user
func (r *AdminGrantRepo) ListByUserID(ctx context.Context, userID string, limit, offset int) ([]models.AdminGrant, int, error) {
	tsid, err := ResolveTenantSubjectID(ctx, r.db.Q(ctx), uuid.Nil, userID)
	if err != nil {
		return nil, 0, err
	}
	var grants []models.AdminGrant

	count, err := r.db.Q(ctx).NewSelect().
		Model(&grants).
		Where("ag.tenant_subject_id = ?", tsid).
		Relation("Price").
		Relation("Payment").
		Order("ag.created_at DESC").
		Limit(limit).
		Offset(offset).
		ScanAndCount(ctx)
	if err != nil {
		return nil, 0, err
	}

	return grants, count, nil
}

// ListByGrantedBy retrieves all admin grants made by a specific admin
func (r *AdminGrantRepo) ListByGrantedBy(ctx context.Context, grantedBy string, limit, offset int) ([]models.AdminGrant, int, error) {
	var grants []models.AdminGrant

	count, err := r.db.Q(ctx).NewSelect().
		Model(&grants).
		Where("ag.granted_by = ?", grantedBy).
		Relation("Price").
		Relation("Payment").
		Order("ag.created_at DESC").
		Limit(limit).
		Offset(offset).
		ScanAndCount(ctx)
	if err != nil {
		return nil, 0, err
	}

	return grants, count, nil
}
