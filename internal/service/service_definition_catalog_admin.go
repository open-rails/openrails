package service

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/internal/pagination"
	"github.com/open-rails/openrails/internal/shared/apperr"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
)

// GetProduct returns a product by ID.
func (s *Service) GetProduct(ctx context.Context, id billing.ProductID) (*billing.Product, error) {
	ctx, release, pinErr := s.pin(ctx)
	if pinErr != nil {
		return nil, pinErr
	}
	defer release()

	products, err := s.requireProductService()
	if err != nil {
		return nil, err
	}
	if id.IsZero() {
		return nil, apperr.Invalidf("product_id required")
	}
	productID := id.UUID()
	p, err := products.GetByID(ctx, productID)
	if err != nil {
		return nil, productLookup(err)
	}
	return productToCatalogProduct(p), nil
}

// GetProductByKey returns a product by its key.
func (s *Service) GetProductByKey(ctx context.Context, key string) (*billing.Product, error) {
	ctx, release, pinErr := s.pin(ctx)
	if pinErr != nil {
		return nil, pinErr
	}
	defer release()

	products, err := s.requireProductService()
	if err != nil {
		return nil, err
	}
	key = strings.TrimSpace(key)
	if key == "" {
		return nil, apperr.Invalidf("key required")
	}
	p, err := products.GetByKey(ctx, key)
	if err != nil {
		return nil, productLookup(err)
	}
	return productToCatalogProduct(p), nil
}

// ListProducts returns one page of products, newest first. Prices are not
// loaded; HydratePrices adds them.
func (s *Service) ListProducts(ctx context.Context, params billing.ProductListParams) (billing.ListPage[billing.Product], error) {
	ctx, release, err := s.pin(ctx)
	if err != nil {
		return billing.ListPage[billing.Product]{}, err
	}
	defer release()
	products, err := s.requireProductService()
	if err != nil {
		return billing.ListPage[billing.Product]{}, err
	}
	if len(params.Keys) > billing.MaxBatchItems || len(params.Entitlements) > billing.MaxBatchItems {
		return billing.ListPage[billing.Product]{}, apperr.Invalidf("keys and entitlement each name at most %d values", billing.MaxBatchItems)
	}
	filter := catalog.ProductFilter{IDs: uuidutil.Of(params.IDs), Keys: params.Keys, Archived: params.Archived, TierGroup: params.TierGroup, Entitlements: params.Entitlements, ForSale: params.ForSale}
	page, err := products.List(ctx, filter, params.PageRequest)
	if err != nil {
		return billing.ListPage[billing.Product]{}, err
	}
	return pagination.Map(page, func(p *models.Product) billing.Product { return *productToCatalogProduct(p) }), nil
}

// HydratePrices sets each product's Prices to its current prices.
func (s *Service) HydratePrices(ctx context.Context, products []billing.Product) error {
	if len(products) == 0 {
		return nil
	}
	ctx, release, err := s.pin(ctx)
	if err != nil {
		return err
	}
	defer release()
	prices, err := s.requirePriceService()
	if err != nil {
		return err
	}
	ids := make([]uuid.UUID, len(products))
	for i, p := range products {
		ids[i] = p.ID.UUID()
	}
	current, err := prices.CurrentByProducts(ctx, ids)
	if err != nil {
		return err
	}
	for i := range products {
		products[i].Prices = make([]billing.Price, 0, len(current[ids[i]]))
		for _, p := range current[ids[i]] {
			products[i].Prices = append(products[i].Prices, *priceToCatalogPrice(p))
		}
	}
	return nil
}

// ActivateProduct sets status=active on a product.
func (s *Service) ActivateProduct(ctx context.Context, id billing.ProductID) (*billing.Product, error) {
	archived := false
	req := UpdateProductRequest{Archived: &archived}
	return catalogCommand(ctx, s, "product/"+id.String(), req, "", productEditPlan(id, req), func(ctx context.Context, s *Service) (*billing.Product, error) {
		return catalogMutation(ctx, s, func(ctx context.Context, scoped *Service) (*billing.Product, error) {
			return scoped.activateProduct(ctx, id)
		})
	})
}

