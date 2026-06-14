package dbtest

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/pkg/merchant"
)

// There is no "default tenant" (#336). Integration fixtures that insert
// tenant-owned rows must run under an explicit tenant; tests use this canonical
// test tenant. EnsureTestTenant materializes the openrails.merchants row, and
// MerchantID()/WithTestTenant pin it on the context so merchant.Require resolves and
// WithTenantConn sets the app.merchant_id GUC.
var (
	// TestTenantID is the canonical tenant id for integration fixtures.
	TestTenantID = merchant.ID(uuid.MustParse("a5a5a5a5-0000-4000-8000-000000000001"))
	// TestTenantSlug is its stable slug.
	TestTenantSlug = "test"
)

// EnsureTestTenant inserts the canonical test tenant (idempotent). qx should be
// a privileged/RLS-bypassing handle (the merchants table is control-plane).
func EnsureTestTenant(ctx context.Context, t testing.TB, qx gen.DBTX) {
	t.Helper()
	_, err := qx.Exec(ctx,
		`INSERT INTO openrails.merchants (id, slug, name, status)
		 VALUES ($1, $2, 'Test Tenant', 'active')
		 ON CONFLICT (slug) DO NOTHING`,
		TestTenantID.UUID(), TestTenantSlug)
	require.NoError(t, err, "ensure test tenant")
}

// WithTestTenant returns a context pinned to the canonical test tenant, so
// merchant.Require resolves and tenant-conn helpers set the app.merchant_id GUC.
func WithTestTenant(ctx context.Context) context.Context {
	return merchant.WithID(ctx, TestTenantID)
}
