package repo

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/pkg/tenant"
)

// ProductAccessGrantRepo persists durable product ownership/access grants
// (issue #250). All tenant-owned queries go through the RLS-aware accessor
// (#227) and stamp/scope the resolved tenant so single-tenant and
// multi-tenant runs behave identically.
type ProductAccessGrantRepo struct {
	db *db.DB
}

func NewProductAccessGrantRepo(d *db.DB) *ProductAccessGrantRepo {
	return &ProductAccessGrantRepo{db: d}
}

func productAccessGrantFromGen(g gen.BillingProductAccessGrant) *models.ProductAccessGrant {
	m := &models.ProductAccessGrant{
		ID:              g.ID,
		TenantID:        g.TenantID,
		TenantSubjectID: g.TenantSubjectID,
		ProductID:       g.ProductID,
		SourceType:      models.ProductAccessSourceType(g.SourceType),
		SourceID:        g.SourceID,
		PaymentID:       g.PaymentID,
		Status:          models.ProductAccessStatus(g.Status),
		StartsAt:        g.StartsAt,
		EndsAt:          g.EndsAt,
		RevokedAt:       g.RevokedAt,
		CreatedAt:       g.CreatedAt,
		UpdatedAt:       g.UpdatedAt,
	}
	if g.RevokeReason != nil {
		rr := models.ProductAccessRevokeReason(*g.RevokeReason)
		m.RevokeReason = &rr
	}
	return m
}

// Insert creates a grant, stamping the resolved tenant when the caller left it
// zero (consistent with reads).
func (r *ProductAccessGrantRepo) Insert(ctx context.Context, grant *models.ProductAccessGrant) error {
	if (grant.TenantID == uuid.UUID{}) {
		grant.TenantID = tenant.FromContextOrDefault(ctx).UUID()
	}
	if err := ensureTenantSubjectRow(ctx, r.db.Qx(ctx), grant.TenantID, grant.TenantSubjectID); err != nil {
		return err
	}
	var revokeReason *string
	if grant.RevokeReason != nil {
		rr := string(*grant.RevokeReason)
		revokeReason = &rr
	}
	id, err := r.db.Gen(ctx).CreateProductAccessGrant(ctx, gen.CreateProductAccessGrantParams{
		ID:              grant.ID,
		TenantID:        grant.TenantID,
		TenantSubjectID: grant.TenantSubjectID,
		ProductID:       grant.ProductID,
		SourceType:      string(grant.SourceType),
		SourceID:        grant.SourceID,
		PaymentID:       grant.PaymentID,
		Status:          string(grant.Status),
		StartsAt:        grant.StartsAt,
		EndsAt:          grant.EndsAt,
		RevokedAt:       grant.RevokedAt,
		RevokeReason:    revokeReason,
		CreatedAt:       grant.CreatedAt,
		UpdatedAt:       grant.UpdatedAt,
	})
	if err != nil {
		return err
	}
	grant.ID = id
	return nil
}