func (s *Service) activateProduct(ctx context.Context, id billing.ProductID) (*billing.Product, error) {
	ctx, release, pinErr := s.pin(ctx)
	if pinErr != nil {
		return nil, pinErr
	}
	defer release()

	products, err := s.requireProductService()
	if err != nil {
		return nil, err
	}
	if id.IsZero() {
		return nil, apperr.Invalidf("product_id required")
	}
	productID := id.UUID()
	current, err := products.GetByID(ctx, productID)
	if err != nil {
		return nil, productLookup(err)
	}
	if err := s.validateProductCreditUpdate(ctx, id, current.CreditGrant); err != nil {
		return nil, err
	}
	if err := products.Activate(ctx, productID); err != nil {
		return nil, productLookup(err)
	}
	updated, err := products.GetByID(ctx, productID)
	if err != nil {
		return nil, productLookup(err)
	}
	return productToCatalogProduct(updated), nil
}

// DeactivateProduct archives a product. Existing subscriptions on its prices
// are grandfathered and keep billing.
func (s *Service) DeactivateProduct(ctx context.Context, id billing.ProductID) (*billing.Product, error) {
	archived := true
	req := UpdateProductRequest{Archived: &archived}
	return catalogCommand(ctx, s, "product/"+id.String(), req, "", productEditPlan(id, req), func(ctx context.Context, s *Service) (*billing.Product, error) {
		return catalogMutation(ctx, s, func(ctx context.Context, scoped *Service) (*billing.Product, error) {
			return scoped.deactivateProduct(ctx, id)
		})
	})
}

func (s *Service) deactivateProduct(ctx context.Context, id billing.ProductID) (*billing.Product, error) {
	ctx, release, pinErr := s.pin(ctx)
	if pinErr != nil {
		return nil, pinErr
	}
	defer release()

	products, err := s.requireProductService()
	if err != nil {
		return nil, err
	}
	if id.IsZero() {
		return nil, apperr.Invalidf("product_id required")
	}
	productID := id.UUID()
	if err := products.Deactivate(ctx, productID); err != nil {
		return nil, productLookup(err)
	}
	updated, err := products.GetByID(ctx, productID)
	if err != nil {
		return nil, productLookup(err)
	}
	return productToCatalogProduct(updated), nil
}

// GetPrice returns a price by ID.
func (s *Service) GetPrice(ctx context.Context, id billing.PriceID) (*billing.Price, error) {
	ctx, release, pinErr := s.pin(ctx)
	if pinErr != nil {
		return nil, pinErr
	}
	defer release()

	prices, err := s.requirePriceService()
	if err != nil {
		return nil, err
	}
	if id.IsZero() {
		return nil, apperr.Invalidf("price_id required")
	}
	p, err := prices.GetByPublicID(ctx, id)
	if err != nil {
		return nil, priceLookup(err)
	}
	return priceToCatalogPrice(p), nil
}

// ListPricesByProduct returns all prices belonging to a product. Set activeOnly=true to filter inactive.
func (s *Service) ListPricesByProduct(ctx context.Context, id billing.ProductID, activeOnly bool) ([]billing.Price, error) {
	ctx, release, pinErr := s.pin(ctx)
	if pinErr != nil {
		return nil, pinErr
	}
	defer release()

	prices, err := s.requirePriceService()
	if err != nil {
		return nil, err
	}
	if id.IsZero() {
		return nil, apperr.Invalidf("product_id required")
	}
	productID := id.UUID()
	var raws []*models.Price
	if activeOnly {
		raws, err = prices.GetActiveByProductID(ctx, productID)
	} else {
		raws, err = prices.GetByProductID(ctx, productID)
	}
	if err != nil {
		return nil, err
	}
	out := make([]billing.Price, 0, len(raws))
	for _, p := range raws {
		out = append(out, *priceToCatalogPrice(p))
	}
	return out, nil
}

