package service

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/internal/catalogscope"
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/pkg/merchant"
	"github.com/stretchr/testify/require"
)

func TestCreatorCatalogRefusalsBeforeSideEffects(t *testing.T) {
	mid := merchant.ID(uuid.New())
	ctx, err := catalogscope.WithOwner(merchant.WithID(t.Context(), mid), catalogscope.Scope{
		MerchantID: mid, CatalogID: uuid.New(), OwnerSubject: "作者/opaque subject?x=1",
	})
	require.NoError(t, err)
	// An unwired service proves these refusals precede any DB/provider access.
	svc := &Service{}
	tier := "premium"
	rank := 1
	for name, call := range map[string]func(context.Context) error{
		"create entitlements": func(ctx context.Context) error {
			_, err := svc.CreateProduct(ctx, CreateProductRequest{EntitlementsSpec: map[string]*int{}})
			return err
		},
		"create tier": func(ctx context.Context) error {
			_, err := svc.CreateProduct(ctx, CreateProductRequest{TierGroup: &tier})
			return err
		},
		"patch entitlements": func(ctx context.Context) error {
			_, err := svc.UpdateProduct(ctx, openrails.ProductID(uuid.New()), UpdateProductRequest{SetEntitlements: true})
			return err
		},
		"patch tier rank": func(ctx context.Context) error {
			_, err := svc.UpdateProduct(ctx, openrails.ProductID(uuid.New()), UpdateProductRequest{TierRank: &rank})
			return err
		},
		"skip product sync": func(ctx context.Context) error {
			_, err := svc.UpdateProduct(ctx, openrails.ProductID(uuid.New()), UpdateProductRequest{SkipRailSync: true})
			return err
		},
		"choose PSP": func(ctx context.Context) error {
			_, err := svc.CreatePrice(ctx, CreatePriceRequest{PSPs: []string{"stripe"}})
			return err
		},
		"attach PSP": func(ctx context.Context) error {
			_, err := svc.CreatePrice(ctx, CreatePriceRequest{PSPLinks: map[string]map[string]string{}})
			return err
		},
		"replace PSP": func(ctx context.Context) error {
			_, err := svc.UpdatePrice(ctx, openrails.PriceID(uuid.New()), UpdatePriceRequest{ReplacePSPLinks: true})
			return err
		},
		"skip price sync": func(ctx context.Context) error {
			_, err := svc.UpdatePrice(ctx, openrails.PriceID(uuid.New()), UpdatePriceRequest{SkipRailSync: true})
			return err
		},
		"verify provider": func(ctx context.Context) error { _, err := svc.VerifyPriceSync(ctx, uuid.New()); return err },
		"reconcile price": func(ctx context.Context) error {
			_, err := svc.ReconcilePrice(ctx, uuid.New(), ReconcileOptions{})
			return err
		},
		"reconcile product": func(ctx context.Context) error {
			_, err := svc.ReconcileProduct(ctx, uuid.New(), ReconcileOptions{})
			return err
		},
	} {
		t.Run(name, func(t *testing.T) { require.ErrorIs(t, call(ctx), catalog.ErrOwnerOperation) })
	}
	_, err = svc.CreateProduct(ctx, CreateProductRequest{CatalogID: openrails.CatalogID(uuid.New())})
	require.ErrorIs(t, err, catalog.ErrOwnerScope)
	_, err = svc.GetProduct(merchant.WithID(ctx, merchant.ID(uuid.New())), openrails.ProductID(uuid.New()))
	require.ErrorIs(t, err, catalog.ErrOwnerScope, "changing the merchant must not widen a captured owner scope")
}
