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
// test merchant. EnsureTestMerchant materializes the openrails.merchants row, and
// MerchantID()/WithTestMerchant pin it on the context so merchant.Require resolves and
// WithMerchantConn sets the app.merchant_id GUC.
var (
	// TestMerchantID is the canonical tenant id for integration fixtures.
	TestMerchantID = merchant.ID(uuid.MustParse("a5a5a5a5-0000-4000-8000-000000000001"))
	// TestMerchantSlug is its stable slug.
	TestMerchantSlug = "test"
)

// EnsureTestMerchant inserts the canonical test merchant (idempotent). qx should be
// a privileged/RLS-bypassing handle (the merchants table is control-plane).
func EnsureTestMerchant(ctx context.Context, t testing.TB, qx gen.DBTX) {
	t.Helper()
	_, err := qx.Exec(ctx,
		`INSERT INTO openrails.merchants (id, slug, status)
		 VALUES ($1, $2, 'active')
		 ON CONFLICT (slug) DO NOTHING`,
		TestMerchantID.UUID(), TestMerchantSlug)
	require.NoError(t, err, "ensure test merchant")
}

// WithTestMerchant returns a context pinned to the canonical test merchant, so
// merchant.Require resolves and tenant-conn helpers set the app.merchant_id GUC.
func WithTestMerchant(ctx context.Context) context.Context {
	return merchant.WithID(ctx, TestMerchantID)
}
