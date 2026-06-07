package repo

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/pkg/tenant"
)

// ProductAccessGrantRepo persists durable product ownership/access grants
// (issue #250). All tenant-owned queries go through r.db.Q(ctx) — the RLS-aware
// accessor (#227) — and stamp/scope the resolved tenant so single-tenant and
// multi-tenant runs behave identically.
type ProductAccessGrantRepo struct {
	db *db.DB
}

func NewProductAccessGrantRepo(d *db.DB) *ProductAccessGrantRepo {
	return &ProductAccessGrantRepo{db: d}
}

// Insert creates a grant, stamping the resolved tenant when the caller left it
// zero (consistent with reads).
func (r *ProductAccessGrantRepo) Insert(ctx context.Context, grant *models.ProductAccessGrant) error {
	if (grant.TenantID == uuid.UUID{}) {
		grant.TenantID = tenant.FromContextOrDefault(ctx).UUID()
	}
	if err := ensureTenantSubjectRow(ctx, r.db.Q(ctx), grant.TenantID, grant.TenantSubjectID); err != nil {
		return err
	}
	_, err := r.db.Q(ctx).NewInsert().Model(grant).Exec(ctx)
	return err
}

// GetActiveBySource returns the existing grant for a (user, product, source)
// idempotency key, or nil if none. Used to make granting idempotent.
func (r *ProductAccessGrantRepo) GetBySource(ctx context.Context, userID string, productID uuid.UUID, sourceID string) (*models.ProductAccessGrant, error) {
	tenantID := tenant.FromContextOrDefault(ctx).UUID()
	tsid, err := ResolveTenantSubjectID(ctx, r.db.Q(ctx), tenantID, userID)
	if err != nil {
		return nil, err
	}
	var grant models.ProductAccessGrant
	err = r.db.Q(ctx).NewSelect().
		Model(&grant).
		Where("pag.tenant_id = ?", tenantID).
		Where("pag.tenant_subject_id = ?", tsid).
		Where("pag.product_id = ?", productID).
		Where("pag.source_id = ?", sourceID).
		Limit(1).
		Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return &grant, nil
}

// HasActiveAccess reports whether the user has an active, in-window grant for the
// product at time t.
func (r *ProductAccessGrantRepo) HasActiveAccess(ctx context.Context, userID string, productID uuid.UUID, at time.Time) (bool, error) {
	tenantID := tenant.FromContextOrDefault(ctx).UUID()
	tsid, err := ResolveTenantSubjectID(ctx, r.db.Q(ctx), tenantID, userID)
	if err != nil {
		return false, err
	}
	return r.db.Q(ctx).NewSelect().
		Model((*models.ProductAccessGrant)(nil)).
		Where("pag.tenant_id = ?", tenantID).
		Where("pag.tenant_subject_id = ?", tsid).
		Where("pag.product_id = ?", productID).
		Where("pag.status = ?", models.ProductAccessStatusActive).
		Where("pag.revoked_at IS NULL").
		Where("pag.starts_at <= ?", at).
		Where("(pag.ends_at IS NULL OR pag.ends_at > ?)", at).
		Exists(ctx)
}

// ListActiveByUser returns the user's active, in-window grants, most recent first.
func (r *ProductAccessGrantRepo) ListActiveByUser(ctx context.Context, userID string, at time.Time) ([]models.ProductAccessGrant, error) {
	tenantID := tenant.FromContextOrDefault(ctx).UUID()
	tsid, err := ResolveTenantSubjectID(ctx, r.db.Q(ctx), tenantID, userID)
	if err != nil {
		return nil, err
	}
	grants := []models.ProductAccessGrant{}
	if err := r.db.Q(ctx).NewSelect().
		Model(&grants).
		Where("pag.tenant_id = ?", tenantID).
		Where("pag.tenant_subject_id = ?", tsid).
		Where("pag.status = ?", models.ProductAccessStatusActive).
		Where("pag.revoked_at IS NULL").
		Where("pag.starts_at <= ?", at).
		Where("(pag.ends_at IS NULL OR pag.ends_at > ?)", at).
		OrderExpr("pag.created_at DESC").
		Scan(ctx); err != nil {
		return nil, err
	}
	return grants, nil
}

// ListByUser returns ALL of the user's grants (active + revoked), most recent
// first. Used by admin views.
func (r *ProductAccessGrantRepo) ListByUser(ctx context.Context, userID string) ([]models.ProductAccessGrant, error) {
	tenantID := tenant.FromContextOrDefault(ctx).UUID()
	tsid, err := ResolveTenantSubjectID(ctx, r.db.Q(ctx), tenantID, userID)
	if err != nil {
		return nil, err
	}
	grants := []models.ProductAccessGrant{}
	if err := r.db.Q(ctx).NewSelect().
		Model(&grants).
		Where("pag.tenant_id = ?", tenantID).
		Where("pag.tenant_subject_id = ?", tsid).
		OrderExpr("pag.created_at DESC").
		Scan(ctx); err != nil {
		return nil, err
	}
	return grants, nil
}

// GetByID returns a single grant by id (tenant-scoped).
func (r *ProductAccessGrantRepo) GetByID(ctx context.Context, id uuid.UUID) (*models.ProductAccessGrant, error) {
	var grant models.ProductAccessGrant
	err := r.db.Q(ctx).NewSelect().
		Model(&grant).
		Where("pag.tenant_id = ?", tenant.FromContextOrDefault(ctx).UUID()).
		Where("pag.id = ?", id).
		Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return &grant, nil
}

// RevokeByID revokes a single active grant. Returns the number of rows affected
// so callers can detect "already revoked / not found".
func (r *ProductAccessGrantRepo) RevokeByID(ctx context.Context, id uuid.UUID, now time.Time, reason models.ProductAccessRevokeReason) (int64, error) {
	res, err := r.db.Q(ctx).NewUpdate().
		Model((*models.ProductAccessGrant)(nil)).
		Set("status = ?", models.ProductAccessStatusRevoked).
		Set("revoked_at = ?", now).
		Set("revoke_reason = ?", reason).
		Set("ends_at = ?", now).
		Set("updated_at = ?", now).
		Where("pag.tenant_id = ?", tenant.FromContextOrDefault(ctx).UUID()).
		Where("pag.id = ?", id).
		Where("pag.status = ?", models.ProductAccessStatusActive).
		Where("pag.revoked_at IS NULL").
		Exec(ctx)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// RevokeByPayment revokes all active grants tied to a payment. Used by the
// refund / chargeback reversal path. Returns rows affected.
func (r *ProductAccessGrantRepo) RevokeByPayment(ctx context.Context, paymentID uuid.UUID, now time.Time, reason models.ProductAccessRevokeReason) (int64, error) {
	res, err := r.db.Q(ctx).NewUpdate().
		Model((*models.ProductAccessGrant)(nil)).
		Set("status = ?", models.ProductAccessStatusRevoked).
		Set("revoked_at = ?", now).
		Set("revoke_reason = ?", reason).
		Set("ends_at = ?", now).
		Set("updated_at = ?", now).
		Where("pag.tenant_id = ?", tenant.FromContextOrDefault(ctx).UUID()).
		Where("pag.payment_id = ?", paymentID).
		Where("pag.status = ?", models.ProductAccessStatusActive).
		Where("pag.revoked_at IS NULL").
		Exec(ctx)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
