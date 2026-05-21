//go:build integration

package tests

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db/models"
	billingservice "github.com/open-rails/openrails/pkg/service"
)

// TestCatalogStatus_RoundTripsThroughFacade verifies that the status enum
// survives create -> get -> list through the public service facade, and that
// the default on create is "active".
func TestCatalogStatus_RoundTripsThroughFacade(t *testing.T) {
	suite := setupTestSuite(t)
	ctx := context.Background()

	svc, err := billingservice.New(suite.App.Runtime)
	require.NoError(t, err)

	created, err := svc.CreateProduct(ctx, billingservice.CreateProductRequest{
		Slug:        "status_roundtrip_" + uuid.NewString(),
		DisplayName: "Status Roundtrip",
	})
	require.NoError(t, err)
	require.Equal(t, models.CatalogStatusActive, created.Status, "default status should be active")

	got, err := svc.GetProduct(ctx, created.ID)
	require.NoError(t, err)
	require.Equal(t, models.CatalogStatusActive, got.Status)

	// Transition to archived and confirm it round-trips.
	archived, err := svc.SetProductStatus(ctx, created.ID, models.CatalogStatusArchived)
	require.NoError(t, err)
	require.Equal(t, models.CatalogStatusArchived, archived.Status)

	reloaded, err := svc.GetProduct(ctx, created.ID)
	require.NoError(t, err)
	require.Equal(t, models.CatalogStatusArchived, reloaded.Status)
}

// TestCatalogStatus_CreateAsArchivedInOneStep verifies the migration use case:
// a historical plan with existing subscribers can be created directly as
// archived (no purchasable gap), and the price round-trips with that status.
func TestCatalogStatus_CreateAsArchivedInOneStep(t *testing.T) {
	suite := setupTestSuite(t)
	ctx := context.Background()

	svc, err := billingservice.New(suite.App.Runtime)
	require.NoError(t, err)

	product, err := svc.CreateProduct(ctx, billingservice.CreateProductRequest{
		Slug:        "create_archived_" + uuid.NewString(),
		DisplayName: "Legacy Product",
		Status:      models.CatalogStatusArchived,
	})
	require.NoError(t, err)
	require.Equal(t, models.CatalogStatusArchived, product.Status)

	price, err := svc.CreatePrice(ctx, billingservice.CreatePriceRequest{
		ProductID:  product.ID,
		UnitAmount: 1999,
		Currency:   "usd",
		Status:     models.CatalogStatusArchived,
	})
	require.NoError(t, err)
	require.Equal(t, models.CatalogStatusArchived, price.Status)

	reloaded, err := svc.GetPrice(ctx, price.ID)
	require.NoError(t, err)
	require.Equal(t, models.CatalogStatusArchived, reloaded.Status)
}

// TestCatalogStatus_DraftHiddenAndNotPurchasable verifies a draft price/product
// is hidden from the public (non-admin) catalog and is not purchasable.
func TestCatalogStatus_DraftHiddenAndNotPurchasable(t *testing.T) {
	suite := setupTestSuite(t)
	ctx := context.Background()

	svc, err := billingservice.New(suite.App.Runtime)
	require.NoError(t, err)

	product, err := svc.CreateProduct(ctx, billingservice.CreateProductRequest{
		Slug:        "draft_hidden_" + uuid.NewString(),
		DisplayName: "Draft Product",
		Status:      models.CatalogStatusDraft,
	})
	require.NoError(t, err)
	require.Equal(t, models.CatalogStatusDraft, product.Status)

	price, err := svc.CreatePrice(ctx, billingservice.CreatePriceRequest{
		ProductID:  product.ID,
		UnitAmount: 500,
		Currency:   "usd",
		Status:     models.CatalogStatusDraft,
	})
	require.NoError(t, err)
	require.Equal(t, models.CatalogStatusDraft, price.Status)
	// Draft prices are not minted in any external provider.
	require.Empty(t, price.Providers, "draft price must not be created in providers")

	// Public catalog (non-admin: includeInactive=false) must not surface the draft.
	public, err := suite.App.Runtime.PublicSubscriptionService.GetProducts(ctx, false)
	require.NoError(t, err)
	for _, p := range public {
		require.NotEqual(t, product.ID, p.ID, "draft product must be hidden from public catalog")
	}
}
