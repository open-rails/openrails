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
	"github.com/open-rails/openrails/pkg/merchant"
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

func productAccessGrantFromGen(g gen.OpenrailsProductAccessGrant) *models.ProductAccessGrant {
	m := &models.ProductAccessGrant{
		ID:              g.ID,
		MerchantID:        g.MerchantID,
		MerchantSubjectID: g.MerchantSubjectID,
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
	if (grant.MerchantID == uuid.UUID{}) {
		tid, err := merchant.Require(ctx)
		if err != nil {
			return err
		}
		grant.MerchantID = tid.UUID()
	}
	if err := ensureMerchantSubjectRow(ctx, r.db.Qx(ctx), grant.MerchantID, grant.MerchantSubjectID); err != nil {
		return err
	}
	var revokeReason *string
	if grant.RevokeReason != nil {
		rr := string(*grant.RevokeReason)
		revokeReason = &rr
	}
	id, err := r.db.Gen(ctx).CreateProductAccessGrant(ctx, gen.CreateProductAccessGrantParams{
		ID:              grant.ID,
		MerchantID:        grant.MerchantID,
		MerchantSubjectID: grant.MerchantSubjectID,
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
	tid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	tenantID := tid.UUID()
	tsid, err := ResolveMerchantSubjectID(userID)
	if err != nil {
		return nil, err
	}
	row, err := r.db.Gen(ctx).GetProductAccessGrantBySource(ctx, gen.GetProductAccessGrantBySourceParams{
		MerchantID:        tenantID,
		MerchantSubjectID: tsid,
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
	tid, err := merchant.Require(ctx)
	if err != nil {
		return false, err
	}
	tenantID := tid.UUID()
	tsid, err := ResolveMerchantSubjectID(userID)
	if err != nil {
		return false, err
	}
	return r.db.Gen(ctx).HasActiveProductAccess(ctx, gen.HasActiveProductAccessParams{
		MerchantID:        tenantID,
		MerchantSubjectID: tsid,
		ProductID:       productID,
		At:              at,
	})
}

// ListActiveByUser returns the user's active, in-window grants, most recent first.
func (r *ProductAccessGrantRepo) ListActiveByUser(ctx context.Context, userID string, at time.Time) ([]models.ProductAccessGrant, error) {
	tid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	tenantID := tid.UUID()
	tsid, err := ResolveMerchantSubjectID(userID)
	if err != nil {
		return nil, err
	}
	rows, err := r.db.Gen(ctx).ListActiveProductAccessGrantsByMerchantSubject(ctx, gen.ListActiveProductAccessGrantsByMerchantSubjectParams{
		MerchantID:        tenantID,
		MerchantSubjectID: tsid,
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
	tid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	tenantID := tid.UUID()
	tsid, err := ResolveMerchantSubjectID(userID)
	if err != nil {
		return nil, err
	}
	rows, err := r.db.Gen(ctx).ListProductAccessGrantsByMerchantSubject(ctx, gen.ListProductAccessGrantsByMerchantSubjectParams{
		MerchantID:        tenantID,
		MerchantSubjectID: tsid,
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
	tid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	row, err := r.db.Gen(ctx).GetProductAccessGrantByID(ctx, gen.GetProductAccessGrantByIDParams{
		MerchantID: tid.UUID(),
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
	tid, err := merchant.Require(ctx)
	if err != nil {
		return 0, err
	}
	return r.db.Gen(ctx).RevokeProductAccessGrantByID(ctx, gen.RevokeProductAccessGrantByIDParams{
		MerchantID: tid.UUID(),
		ID:       id,
		Now:      now,
		Reason:   &rr,
	})
}

// RevokeByPayment revokes all active grants tied to a payment. Used by the
// refund / chargeback reversal path. Returns rows affected.
func (r *ProductAccessGrantRepo) RevokeByPayment(ctx context.Context, paymentID uuid.UUID, now time.Time, reason models.ProductAccessRevokeReason) (int64, error) {
	rr := string(reason)
	tid, err := merchant.Require(ctx)
	if err != nil {
		return 0, err
	}
	return r.db.Gen(ctx).RevokeProductAccessGrantsByPayment(ctx, gen.RevokeProductAccessGrantsByPaymentParams{
		MerchantID:  tid.UUID(),
		PaymentID: &paymentID,
		Now:       now,
		Reason:    &rr,
	})
}
