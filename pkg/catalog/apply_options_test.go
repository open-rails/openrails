package catalog

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/internal/db/models"
	billingservice "github.com/open-rails/openrails/pkg/service"
)

func TestApplyWithOptionsFiltersMutationClasses(t *testing.T) {
	ctx := context.Background()
	existingProductID := uuid.New()
	existingPriceID := uuid.New()
	removedProductID := uuid.New()

	plan := &ApplyPlan{Groups: []GroupPlan{{
		Slug: "memberships",
		Products: []ProductPlan{
			{
				Slug:   "new",
				Action: ProductCreate,
				CreateReq: billingservice.CreateProductRequest{
					Slug:        "new",
					DisplayName: "New",
					Status:      models.CatalogStatusActive,
				},
				Prices: []PricePlan{{
					Label:  "new $9.99/month",
					Action: PriceCreate,
					CreateReq: billingservice.CreatePriceRequest{
						UnitAmount: 999,
						Currency:   "usd",
						Status:     models.CatalogStatusActive,
					},
				}},
			},
			{
				Slug:     "existing",
				Action:   ProductUpdate,
				UpdateID: existingProductID,
				Prices: []PricePlan{
					{Label: "existing $9.99/month", Action: PriceActivate, ExistingID: existingPriceID},
					{Label: "existing $19.99/month", Action: PriceArchive, ExistingID: uuid.New()},
				},
			},
		},
		RemovedProducts: []billingservice.CatalogProduct{{ID: removedProductID, Slug: "removed"}},
	}}}

	t.Run("insert only", func(t *testing.T) {
		applier := newFakeApplier()
		res, err := ApplyWithOptions(ctx, applier, plan, ApplyOptions{Insert: true})
		if err != nil {
			t.Fatalf("ApplyWithOptions: %v", err)
		}
		if res.ProductsCreated != 1 || res.PricesCreated != 1 {
			t.Fatalf("insert counts = products %d prices %d, want 1/1", res.ProductsCreated, res.PricesCreated)
		}
		if len(applier.updatedProducts) != 0 || len(applier.activatedPrices) != 0 || len(applier.archivedPrices) != 0 || len(applier.deactivatedProds) != 0 {
			t.Fatalf("insert-only executed non-insert mutations: updates=%d activates=%d priceArchives=%d productArchives=%d",
				len(applier.updatedProducts), len(applier.activatedPrices), len(applier.archivedPrices), len(applier.deactivatedProds))
		}
	})

	t.Run("overwrite only", func(t *testing.T) {
		applier := newFakeApplier()
		res, err := ApplyWithOptions(ctx, applier, plan, ApplyOptions{Overwrite: true})
		if err != nil {
			t.Fatalf("ApplyWithOptions: %v", err)
		}
		if res.ProductsUpdated != 1 || res.PricesActivated != 1 {
			t.Fatalf("overwrite counts = products %d prices %d, want 1/1", res.ProductsUpdated, res.PricesActivated)
		}
		if len(applier.createdProducts) != 0 || len(applier.createdPrices) != 0 || len(applier.archivedPrices) != 0 || len(applier.deactivatedProds) != 0 {
			t.Fatalf("overwrite-only executed non-overwrite mutations: productCreates=%d priceCreates=%d priceArchives=%d productArchives=%d",
				len(applier.createdProducts), len(applier.createdPrices), len(applier.archivedPrices), len(applier.deactivatedProds))
		}
	})

	t.Run("prune only", func(t *testing.T) {
		applier := newFakeApplier()
		res, err := ApplyWithOptions(ctx, applier, plan, ApplyOptions{Prune: true})
		if err != nil {
			t.Fatalf("ApplyWithOptions: %v", err)
		}
		if res.ProductsArchived != 1 || res.PricesArchived != 1 {
			t.Fatalf("prune counts = products %d prices %d, want 1/1", res.ProductsArchived, res.PricesArchived)
		}
		if len(applier.createdProducts) != 0 || len(applier.createdPrices) != 0 || len(applier.updatedProducts) != 0 || len(applier.activatedPrices) != 0 {
			t.Fatalf("prune-only executed non-prune mutations: productCreates=%d priceCreates=%d updates=%d activates=%d",
				len(applier.createdProducts), len(applier.createdPrices), len(applier.updatedProducts), len(applier.activatedPrices))
		}
	})
}
