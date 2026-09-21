//go:build integration

package merchants

import (
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/pkg/merchant"
	"github.com/stretchr/testify/require"
)

func TestMerchantPurgePreservesCreatorCatalogIdentity(t *testing.T) {
	ctx := t.Context()
	owner := dbtest.SharedSuperuserPGXPool(t)
	runtime := dbtest.SharedPGXPool(t)
	q := dbtest.Queries(runtime)
	mid := merchant.ID(uuid.New())
	slug := "catalog-purge-" + mid.String()
	_, err := owner.Exec(ctx, "INSERT INTO billing.merchants(id,slug,status) VALUES($1,$2,'active')", mid.UUID(), slug)
	require.NoError(t, err)
	defaults, err := q.EnsureDefaultCatalog(ctx, mid.UUID())
	require.NoError(t, err)
	catalogs := []gen.OpenrailsCatalog{defaults}
	for _, subject := range []string{"creator/α", "creator/β"} {
		row, err := q.EnsureOwnedCatalog(ctx, gen.EnsureOwnedCatalogParams{MerchantID: mid.UUID(), OwnerSubject: subject})
		require.NoError(t, err)
		catalogs = append(catalogs, row)
	}
	for i, catalog := range catalogs {
		product, price := uuid.New(), uuid.New()
		_, err = q.CreateProduct(ctx, gen.CreateProductParams{ID: product, MerchantID: mid.UUID(), CatalogID: &catalog.ID, Key: fmt.Sprintf("purge-product-%d", i), DisplayName: "Purge product", EntitlementsSpec: []byte("{}")})
		require.NoError(t, err)
		_, err = q.CreatePrice(ctx, gen.CreatePriceParams{ID: price, MerchantID: mid.UUID(), ProductID: product, CatalogID: &catalog.ID, Key: fmt.Sprintf("purge-price-%d", i), Amount: 1000, Currency: "USD"})
		require.NoError(t, err)
	}
	svc, err := NewService(db.WrapPool(runtime, config.DefaultSchema), nil, "test")
	require.NoError(t, err)
	svc.WithDestructivePolicy(allowAllDestructive{})
	inventory, err := svc.TakePurgeInventory(ctx, mid)
	require.NoError(t, err)
	require.Equal(t, 6, inventory.TotalRows)
	require.Contains(t, strings.Join(inventory.NotCaptured, " "), "CATALOG IDENTITY")
	require.NoError(t, svc.Delete(ctx, mid, DeleteOptions{ConfirmPhrase: PurgeConfirmPhrase(slug), ExpectRows: &inventory.TotalRows, InventoryID: inventory.ID, Actor: "catalog-purge-operator"}))
	products, err := q.ListAllProducts(ctx, gen.ListAllProductsParams{MerchantID: mid.UUID()})
	require.NoError(t, err)
	require.Empty(t, products)
	prices, err := q.ListAllPricesWithProduct(ctx, gen.ListAllPricesWithProductParams{MerchantID: mid.UUID()})
	require.NoError(t, err)
	require.Empty(t, prices)
	var status string
	require.NoError(t, owner.QueryRow(ctx, "SELECT status FROM billing.merchants WHERE id=$1", mid.UUID()).Scan(&status))
	require.Equal(t, "deleted", status)
	for _, before := range catalogs {
		after, err := q.GetCatalog(ctx, gen.GetCatalogParams{MerchantID: mid.UUID(), ID: before.ID})
		require.NoError(t, err)
		require.Equal(t, before.OwnerSubject, after.OwnerSubject)
		_, err = owner.Exec(ctx, "UPDATE billing.catalogs SET owner_subject='rebound' WHERE id=$1", before.ID)
		require.ErrorContains(t, err, "immutable")
		_, err = owner.Exec(ctx, "DELETE FROM billing.catalogs WHERE id=$1", before.ID)
		require.ErrorContains(t, err, "immutable")
	}
}
