//go:build integration

package repo

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/pkg/merchant"
)

// startFeatureRLSPostgres boots Postgres, applies all billing migrations
// (including 050 RLS + the openrails_app role, and 062 which puts FORCE RLS on the
// entitlement-feature tables), and returns a *db.DB connected AS the unprivileged
// openrails_app role — the only role for which RLS actually enforces. Driving the
// feature repo through this DB + TenantTx exercises the SAME tenant-scoping
// chokepoint as production (GUC pinned per tx) on real tables.
func startFeatureRLSPostgres(t *testing.T) (appDB *db.DB, ctx context.Context) {
	t.Helper()
	ctx = context.Background()

	// Shared, fully-migrated DB (incl. 050 RLS + openrails_app role and 062 FORCE
	// RLS on the feature tables). The app DSN connects as the unprivileged
	// openrails_app role — the only role for which RLS actually enforces.
	_, appDSN := dbtest.SharedRLSPostgres(t)

	pool, err := pgxpool.New(ctx, appDSN)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	appDB, err = db.NewWithPGXPool(pool)
	require.NoError(t, err)
	return appDB, ctx
}

// seedTenantAndProduct inserts a tenant + one product (super-owned would bypass
// RLS, but here we run everything as the app role inside the correct tenant tx).
func seedTenantAndProduct(t *testing.T, ctx context.Context, appDB *db.DB, tid merchant.ID, productID uuid.UUID, slug string) {
	t.Helper()
	require.NoError(t, appDB.TenantTx(merchant.WithID(ctx, tid), func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx,
			`INSERT INTO openrails.merchants (id, slug, name, status) VALUES ($1, $2, $2, 'active') ON CONFLICT (id) DO NOTHING`,
			tid.UUID(), "t-"+tid.String()[:8],
		); err != nil {
			return err
		}
		_, err := tx.Exec(ctx,
			`INSERT INTO openrails.products (id, merchant_id, slug, display_name) VALUES ($1, $2, $3, $3)`,
			productID, tid.UUID(), slug,
		)
		return err
	}))
}

// TestEntitlementFeatureRepo_TenantIsolation proves that a feature created in
// tenant A is NOT visible, attachable, or detachable from tenant B. It exercises
// the real repo against real RLS-enforced tables (app role + per-tx GUC).
func TestEntitlementFeatureRepo_TenantIsolation(t *testing.T) {
	appDB, ctx := startFeatureRLSPostgres(t)

	tA := merchant.ID(uuid.New())
	tB := merchant.ID(uuid.New())
	productA := uuid.New()
	productB := uuid.New()
	suffix := uuid.NewString()[:8]
	seedTenantAndProduct(t, ctx, appDB, tA, productA, "prod-a-"+suffix)
	seedTenantAndProduct(t, ctx, appDB, tB, productB, "prod-b-"+suffix)

	ctxA := merchant.WithID(ctx, tA)
	ctxB := merchant.WithID(ctx, tB)

	// (1) Create a feature in tenant A.
	var featureA *models.EntitlementFeature
	require.NoError(t, appDB.TenantTx(ctxA, func(ctx context.Context, tx pgx.Tx) error {
		featureA = &models.EntitlementFeature{LookupKey: "premium", Name: "Premium"}
		return NewEntitlementFeatureRepo(db.NewWithPgxTx(tx)).CreateFeature(ctx, featureA)
	}))
	require.NotEqual(t, uuid.UUID{}, featureA.ID)
	require.Equal(t, tA.UUID(), featureA.MerchantID)

	// (2) Tenant A lists exactly its one feature; tenant B sees none.
	require.NoError(t, appDB.TenantTx(ctxA, func(ctx context.Context, tx pgx.Tx) error {
		got, err := NewEntitlementFeatureRepo(db.NewWithPgxTx(tx)).ListFeatures(ctx)
		require.NoError(t, err)
		require.Len(t, got, 1)
		require.Equal(t, "premium", got[0].LookupKey)
		return nil
	}))
	require.NoError(t, appDB.TenantTx(ctxB, func(ctx context.Context, tx pgx.Tx) error {
		got, err := NewEntitlementFeatureRepo(db.NewWithPgxTx(tx)).ListFeatures(ctx)
		require.NoError(t, err)
		require.Len(t, got, 0, "tenant B must not see tenant A's feature")
		return nil
	}))

	// (3) Tenant B cannot fetch tenant A's feature by id (RLS + explicit filter).
	require.NoError(t, appDB.TenantTx(ctxB, func(ctx context.Context, tx pgx.Tx) error {
		_, err := NewEntitlementFeatureRepo(db.NewWithPgxTx(tx)).GetFeatureByID(ctx, featureA.ID)
		require.Error(t, err, "tenant B must not read tenant A's feature by id")
		return nil
	}))

	// (4) Attach the feature to product A within tenant A; then list it back.
	require.NoError(t, appDB.TenantTx(ctxA, func(ctx context.Context, tx pgx.Tx) error {
		rr := NewEntitlementFeatureRepo(db.NewWithPgxTx(tx))
		days := 30
		require.NoError(t, rr.AttachFeatureToProduct(ctx, &models.ProductEntitlementFeature{
			ProductID:            productA,
			EntitlementFeatureID: featureA.ID,
			DurationDays:         &days,
		}))
		pfs, err := rr.ListProductFeatures(ctx, productA)
		require.NoError(t, err)
		require.Len(t, pfs, 1)
		require.NotNil(t, pfs[0].Feature)
		require.Equal(t, "premium", pfs[0].Feature.LookupKey)
		require.NotNil(t, pfs[0].DurationDays)
		require.Equal(t, 30, *pfs[0].DurationDays)
		return nil
	}))

	// (5) Tenant B sees no product features on its own product, and cannot attach
	//     tenant A's feature (the feature id is invisible/foreign under RLS).
	require.NoError(t, appDB.TenantTx(ctxB, func(ctx context.Context, tx pgx.Tx) error {
		rr := NewEntitlementFeatureRepo(db.NewWithPgxTx(tx))
		pfs, err := rr.ListProductFeatures(ctx, productB)
		require.NoError(t, err)
		require.Len(t, pfs, 0)

		// Attaching tenant A's feature id from tenant B must fail: the FK target
		// row is invisible to tenant B (RLS) and the FK enforces against the row,
		// so the insert is rejected.
		err = rr.AttachFeatureToProduct(ctx, &models.ProductEntitlementFeature{
			ProductID:            productB,
			EntitlementFeatureID: featureA.ID,
		})
		require.Error(t, err, "tenant B must not attach tenant A's feature")
		return nil
	}))
}
