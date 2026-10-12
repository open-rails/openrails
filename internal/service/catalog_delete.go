package service

import (
	"context"
	"errors"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/shared/apperr"
)

// ErrProductInUse refuses to delete a product a record refers to: a purchase
// keeps naming its product, so it is only ever archived.
var ErrProductInUse = apperr.New(http.StatusConflict, billing.CodeProductInUse, "records refer to the product; archive it instead")

// DeleteProduct deletes a product nothing ever referred to, with its prices.
// A product a purchase, subscription, grant, access, checkout, migration,
// provider write, PSP link or rate card refers to is refused (ErrProductInUse):
// archive it instead.
func (s *Service) DeleteProduct(ctx context.Context, id billing.ProductID) error {
	_, err := catalogCommand(ctx, s, "product/"+id.String(), struct {
		Operation string
		ID        billing.ProductID
	}{"delete", id}, "", false, deleteProductPlan(id), func(ctx context.Context, s *Service) (struct{}, error) {
		return struct{}, s.deleteProductCommand(ctx, id)
	})
	return err
}

func (s *Service) deleteProductCommand(ctx context.Context, id billing.ProductID) error {
	_, err := catalogMutation(ctx, s, func(ctx context.Context, scoped *Service) (struct{}, error) {
		if id.IsZero() {
			return struct{}{}, apperr.Invalidf("product_id required")
		}
		return struct{}{}, scoped.deleteProduct(ctx, id.UUID())
	})
	return err
}

// deleteProduct deletes the product, or refuses with ErrProductInUse. Within
// a catalog mutation.
func (s *Service) deleteProduct(ctx context.Context, productID uuid.UUID) error {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	q := s.catalogDatabase().Gen(ctx)
	if _, err := q.LockProductForDelete(ctx, gen.LockProductForDeleteParams{MerchantID: mid.UUID(), ID: productID}); errors.Is(err, pgx.ErrNoRows) {
		return ErrProductNotFound
	} else if err != nil {
		return err
	}
	referenced, err := q.ProductHoldsReferences(ctx, gen.ProductHoldsReferencesParams{MerchantID: mid.UUID(), ProductID: productID})
	if err != nil {
		return err
	}
	priceIDs, err := q.ListProductPriceIDs(ctx, gen.ListProductPriceIDsParams{MerchantID: mid.UUID(), ProductID: productID})
	if err != nil {
		return err
	}
	if !referenced && len(priceIDs) > 0 {
		if referenced, err = q.PricesHoldReferences(ctx, gen.PricesHoldReferencesParams{MerchantID: mid.UUID(), PriceIds: priceIDs}); err != nil {
			return err
		}
	}
	if referenced {
		return ErrProductInUse
	}
	// Its keys close first: no key query or benefit check sees them again.
	keys, err := q.ListLiveProductEntitlements(ctx, gen.ListLiveProductEntitlementsParams{MerchantID: mid.UUID(), ProductIds: []uuid.UUID{productID}})
	if err != nil {
		return err
	}
	if len(keys) > 0 {
		names := make([]string, len(keys))
		for i, k := range keys {
			names[i] = k.Entitlement
		}
		edit := s.keyEdit(ctx)
		if _, err := q.RemoveProductEntitlements(ctx, gen.RemoveProductEntitlementsParams{MerchantID: mid.UUID(), ProductID: productID, Entitlements: names, At: edit.At, Actor: edit.Actor}); err != nil {
			return err
		}
	}
	if err := q.DeleteProductPrices(ctx, gen.DeleteProductPricesParams{MerchantID: mid.UUID(), ProductID: productID}); err != nil {
		return err
	}
	if _, err := q.DeleteProduct(ctx, gen.DeleteProductParams{MerchantID: mid.UUID(), ID: productID}); err != nil {
		return err
	}
	return nil
}

// deletePriceKey deletes every version of a price key nothing ever referred
// to and reports true, or reports false when a record refers to one of them.
// Within a catalog mutation.
func (s *Service) deletePriceKey(ctx context.Context, productID uuid.UUID, key string) (bool, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return false, err
	}
	q := s.catalogDatabase().Gen(ctx)
	ids, err := q.ListPriceKeyIDs(ctx, gen.ListPriceKeyIDsParams{MerchantID: mid.UUID(), ProductID: productID, Key: key})
	if err != nil || len(ids) == 0 {
		return false, err
	}
	referenced, err := q.PricesHoldReferences(ctx, gen.PricesHoldReferencesParams{MerchantID: mid.UUID(), PriceIds: ids})
	if err != nil || referenced {
		return false, err
	}
	return true, q.DeletePriceKey(ctx, gen.DeletePriceKeyParams{MerchantID: mid.UUID(), ProductID: productID, Key: key})
}
