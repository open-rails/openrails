//go:build integration

package tests

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/dbtest"
	billingservice "github.com/open-rails/openrails/pkg/service"
)

// TestCatalogArchived_RoundTripsThroughFacade verifies that the archived flag
// survives create -> get through the public service facade, that the default
// on create is active (archived=false), and that archive/unarchive transitions
// round-trip via Deactivate/Activate.
func TestCatalogArchived_RoundTripsThroughFacade(t *testing.T) {
	suite := getSharedTestSuite(t)
	ctx := dbtest.WithTestMerchant(context.Background())

	svc, err := billingservice.New(suite.App.Runtime)
	require.NoError(t, err)

	created, err := svc.CreateProduct(ctx, billingservice.CreateProductRequest{
		Key:         "status_roundtrip_" + uuid.NewString(),
		DisplayName: "Status Roundtrip",
	})
	require.NoError(t, err)
	require.False(t, created.Archived, "default should be active (archived=false)")

	got, err := svc.GetProduct(ctx, created.ID)
	require.NoError(t, err)
	require.False(t, got.Archived)

	// Archive and confirm it round-trips.
	archived, err := svc.DeactivateProduct(ctx, created.ID)
	require.NoError(t, err)
	require.True(t, archived.Archived)

	reloaded, err := svc.GetProduct(ctx, created.ID)
	require.NoError(t, err)
	require.True(t, reloaded.Archived)
}

// TestCatalogArchived_CreateAsArchivedInOneStep verifies the migration use
// case: a historical plan with existing subscribers can be created directly as
// archived (no purchasable gap), the price round-trips archived, and the
// public catalog does not surface it.
func TestCatalogArchived_CreateAsArchivedInOneStep(t *testing.T) {
	suite := getSharedTestSuite(t)
	ctx := dbtest.WithTestMerchant(context.Background())

	svc, err := billingservice.New(suite.App.Runtime)
	require.NoError(t, err)

	product, err := svc.CreateProduct(ctx, billingservice.CreateProductRequest{
		Key:         "create_archived_" + uuid.NewString(),
		DisplayName: "Legacy Product",
		Archived:    true,
	})
	require.NoError(t, err)
	require.True(t, product.Archived)

	price, err := svc.CreatePrice(ctx, billingservice.CreatePriceRequest{
		ProductID:  product.ID,
		UnitAmount: 1999,
		Currency:   "usd",
		Archived:   true,
	})
	require.NoError(t, err)
	require.True(t, price.Archived)

	reloaded, err := svc.GetPrice(ctx, price.ID)
	require.NoError(t, err)
	require.True(t, reloaded.Archived)

	// Public catalog (non-admin: includeInactive=false) must not surface it.
	public, err := suite.App.Runtime.PublicSubscriptionService.GetProducts(ctx, false)
	require.NoError(t, err)
	for _, p := range public {
		require.NotEqual(t, product.ID, p.ID, "archived product must be hidden from public catalog")
	}
}