// ListPrices returns one page of prices, newest first.
func (s *Service) ListPrices(ctx context.Context, params billing.PriceListParams) (billing.ListPage[billing.Price], error) {
	ctx, release, err := s.pin(ctx)
	if err != nil {
		return billing.ListPage[billing.Price]{}, err
	}
	defer release()
	prices, err := s.requirePriceService()
	if err != nil {
		return billing.ListPage[billing.Price]{}, err
	}
	ids := make([]string, 0, len(params.IDs))
	if params.IDs == nil {
		ids = nil
	}
	for _, id := range params.IDs {
		ids = append(ids, id.String())
	}
	filter := catalog.PriceFilter{IDs: ids, Archived: params.Archived, Currency: moneyutil.NormalizeCurrency(params.Currency),
		ProductKey: strings.TrimSpace(params.ProductKey), Key: strings.TrimSpace(params.Key), Recurring: params.Recurring}
	if !params.ProductID.IsZero() {
		id := params.ProductID.UUID()
		filter.ProductID = &id
	}
	page, err := prices.List(ctx, filter, params.PageRequest)
	if err != nil {
		return billing.ListPage[billing.Price]{}, err
	}
	return pagination.Map(page, func(p *models.Price) billing.Price { return *priceToCatalogPrice(p) }), nil
}

// ActivatePrice un-archives a price. #774: for a price that was ARCHIVED, this
// is exactly the flip-flop REACTIVATION path — re-declaring a previously-seen
// substance (matched by matchPrice against the #662 deterministic id) finds
// this archived row rather than minting a new one. Its key is already set
// from creation, so activation repoints that key here: whatever OTHER row
// currently holds it is archived first (the partial unique index on
// (merchant_id, key) WHERE NOT archived allows only one live holder), then
// this row is un-archived, then one pointer-movement log entry records the
// move. Activating an already-active row is a no-op (no movement logged).
func (s *Service) ActivatePrice(ctx context.Context, id billing.PriceID) (*billing.Price, error) {
	return catalogCommand(ctx, s, "price/"+id.String(), struct{ Archived bool }{false}, "", func(ctx context.Context, s *Service, state catalogState) ([]catalogObject, error) {
		archived := false
		return priceEditPlan(id, UpdatePriceRequest{Archived: &archived})(ctx, s, state)
	}, func(ctx context.Context, s *Service) (*billing.Price, error) { return s.activatePriceCommand(ctx, id) })
}

func (s *Service) activatePriceCommand(ctx context.Context, id billing.PriceID) (*billing.Price, error) {
	return catalogMutation(ctx, s, func(ctx context.Context, scoped *Service) (*billing.Price, error) {
		return scoped.activatePrice(ctx, id)
	})
}

func (s *Service) activatePrice(ctx context.Context, id billing.PriceID) (*billing.Price, error) {
	ctx, release, pinErr := s.pin(ctx)
	if pinErr != nil {
		return nil, pinErr
	}
	defer release()

	prices, err := s.requirePriceService()
	if err != nil {
		return nil, err
	}
	if id.IsZero() {
		return nil, apperr.Invalidf("price_id required")
	}
	resolved, err := prices.GetByPublicID(ctx, id)
	if err != nil {
		return nil, priceLookup(err)
	}
	priceID := resolved.ID
	current := resolved
	products, err := s.requireProductService()
	if err != nil {
		return nil, err
	}
	product, err := products.GetByID(ctx, current.ProductID)
	if err != nil {
		return nil, productLookup(err)
	}
	if err := validateCreditPrice(product.CreditGrant, billing.CreatePriceParams{Currency: current.Currency, UnitAmount: current.Amount, BillingIntervalHours: current.BillingIntervalHours, CustomerAmount: current.CustomerAmount}); err != nil {
		return nil, err
	}
	wasArchived := current.Archived
	if wasArchived {
		tid, err := merchant.Require(ctx)
		if err != nil {
			return nil, err
		}
		displaced, dErr := prices.GetCurrentByKey(ctx, tid.UUID(), current.ProductID, current.Key)
		if dErr != nil && !errors.Is(dErr, pgx.ErrNoRows) {
			return nil, fmt.Errorf("resolve current holder of price key %q: %w", current.Key, dErr)
		}
		if displaced != nil && displaced.ID != priceID {
			if err := prices.SetArchived(ctx, displaced.ID, true); err != nil {
				return nil, fmt.Errorf("archive displaced price %s for key %q: %w", displaced.ID, current.Key, err)
			}
		}
	}
	if err := prices.Activate(ctx, priceID); err != nil {
		return nil, priceLookup(err)
	}
	updated, err := prices.GetByID(ctx, priceID)
	if err != nil {
		return nil, priceLookup(err)
	}
	if wasArchived {
		tid, err := merchant.Require(ctx)
		if err != nil {
			return nil, err
		}
		if err := prices.RecordAuthoredKeyMovement(ctx, tid.UUID(), priceID, updated.Key); err != nil {
			return nil, fmt.Errorf("record key movement for %q -> %s: %w", updated.Key, priceID, err)
		}
	}
	return priceToCatalogPrice(updated), nil
}

