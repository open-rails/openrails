//go:build integration

package merchantarchive

import (
	"bytes"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/pkg/merchant"
	"github.com/stretchr/testify/require"
)

func TestCatalogOwnershipArchiveRoundTripAndOccupiedRefusal(t *testing.T) {
	source := archiveDB(t, "creator_archive_source")
	target := archiveDB(t, "creator_archive_target")
	occupied := archiveDB(t, "creator_archive_occupied")
	mid := merchant.ID(uuid.New())
	provision(t, source, mid)
	provision(t, target, mid)
	provision(t, occupied, mid)
	ctx := merchant.WithID(t.Context(), mid)
	subject := "https://identity.example/创作者?subject=Case%2F#author"
	owned, err := source.Gen(ctx).EnsureOwnedCatalog(ctx, gen.EnsureOwnedCatalogParams{MerchantID: mid.UUID(), OwnerSubject: subject})
	require.NoError(t, err)
	product := uuid.New()
	_, err = source.Gen(ctx).CreateProduct(ctx, gen.CreateProductParams{ID: product, MerchantID: mid.UUID(), CatalogID: &owned.ID, Key: "creator-product", DisplayName: "Creator product", EntitlementsSpec: []byte("{}")})
	require.NoError(t, err)
	var artifact bytes.Buffer
	require.NoError(t, Export(ctx, source, mid, &artifact))
	_, err = Restore(ctx, target, mid, bytes.NewReader(artifact.Bytes()))
	require.NoError(t, err)
	restored, err := target.Gen(ctx).GetCatalog(ctx, gen.GetCatalogParams{MerchantID: mid.UUID(), ID: owned.ID})
	require.NoError(t, err)
	require.NotNil(t, restored.OwnerSubject)
	require.Equal(t, subject, *restored.OwnerSubject)
	restoredProduct, err := target.Gen(ctx).GetProductByID(ctx, gen.GetProductByIDParams{MerchantID: mid.UUID(), ID: product})
	require.NoError(t, err)
	require.Equal(t, owned.ID, restoredProduct.CatalogID)
	existing, err := occupied.Gen(ctx).EnsureDefaultCatalog(ctx, mid.UUID())
	require.NoError(t, err)
	_, err = Restore(ctx, occupied, mid, bytes.NewReader(artifact.Bytes()))
	require.ErrorContains(t, err, "not_empty")
	preserved, err := occupied.Gen(ctx).GetCatalog(ctx, gen.GetCatalogParams{MerchantID: mid.UUID(), ID: existing.ID})
	require.NoError(t, err)
	require.Nil(t, preserved.OwnerSubject)
	products, err := occupied.Gen(ctx).ListAllProducts(ctx, gen.ListAllProductsParams{MerchantID: mid.UUID()})
	require.NoError(t, err)
	require.Empty(t, products)
}
