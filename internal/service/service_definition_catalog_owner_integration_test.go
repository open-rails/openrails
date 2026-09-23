//go:build integration

package service

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/catalogscope"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/pkg/merchant"
	"github.com/stretchr/testify/require"
)

func creatorCatalogService(t *testing.T) (*Service, context.Context) {
	t.Helper()
	pool := dbtest.SharedSuperuserPGXPool(t)
	mid := merchant.ID(uuid.New())
	ctx := merchant.WithID(t.Context(), mid)
	_, err := pool.Exec(ctx, "INSERT INTO billing.merchants(id,slug) VALUES($1,$2)", mid.UUID(), "creator-"+mid.String())
	require.NoError(t, err)
	database, err := db.NewWithPGXPool(pool, "")
	require.NoError(t, err)
	return &Service{rt: &app.Runtime{
		DB: database, Config: &config.Config{TestMode: config.CredentialPostureSandbox, AllowCatalogUpdates: true},
		ProductService: catalog.NewProductService(database), PriceService: catalog.NewPriceService(database),
	}}, ctx
}

func creatorContext(t *testing.T, svc *Service, ctx context.Context, subject string) (context.Context, openrails.CatalogID) {
	t.Helper()
	row, err := catalog.NewCatalogRepo(svc.rt.DB).Ensure(ctx, &subject)
	require.NoError(t, err)
	mid, err := merchant.Require(ctx)
	require.NoError(t, err)
	owner, err := catalogscope.WithOwner(ctx, catalogscope.Scope{MerchantID: mid, CatalogID: row.ID, OwnerSubject: subject})
	require.NoError(t, err)
	return owner, openrails.CatalogID(row.ID)
}

func TestCreatorCatalogServiceIsolationAndPriceKeys(t *testing.T) {
	svc, admin := creatorCatalogService(t)
	a, catalogA := creatorContext(t, svc, admin, "作者/a?opaque=1")
	b, catalogB := creatorContext(t, svc, admin, "creator-b")
	pa, err := svc.CreateProduct(a, CreateProductRequest{Key: "a", DisplayName: "A"})
	require.NoError(t, err)
	require.Equal(t, catalogA, pa.CatalogID)
	pb, err := svc.CreateProduct(b, CreateProductRequest{CatalogID: catalogB, Key: "b", DisplayName: "B"})
	require.NoError(t, err)
	defaultProduct, err := svc.CreateProduct(admin, CreateProductRequest{Key: "default", DisplayName: "Default"})
	require.NoError(t, err)
	require.NotEqual(t, catalogA, defaultProduct.CatalogID)
	require.NotEqual(t, catalogB, defaultProduct.CatalogID)

	_, err = svc.GetProduct(a, pb.ID)
	require.ErrorIs(t, err, openrails.ErrNotFound)
	_, err = svc.GetProductByKey(a, pb.Key)
	require.ErrorIs(t, err, openrails.ErrNotFound)
	_, err = svc.DeactivateProduct(a, pb.ID)
	require.ErrorIs(t, err, openrails.ErrNotFound)
	page, err := svc.ListProducts(a, ListProductsOptions{})
	require.NoError(t, err)
	require.EqualValues(t, 1, page.Total)
	require.Equal(t, pa.ID, page.Items[0].ID)
	all, err := svc.ListProducts(admin, ListProductsOptions{})
	require.NoError(t, err)
	require.EqualValues(t, 3, all.Total)
	idA := catalogA.UUID()
	onlyA, err := svc.ListProducts(admin, ListProductsOptions{CatalogID: &idA})
	require.NoError(t, err)
	require.EqualValues(t, 1, onlyA.Total)

	priceA, err := svc.CreatePrice(a, CreatePriceRequest{ProductID: pa.ID, Key: "a-usd", UnitAmount: 1_000_000, Currency: "USD"})
	require.NoError(t, err)
	priceB, err := svc.CreatePrice(b, CreatePriceRequest{ProductID: pb.ID, Key: "b-usd", UnitAmount: 2_000_000, Currency: "USD"})
	require.NoError(t, err)
	_, err = svc.CreatePrice(a, CreatePriceRequest{ProductID: pb.ID, Key: "foreign", UnitAmount: 3_000_000, Currency: "USD"})
	require.ErrorIs(t, err, openrails.ErrNotFound)
	_, err = svc.GetPrice(a, priceB.ID)
	require.ErrorIs(t, err, openrails.ErrNotFound)
	_, err = svc.DeactivatePrice(a, priceB.ID)
	require.ErrorIs(t, err, openrails.ErrNotFound)
	_, err = svc.GetPriceKeyHistory(a, priceB.Key)
	require.ErrorIs(t, err, openrails.ErrNotFound)
	_, err = svc.SetPriceKey(a, priceA.ID, priceB.Key)
	require.ErrorIs(t, err, openrails.ErrConflict)
	unchangedB, err := svc.GetPrice(admin, priceB.ID)
	require.NoError(t, err)
	require.False(t, unchangedB.Archived, "a colliding creator key must never archive another catalog's holder")
	require.Equal(t, priceB.Key, unchangedB.Key)
	unchangedA, err := svc.GetPrice(a, priceA.ID)
	require.NoError(t, err)
	require.Equal(t, priceA.Key, unchangedA.Key)
	history, err := svc.GetPriceKeyHistory(a, priceA.Key)
	require.NoError(t, err)
	require.Len(t, history, 1)
	require.Equal(t, priceA.ID, history[0].Price.ID)
	prices, err := svc.ListPrices(a, catalog.PriceFilter{}, 100, 0)
	require.NoError(t, err)
	require.EqualValues(t, 1, prices.Total)
	require.Equal(t, priceA.ID, prices.Items[0].ID)
}

func TestCreatorProviderPolicyUsesCurrentActiveMerchantAccounts(t *testing.T) {
	svc, admin := creatorCatalogService(t)
	owner, _ := creatorContext(t, svc, admin, "creator")
	mid, err := merchant.Require(admin)
	require.NoError(t, err)
	for _, account := range []struct {
		key, environment string
		archived         bool
	}{
		{key: "stripe-one", environment: "test"},
		{key: "stripe-two", environment: "test"},
		{key: "archived", environment: "test", archived: true},
		{key: "live-only", environment: "live"},
	} {
		_, err := svc.rt.DB.Qx(admin).Exec(admin, `INSERT INTO billing.psps(id,merchant_id,rail,environment,account_id,key,archived)
			VALUES($1,$2,'stripe',$3,$4,$5,$6)`, uuid.New(), mid.UUID(), account.environment, uuid.NewString(), account.key, account.archived)
		require.NoError(t, err)
	}
	keys, err := svc.creatorProviderKeys(owner)
	require.NoError(t, err)
	require.Equal(t, []string{"stripe-one", "stripe-two"}, keys)
	svc.rt.Config.TestMode = config.CredentialPostureLive
	keys, err = svc.creatorProviderKeys(owner)
	require.NoError(t, err)
	require.Equal(t, []string{"live-only"}, keys)
}