// GetBySource returns the existing grant for a (user, product, source)
// idempotency key, or nil if none. Used to make granting idempotent.
func (r *ProductAccessGrantRepo) GetBySource(ctx context.Context, userID string, productID uuid.UUID, sourceID string) (*models.ProductAccessGrant, error) {
	tenantID := tenant.FromContextOrDefault(ctx).UUID()
	tsid, err := ResolveTenantSubjectID(ctx, r.db.Qx(ctx), tenantID, userID)
	if err != nil {
		return nil, err
	}
	row, err := r.db.Gen(ctx).GetProductAccessGrantBySource(ctx, gen.GetProductAccessGrantBySourceParams{
		TenantID:        tenantID,
		TenantSubjectID: tsid,
		ProductID:       productID,
		SourceID:        sourceID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return productAccessGrantFromGen(row), nil
}

// HasActiveAccess reports whether the user has an active, in-window grant for the
// product at time t.
func (r *ProductAccessGrantRepo) HasActiveAccess(ctx context.Context, userID string, productID uuid.UUID, at time.Time) (bool, error) {
	tenantID := tenant.FromContextOrDefault(ctx).UUID()
	tsid, err := ResolveTenantSubjectID(ctx, r.db.Qx(ctx), tenantID, userID)
	if err != nil {
		return false, err
	}
	return r.db.Gen(ctx).HasActiveProductAccess(ctx, gen.HasActiveProductAccessParams{
		TenantID:        tenantID,
		TenantSubjectID: tsid,
		ProductID:       productID,
		At:              at,
	})
}

// ListActiveByUser returns the user's active, in-window grants, most recent first.
func (r *ProductAccessGrantRepo) ListActiveByUser(ctx context.Context, userID string, at time.Time) ([]models.ProductAccessGrant, error) {
	tenantID := tenant.FromContextOrDefault(ctx).UUID()
	tsid, err := ResolveTenantSubjectID(ctx, r.db.Qx(ctx), tenantID, userID)
	if err != nil {
		return nil, err
	}
	rows, err := r.db.Gen(ctx).ListActiveProductAccessGrantsByTenantSubject(ctx, gen.ListActiveProductAccessGrantsByTenantSubjectParams{
		TenantID:        tenantID,
		TenantSubjectID: tsid,
		At:              at,
	})
	if err != nil {
		return nil, err
	}
	grants := make([]models.ProductAccessGrant, 0, len(rows))
	for _, row := range rows {
		grants = append(grants, *productAccessGrantFromGen(row))
	}
	return grants, nil
}

// ListByUser returns ALL of the user's grants (active + revoked), most recent
// first. Used by admin views.
func (r *ProductAccessGrantRepo) ListByUser(ctx context.Context, userID string) ([]models.ProductAccessGrant, error) {
	tenantID := tenant.FromContextOrDefault(ctx).UUID()
	tsid, err := ResolveTenantSubjectID(ctx, r.db.Qx(ctx), tenantID, userID)
	if err != nil {
		return nil, err
	}
	rows, err := r.db.Gen(ctx).ListProductAccessGrantsByTenantSubject(ctx, gen.ListProductAccessGrantsByTenantSubjectParams{
		TenantID:        tenantID,
		TenantSubjectID: tsid,
	})
	if err != nil {
		return nil, err
	}
	grants := make([]models.ProductAccessGrant, 0, len(rows))
	for _, row := range rows {
		grants = append(grants, *productAccessGrantFromGen(row))
	}
	return grants, nil
}

// GetByID returns a single grant by id (tenant-scoped).
func (r *ProductAccessGrantRepo) GetByID(ctx context.Context, id uuid.UUID) (*models.ProductAccessGrant, error) {
	row, err := r.db.Gen(ctx).GetProductAccessGrantByID(ctx, gen.GetProductAccessGrantByIDParams{
		TenantID: tenant.FromContextOrDefault(ctx).UUID(),
		ID:       id,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return productAccessGrantFromGen(row), nil
}

// RevokeByID revokes a single active grant. Returns the number of rows affected
// so callers can detect "already revoked / not found".
func (r *ProductAccessGrantRepo) RevokeByID(ctx context.Context, id uuid.UUID, now time.Time, reason models.ProductAccessRevokeReason) (int64, error) {
	rr := string(reason)
	return r.db.Gen(ctx).RevokeProductAccessGrantByID(ctx, gen.RevokeProductAccessGrantByIDParams{
		TenantID: tenant.FromContextOrDefault(ctx).UUID(),
		ID:       id,
		Now:      now,
		Reason:   &rr,
	})
}

// RevokeByPayment revokes all active grants tied to a payment. Used by the
// refund / chargeback reversal path. Returns rows affected.
func (r *ProductAccessGrantRepo) RevokeByPayment(ctx context.Context, paymentID uuid.UUID, now time.Time, reason models.ProductAccessRevokeReason) (int64, error) {
	rr := string(reason)
	return r.db.Gen(ctx).RevokeProductAccessGrantsByPayment(ctx, gen.RevokeProductAccessGrantsByPaymentParams{
		TenantID:  tenant.FromContextOrDefault(ctx).UUID(),
		PaymentID: &paymentID,
		Now:       now,
		Reason:    &rr,
	})
}
