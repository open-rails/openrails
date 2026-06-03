package repo

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/pkg/tenant"
	"github.com/uptrace/bun"
)

// EntitlementFeatureRepo persists Stripe-shaped feature definitions and product
// feature attachments (issue #245).
//
// Tenant isolation is enforced two ways, matching the existing entitlement repo
// in this package: (1) every query filters explicitly on tenant_id from the
// request context (tenant.FromContextOrDefault), and (2) migration 062 puts FORCE
// ROW LEVEL SECURITY on both tables so that — when the app connects as the
// unprivileged openrails_app role inside a tenant-scoped transaction (the
// app.tenant_id GUC set via RunInTenantTx) — Postgres rejects any cross-tenant row
// regardless of the WHERE clause (issue #227 defense-in-depth).
type EntitlementFeatureRepo struct {
	db *db.DB
}

func NewEntitlementFeatureRepo(d *db.DB) *EntitlementFeatureRepo {
	return &EntitlementFeatureRepo{db: d}
}

func (r *EntitlementFeatureRepo) tenantID(ctx context.Context) uuid.UUID {
	return tenant.FromContextOrDefault(ctx).UUID()
}

// CreateFeature inserts a new entitlement feature, stamping the request tenant.
func (r *EntitlementFeatureRepo) CreateFeature(ctx context.Context, f *models.EntitlementFeature) error {
	if (f.TenantID == uuid.UUID{}) {
		f.TenantID = r.tenantID(ctx)
	}
	res, err := r.db.Q(ctx).NewInsert().Model(f).Exec(ctx)
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

// UpdateFeature updates a feature's mutable fields (name, active, metadata).
func (r *EntitlementFeatureRepo) UpdateFeature(ctx context.Context, f *models.EntitlementFeature) error {
	res, err := r.db.Q(ctx).NewUpdate().
		Model(f).
		Column("name", "active", "metadata", "updated_at").
		WherePK().
		Where("ef.tenant_id = ?", r.tenantID(ctx)).
		Exec(ctx)
	if err != nil {
		return err
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if rows < 1 {
		return errors.New("entitlement feature not found")
	}
	return nil
}

// ListFeatures returns all features for the request tenant, newest first.
func (r *EntitlementFeatureRepo) ListFeatures(ctx context.Context) ([]models.EntitlementFeature, error) {
	out := []models.EntitlementFeature{}
	if err := r.db.Q(ctx).NewSelect().
		Model(&out).
		Where("ef.tenant_id = ?", r.tenantID(ctx)).
		OrderExpr("ef.created_at DESC").
		Scan(ctx); err != nil {
		return nil, err
	}
	return out, nil
}

// GetFeatureByID returns a single feature by id within the request tenant.
func (r *EntitlementFeatureRepo) GetFeatureByID(ctx context.Context, id uuid.UUID) (*models.EntitlementFeature, error) {
	f := new(models.EntitlementFeature)
	if err := r.db.Q(ctx).NewSelect().
		Model(f).
		Where("ef.id = ?", id).
		Where("ef.tenant_id = ?", r.tenantID(ctx)).
		Scan(ctx); err != nil {
		return nil, err
	}
	return f, nil
}

// GetFeatureByLookupKey returns a single feature by its tenant-unique lookup_key.
func (r *EntitlementFeatureRepo) GetFeatureByLookupKey(ctx context.Context, lookupKey string) (*models.EntitlementFeature, error) {
	f := new(models.EntitlementFeature)
	if err := r.db.Q(ctx).NewSelect().
		Model(f).
		Where("ef.lookup_key = ?", lookupKey).
		Where("ef.tenant_id = ?", r.tenantID(ctx)).
		Scan(ctx); err != nil {
		return nil, err
	}
	return f, nil
}

// ListFeaturesByLookupKeys returns features whose lookup_key is in keys, scoped to
// the request tenant.
func (r *EntitlementFeatureRepo) ListFeaturesByLookupKeys(ctx context.Context, keys []string) ([]models.EntitlementFeature, error) {
	out := []models.EntitlementFeature{}
	if len(keys) == 0 {
		return out, nil
	}
	if err := r.db.Q(ctx).NewSelect().
		Model(&out).
		Where("ef.tenant_id = ?", r.tenantID(ctx)).
		Where("ef.lookup_key IN (?)", bun.In(keys)).
		Scan(ctx); err != nil {
		return nil, err
	}
	return out, nil
}

// AttachFeatureToProduct creates a product_feature attachment.
func (r *EntitlementFeatureRepo) AttachFeatureToProduct(ctx context.Context, pef *models.ProductEntitlementFeature) error {
	if (pef.TenantID == uuid.UUID{}) {
		pef.TenantID = r.tenantID(ctx)
	}
	// Same-tenant guard: Postgres FK checks BYPASS RLS, so without this an attacker
	// who knows another tenant's feature UUID could attach it. Verify the feature is
	// visible under the CURRENT tenant's RLS/tenant scope before inserting.
	if _, err := r.GetFeatureByID(ctx, pef.EntitlementFeatureID); err != nil {
		return errors.New("entitlement feature not found in tenant")
	}
	res, err := r.db.Q(ctx).NewInsert().Model(pef).Exec(ctx)
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

// DetachFeature removes a single product_feature attachment by id, scoped to the
// request tenant.
func (r *EntitlementFeatureRepo) DetachFeature(ctx context.Context, productFeatureID uuid.UUID) error {
	res, err := r.db.Q(ctx).NewDelete().
		Model((*models.ProductEntitlementFeature)(nil)).
		Where("pef.id = ?", productFeatureID).
		Where("pef.tenant_id = ?", r.tenantID(ctx)).
		Exec(ctx)
	if err != nil {
		return err
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if rows < 1 {
		return errors.New("product feature attachment not found")
	}
	return nil
}

// ListProductFeatures returns the feature attachments for a product (with the
// joined feature definition), scoped to the request tenant.
func (r *EntitlementFeatureRepo) ListProductFeatures(ctx context.Context, productID uuid.UUID) ([]models.ProductEntitlementFeature, error) {
	out := []models.ProductEntitlementFeature{}
	if err := r.db.Q(ctx).NewSelect().
		Model(&out).
		Relation("Feature").
		Where("pef.product_id = ?", productID).
		Where("pef.tenant_id = ?", r.tenantID(ctx)).
		OrderExpr("pef.created_at ASC").
		Scan(ctx); err != nil {
		return nil, err
	}
	return out, nil
}

// GetProductFeatureByID returns a single attachment by id within the request tenant.
func (r *EntitlementFeatureRepo) GetProductFeatureByID(ctx context.Context, id uuid.UUID) (*models.ProductEntitlementFeature, error) {
	pef := new(models.ProductEntitlementFeature)
	if err := r.db.Q(ctx).NewSelect().
		Model(pef).
		Where("pef.id = ?", id).
		Where("pef.tenant_id = ?", r.tenantID(ctx)).
		Scan(ctx); err != nil {
		return nil, err
	}
	return pef, nil
}
