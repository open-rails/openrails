package repo

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/pkg/tenant"
)

// EntitlementFeatureRepo persists Stripe-shaped feature definitions and product
// feature attachments (issue #245).
//
// Tenant isolation is enforced two ways, matching the existing entitlement repo
// in this package: (1) every query filters explicitly on tenant_id from the
// request context (tenant.Require), and (2) migration 062 puts FORCE
// ROW LEVEL SECURITY on both tables so that — when the app connects as the
// unprivileged openrails_app role inside a tenant-scoped transaction (the
// app.tenant_id GUC set via TenantTx) — Postgres rejects any cross-tenant row
// regardless of the WHERE clause (issue #227 defense-in-depth).
type EntitlementFeatureRepo struct {
	db *db.DB
}

func NewEntitlementFeatureRepo(d *db.DB) *EntitlementFeatureRepo {
	return &EntitlementFeatureRepo{db: d}
}

func (r *EntitlementFeatureRepo) tenantID(ctx context.Context) (uuid.UUID, error) {
	tid, err := tenant.Require(ctx)
	if err != nil {
		return uuid.UUID{}, err
	}
	return tid.UUID(), nil
}

func entitlementFeatureFromGen(f gen.BillingEntitlementFeature) (*models.EntitlementFeature, error) {
	m := &models.EntitlementFeature{
		ID:        f.ID,
		TenantID:  f.TenantID,
		LookupKey: f.LookupKey,
		Name:      f.Name,
		Active:    f.Active,
		CreatedAt: f.CreatedAt,
		UpdatedAt: f.UpdatedAt,
	}
	if err := fromJSONB(f.Metadata, &m.Metadata, "entitlement_features.metadata"); err != nil {
		return nil, err
	}
	return m, nil
}

func productEntitlementFeatureFromGen(p gen.BillingProductEntitlementFeature) (*models.ProductEntitlementFeature, error) {
	m := &models.ProductEntitlementFeature{
		ID:                   p.ID,
		TenantID:             p.TenantID,
		ProductID:            p.ProductID,
		EntitlementFeatureID: p.EntitlementFeatureID,
		DurationDays:         derefIntPtr(p.DurationDays),
		CreatedAt:            p.CreatedAt,
		UpdatedAt:            p.UpdatedAt,
	}
	if err := fromJSONB(p.Metadata, &m.Metadata, "product_entitlement_features.metadata"); err != nil {
		return nil, err
	}
	return m, nil
}

// CreateFeature inserts a new entitlement feature, stamping the request tenant.
func (r *EntitlementFeatureRepo) CreateFeature(ctx context.Context, f *models.EntitlementFeature) error {
	if (f.TenantID == uuid.UUID{}) {
		tid, err := r.tenantID(ctx)
		if err != nil {
			return err
		}
		f.TenantID = tid
	}
	meta, err := toJSONB(f.Metadata)
	if err != nil {
		return err
	}
	id, err := r.db.Gen(ctx).CreateEntitlementFeature(ctx, gen.CreateEntitlementFeatureParams{
		ID:        f.ID,
		TenantID:  f.TenantID,
		LookupKey: f.LookupKey,
		Name:      f.Name,
		Active:    f.Active,
		Metadata:  meta,
		CreatedAt: f.CreatedAt,
		UpdatedAt: f.UpdatedAt,
	})
	if err != nil {
		return err
	}
	f.ID = id
	return nil
}

