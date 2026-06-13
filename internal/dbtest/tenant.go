package dbtest

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/pkg/tenant"
)

// There is no "default tenant" (#336). Integration fixtures that insert
// tenant-owned rows must run under an explicit tenant; tests use this canonical
// test tenant. EnsureTestTenant materializes the openrails.tenants row, and
// TenantID()/WithTestTenant pin it on the context so tenant.Require resolves and
// WithTenantConn sets the app.tenant_id GUC.
var (
	// TestTenantID is the canonical tenant id for integration fixtures.
	TestTenantID = tenant.ID(uuid.MustParse("a5a5a5a5-0000-4000-8000-000000000001"))
	// TestTenantSlug is its stable slug.
	TestTenantSlug = "test"
)

// EnsureTestTenant inserts the canonical test tenant (idempotent). qx should be
// a privileged/RLS-bypassing handle (the tenants table is control-plane).
func EnsureTestTenant(ctx context.Context, t testing.TB, qx gen.DBTX) {
	t.Helper()
	_, err := qx.Exec(ctx,
		`INSERT INTO openrails.tenants (id, slug, name, status)
		 VALUES ($1, $2, 'Test Tenant', 'active')
		 ON CONFLICT (slug) DO NOTHING`,
		TestTenantID.UUID(), TestTenantSlug)
	require.NoError(t, err, "ensure test tenant")
}

// WithTestTenant returns a context pinned to the canonical test tenant, so
// tenant.Require resolves and tenant-conn helpers set the app.tenant_id GUC.
func WithTestTenant(ctx context.Context) context.Context {
	return tenant.WithID(ctx, TestTenantID)
}
