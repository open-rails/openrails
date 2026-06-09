package repo

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/pkg/tenant"
)

var ErrUSDCFundingSessionNotFound = errors.New("usdc funding session not found")

type USDCFundingSessionRepo struct {
	db *db.DB
}

func NewUSDCFundingSessionRepo(d *db.DB) *USDCFundingSessionRepo {
	return &USDCFundingSessionRepo{db: d}
}

func (r *USDCFundingSessionRepo) CreateForUserID(ctx context.Context, userID string, session *models.USDCFundingSession) error {
	tsid, err := EnsureTenantSubjectID(ctx, r.db.Q(ctx), uuid.Nil, userID)
	if err != nil {
		return err
	}
	if err := ensureTenantSubjectRow(ctx, r.db.Q(ctx), uuid.Nil, tsid); err != nil {
		return err
	}
	session.TenantID = tenant.FromContextOrDefault(ctx).UUID()
	session.TenantSubjectID = tsid
	if session.Metadata == nil {
		session.Metadata = map[string]any{}
	}
	_, err = r.db.Q(ctx).NewInsert().Model(session).Exec(ctx)
	return err
}

func (r *USDCFundingSessionRepo) GetByIDForUserID(ctx context.Context, id uuid.UUID, userID string) (*models.USDCFundingSession, error) {
	tsid, err := ResolveTenantSubjectID(ctx, r.db.Q(ctx), uuid.Nil, userID)
	if err != nil {
		return nil, err
	}
	if tsid == uuid.Nil {
		return nil, ErrUSDCFundingSessionNotFound
	}
	session := new(models.USDCFundingSession)
	err = r.db.Q(ctx).NewSelect().Model(session).
		Where("ufs.id = ?", id).
		Where("ufs.tenant_subject_id = ?", tsid).
		Limit(1).
		Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrUSDCFundingSessionNotFound
	}
	return session, err
}

func (r *USDCFundingSessionRepo) GetByID(ctx context.Context, id uuid.UUID) (*models.USDCFundingSession, error) {
	session := new(models.USDCFundingSession)
	err := r.db.Q(ctx).NewSelect().Model(session).
		Where("ufs.id = ?", id).
		Limit(1).
		Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrUSDCFundingSessionNotFound
	}
	return session, err
}

func (r *USDCFundingSessionRepo) GetByIdempotencyKeyForUserID(ctx context.Context, userID, key string) (*models.USDCFundingSession, error) {
	key = strings.TrimSpace(key)
	if key == "" {
		return nil, ErrUSDCFundingSessionNotFound
	}
	tsid, err := ResolveTenantSubjectID(ctx, r.db.Q(ctx), uuid.Nil, userID)
	if err != nil {
		return nil, err
	}
	if tsid == uuid.Nil {
		return nil, ErrUSDCFundingSessionNotFound
	}
	session := new(models.USDCFundingSession)
	err = r.db.Q(ctx).NewSelect().Model(session).
		Where("ufs.tenant_subject_id = ?", tsid).
		Where("ufs.idempotency_key = ?", key).
		Limit(1).
		Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrUSDCFundingSessionNotFound
	}
	return session, err
}

func (r *USDCFundingSessionRepo) UpdateStatus(ctx context.Context, session *models.USDCFundingSession, status models.USDCFundingSessionStatus, checkedAt *time.Time, metadata map[string]any) error {
	session.Status = status
	session.LastCheckedAt = checkedAt
	if metadata != nil {
		session.Metadata = metadata
	}
	session.UpdatedAt = time.Now().UTC()
	_, err := r.db.Q(ctx).NewUpdate().Model(session).
		Column("status", "last_checked_at", "metadata", "updated_at").
		WherePK().
		Exec(ctx)
	return err
}
