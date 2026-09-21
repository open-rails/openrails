//go:build integration

package postgresmigrations_test

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/stretchr/testify/require"
	"golang.org/x/sync/errgroup"
)

func TestCreatorCatalogQueriesAndImmutableOwnership(t *testing.T) {
	ctx := t.Context()
	owner := dbtest.SharedSuperuserPGXPool(t)
	app := dbtest.SharedPGXPool(t)
	q := dbtest.Queries(app)
	mid, other := uuid.New(), uuid.New()
	_, err := owner.Exec(ctx, "INSERT INTO billing.merchants(id,slug) VALUES($1,$3),($2,$4)", mid, other, "catalog-a-"+mid.String(), "catalog-b-"+other.String())
	require.NoError(t, err)
	subject := "创作者/https://issuer.invalid|Case%2f?#x"
	a, err := q.EnsureOwnedCatalog(ctx, gen.EnsureOwnedCatalogParams{MerchantID: mid, OwnerSubject: subject})
	require.NoError(t, err)
	require.Equal(t, subject, *a.OwnerSubject)
	b, err := q.EnsureOwnedCatalog(ctx, gen.EnsureOwnedCatalogParams{MerchantID: mid, OwnerSubject: strings.ToLower(subject)})
	require.NoError(t, err)
	require.NotEqual(t, a.ID, b.ID)
	foreign, err := q.EnsureOwnedCatalog(ctx, gen.EnsureOwnedCatalogParams{MerchantID: other, OwnerSubject: subject})
	require.NoError(t, err)
	require.NotEqual(t, a.ID, foreign.ID)
	var concurrent errgroup.Group
	ids := make(chan uuid.UUID, 8)
	for range 8 {
		concurrent.Go(func() error {
			row, err := q.EnsureOwnedCatalog(ctx, gen.EnsureOwnedCatalogParams{MerchantID: mid, OwnerSubject: "concurrent-author"})
			if err == nil {
				ids <- row.ID
			}
			return err
		})
	}
	require.NoError(t, concurrent.Wait())
	close(ids)
	var canonical uuid.UUID
	for id := range ids {
		if canonical == uuid.Nil {
			canonical = id
		}
		require.Equal(t, canonical, id)
	}
	defaults, err := q.EnsureDefaultCatalog(ctx, mid)
	require.NoError(t, err)
	require.Nil(t, defaults.OwnerSubject)
	pa, pb, legacy := uuid.New(), uuid.New(), uuid.New()
	_, err = app.Exec(ctx, "INSERT INTO billing.products(id,merchant_id,catalog_id,key,display_name) VALUES($1,$3,$4,'a','A'),($2,$3,$5,'b','B')", pa, pb, mid, a.ID, b.ID)
	require.NoError(t, err)
	_, err = app.Exec(ctx, "INSERT INTO billing.products(id,merchant_id,key,display_name) VALUES($1,$2,'legacy','Legacy')", legacy, mid)
	require.NoError(t, err)
	row, err := q.GetProductByID(ctx, gen.GetProductByIDParams{MerchantID: mid, ID: legacy})
	require.NoError(t, err)
	require.Equal(t, defaults.ID, row.CatalogID)
	_, err = q.GetProductByID(ctx, gen.GetProductByIDParams{MerchantID: mid, ID: pb, CatalogID: &a.ID})
	require.ErrorIs(t, err, pgx.ErrNoRows)
	all, err := q.ListAllProducts(ctx, gen.ListAllProductsParams{MerchantID: mid, CatalogID: &a.ID})
	require.NoError(t, err)
	require.Len(t, all, 1)
	require.Equal(t, pa, all[0].ID)
	_, err = q.PatchProduct(ctx, gen.PatchProductParams{MerchantID: mid, ID: pb, CatalogID: &a.ID})
	require.ErrorIs(t, err, pgx.ErrNoRows)
	priceA, priceB := uuid.New(), uuid.New()
	create := func(id, product, catalog uuid.UUID, key string) (int64, error) {
		return q.CreatePrice(ctx, gen.CreatePriceParams{ID: id, MerchantID: mid, ProductID: product, CatalogID: &catalog, Key: key, Amount: 1200, Currency: "USD"})
	}
	n, err := create(priceA, pa, a.ID, "price-a")
	require.NoError(t, err)
	require.EqualValues(t, 1, n)
	n, err = create(priceB, pb, a.ID, "foreign")
	require.NoError(t, err)
	require.Zero(t, n)
	n, err = create(priceB, pb, b.ID, "price-b")
	require.NoError(t, err)
	require.EqualValues(t, 1, n)
	_, err = q.GetPriceByID(ctx, gen.GetPriceByIDParams{MerchantID: mid, ID: priceB, CatalogID: &a.ID})
	require.ErrorIs(t, err, pgx.ErrNoRows)
	n, err = q.UpdatePriceKey(ctx, gen.UpdatePriceKeyParams{MerchantID: mid, ID: priceB, CatalogID: &a.ID, Key: "stolen"})
	require.NoError(t, err)
	require.Zero(t, n)
	n, err = q.InsertPriceKeyMovement(ctx, gen.InsertPriceKeyMovementParams{MerchantID: mid, PriceID: priceB, CatalogID: &a.ID, Key: "foreign-history", EffectiveAt: time.Now()})
	require.NoError(t, err)
	require.Zero(t, n)
	_, err = q.UpdatePriceKey(ctx, gen.UpdatePriceKeyParams{MerchantID: mid, ID: priceA, CatalogID: &a.ID, Key: "price-b"})
	require.Error(t, err, "merchant-wide active keys must still conflict across catalogs")
	for _, statement := range []string{
		"UPDATE billing.catalogs SET owner_subject='rebound' WHERE id=$1",
		"UPDATE billing.catalogs SET merchant_id='00000000-0000-0000-0000-000000000001' WHERE id=$1",
		"DELETE FROM billing.catalogs WHERE id=$1",
	} {
		_, err := owner.Exec(ctx, statement, a.ID)
		require.ErrorContains(t, err, "immutable")
	}
	_, err = owner.Exec(ctx, "UPDATE billing.products SET catalog_id=$1 WHERE id=$2", b.ID, pa)
	require.ErrorContains(t, err, "immutable")
	_, err = app.Exec(ctx, "INSERT INTO billing.products(merchant_id,catalog_id,key,display_name) VALUES($1,$2,'foreign','Foreign')", mid, foreign.ID)
	require.ErrorContains(t, err, "products_catalog_fk")
}