// DeactivatePrice archives a price. Existing subscriptions on this price are
// grandfathered and keep billing; new purchases are rejected.
func (s *Service) DeactivatePrice(ctx context.Context, id billing.PriceID) (*billing.Price, error) {
	return catalogCommand(ctx, s, "price/"+id.String(), struct{ Archived bool }{true}, "", func(ctx context.Context, s *Service, state catalogState) ([]catalogObject, error) {
		archived := true
		return priceEditPlan(id, UpdatePriceRequest{Archived: &archived})(ctx, s, state)
	}, func(ctx context.Context, s *Service) (*billing.Price, error) {
		return s.deactivatePriceCommand(ctx, id)
	})
}

func (s *Service) deactivatePriceCommand(ctx context.Context, id billing.PriceID) (*billing.Price, error) {
	return catalogMutation(ctx, s, func(ctx context.Context, scoped *Service) (*billing.Price, error) {
		return scoped.deactivatePrice(ctx, id)
	})
}

func (s *Service) deactivatePrice(ctx context.Context, id billing.PriceID) (*billing.Price, error) {
	ctx, release, pinErr := s.pin(ctx)
	if pinErr != nil {
		return nil, pinErr
	}
	defer release()

	prices, err := s.requirePriceService()
	if err != nil {
		return nil, err
	}
	if id.IsZero() {
		return nil, apperr.Invalidf("price_id required")
	}
	resolved, err := prices.GetByPublicID(ctx, id)
	if err != nil {
		return nil, priceLookup(err)
	}
	priceID := resolved.ID
	if err := prices.Deactivate(ctx, priceID); err != nil {
		return nil, priceLookup(err)
	}
	updated, err := prices.GetByID(ctx, priceID)
	if err != nil {
		return nil, priceLookup(err)
	}
	return priceToCatalogPrice(updated), nil
}

// VerifyPublicPriceSync reads a provider price's provider again and reports
// whether its terms still match; one of ours has nothing to verify.
func (s *Service) VerifyPublicPriceSync(ctx context.Context, id billing.PriceID) (*billing.Price, error) {
	ctx, release, pinErr := s.pin(ctx)
	if pinErr != nil {
		return nil, pinErr
	}
	defer release()
	prices, err := s.requirePriceService()
	if err != nil {
		return nil, err
	}
	price, err := prices.GetByPublicID(ctx, id)
	if err != nil {
		return nil, priceLookup(err)
	}
	out := priceToCatalogPrice(price)
	s.verifyProviderPrice(ctx, price, out.Provider)
	return out, nil
}

// cloneRails copies a price's bindings.
func cloneRails(in map[string]map[string]string) map[string]map[string]string {
	out := make(map[string]map[string]string, len(in))
	for k, v := range in {
		out[k] = maps.Clone(v)
	}
	return out
}