// UpdateFeature updates a feature's mutable fields (name, active, metadata).
func (r *EntitlementFeatureRepo) UpdateFeature(ctx context.Context, f *models.EntitlementFeature) error {
	meta, err := toJSONB(f.Metadata)
	if err != nil {
		return err
	}
	tid, err := r.tenantID(ctx)
	if err != nil {
		return err
	}
	rows, err := r.db.Gen(ctx).UpdateEntitlementFeature(ctx, gen.UpdateEntitlementFeatureParams{
		ID:        f.ID,
		Name:      f.Name,
		Active:    f.Active,
		Metadata:  meta,
		UpdatedAt: updateTimestamp(f.UpdatedAt),
		TenantID:  tid,
	})
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
	tid, err := r.tenantID(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := r.db.Gen(ctx).ListEntitlementFeatures(ctx, tid)
	if err != nil {
		return nil, err
	}
	out := make([]models.EntitlementFeature, 0, len(rows))
	for _, row := range rows {
		f, err := entitlementFeatureFromGen(row)
		if err != nil {
			return nil, err
		}
		out = append(out, *f)
	}
	return out, nil
}

// GetFeatureByID returns a single feature by id within the request tenant.
func (r *EntitlementFeatureRepo) GetFeatureByID(ctx context.Context, id uuid.UUID) (*models.EntitlementFeature, error) {
	tid, err := r.tenantID(ctx)
	if err != nil {
		return nil, err
	}
	row, err := r.db.Gen(ctx).GetEntitlementFeatureByID(ctx, gen.GetEntitlementFeatureByIDParams{
		ID:       id,
		TenantID: tid,
	})
	if err != nil {
		return nil, err
	}
	return entitlementFeatureFromGen(row)
}

// GetFeatureByLookupKey returns a single feature by its tenant-unique lookup_key.
func (r *EntitlementFeatureRepo) GetFeatureByLookupKey(ctx context.Context, lookupKey string) (*models.EntitlementFeature, error) {
	tid, err := r.tenantID(ctx)
	if err != nil {
		return nil, err
	}
	row, err := r.db.Gen(ctx).GetEntitlementFeatureByLookupKey(ctx, gen.GetEntitlementFeatureByLookupKeyParams{
		LookupKey: lookupKey,
		TenantID:  tid,
	})
	if err != nil {
		return nil, err
	}
	return entitlementFeatureFromGen(row)
}

// ListFeaturesByLookupKeys returns features whose lookup_key is in keys, scoped to
// the request tenant.
func (r *EntitlementFeatureRepo) ListFeaturesByLookupKeys(ctx context.Context, keys []string) ([]models.EntitlementFeature, error) {
	out := []models.EntitlementFeature{}
	if len(keys) == 0 {
		return out, nil
	}
	tid, err := r.tenantID(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := r.db.Gen(ctx).ListEntitlementFeaturesByLookupKeys(ctx, gen.ListEntitlementFeaturesByLookupKeysParams{
		TenantID:   tid,
		LookupKeys: keys,
	})
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		f, err := entitlementFeatureFromGen(row)
		if err != nil {
			return nil, err
		}
		out = append(out, *f)
	}
	return out, nil
}

// AttachFeatureToProduct creates a product_feature attachment.
func (r *EntitlementFeatureRepo) AttachFeatureToProduct(ctx context.Context, pef *models.ProductEntitlementFeature) error {
	if (pef.TenantID == uuid.UUID{}) {
		tid, err := r.tenantID(ctx)
		if err != nil {
			return err
		}
		pef.TenantID = tid
	}
	// Same-tenant guard: Postgres FK checks BYPASS RLS, so without this an attacker
	// who knows another tenant's feature UUID could attach it. Verify the feature is
	// visible under the CURRENT tenant's RLS/tenant scope before inserting.
	if _, err := r.GetFeatureByID(ctx, pef.EntitlementFeatureID); err != nil {
		return errors.New("entitlement feature not found in tenant")
	}
	meta, err := toJSONB(pef.Metadata)
	if err != nil {
		return err
	}
	id, err := r.db.Gen(ctx).CreateProductEntitlementFeature(ctx, gen.CreateProductEntitlementFeatureParams{
		ID:                   pef.ID,
		TenantID:             pef.TenantID,
		ProductID:            pef.ProductID,
		EntitlementFeatureID: pef.EntitlementFeatureID,
		DurationDays:         intPtrTo32(pef.DurationDays),
		Metadata:             meta,
		CreatedAt:            pef.CreatedAt,
		UpdatedAt:            pef.UpdatedAt,
	})
	if err != nil {
		return err
	}
	pef.ID = id
	return nil
}

// DetachFeature removes a single product_feature attachment by id, scoped to the
// request tenant.
func (r *EntitlementFeatureRepo) DetachFeature(ctx context.Context, productFeatureID uuid.UUID) error {
	tid, err := r.tenantID(ctx)
	if err != nil {
		return err
	}
	rows, err := r.db.Gen(ctx).DeleteProductEntitlementFeature(ctx, gen.DeleteProductEntitlementFeatureParams{
		ID:       productFeatureID,
		TenantID: tid,
	})
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
	tid, err := r.tenantID(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := r.db.Gen(ctx).ListProductEntitlementFeatures(ctx, gen.ListProductEntitlementFeaturesParams{
		ProductID: productID,
		TenantID:  tid,
	})
	if err != nil {
		return nil, err
	}
	out := make([]models.ProductEntitlementFeature, 0, len(rows))
	for _, row := range rows {
		pef, err := productEntitlementFeatureFromGen(row.BillingProductEntitlementFeature)
		if err != nil {
			return nil, err
		}
		feature, err := entitlementFeatureFromGen(row.BillingEntitlementFeature)
		if err != nil {
			return nil, err
		}
		pef.Feature = feature
		out = append(out, *pef)
	}
	return out, nil
}

// GetProductFeatureByID returns a single attachment by id within the request tenant.
func (r *EntitlementFeatureRepo) GetProductFeatureByID(ctx context.Context, id uuid.UUID) (*models.ProductEntitlementFeature, error) {
	tid, err := r.tenantID(ctx)
	if err != nil {
		return nil, err
	}
	row, err := r.db.Gen(ctx).GetProductEntitlementFeatureByID(ctx, gen.GetProductEntitlementFeatureByIDParams{
		ID:       id,
		TenantID: tid,
	})
	if err != nil {
		return nil, err
	}
	return productEntitlementFeatureFromGen(row)
}
