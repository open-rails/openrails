package service

import (
	"context"
	"fmt"
	"strings"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/shared/apperr"
	"github.com/open-rails/openrails/internal/shared/cadence"
)

// resolvePriceKey returns the explicit key when supplied, else the
// auto-default `<product-key>-<interval>` (cadence.PriceIntervalLabel), and
// whether the default was used. Batch applications require explicit price
// keys; this default belongs to individual price creation. Changing the
// financial terms at a key creates or reuses a financial version.
func resolvePriceKey(product *models.Product, req billing.CreatePriceParams) (string, bool) {
	key := strings.TrimSpace(req.Key)
	if key != "" {
		return key, false
	}
	return product.Key + "-" + cadence.PriceIntervalLabel(req.BillingIntervalHours), true
}

// defaultKeyCadenceConflict refuses a defaulted key whose current holder bills
// on another cadence: a default key never repoints across cadences.
func defaultKeyCadenceConflict(defaulted bool, holder *models.Price, req billing.CreatePriceParams, key string) error {
	if !defaulted || holder == nil || cadence.Same(holder.BillingIntervalHours, req.BillingIntervalHours) {
		return nil
	}
	return fmt.Errorf("%w: default key %q is held by price %s on another cadence; supply an explicit key", ErrPriceKeyCadenceConflict, key, holder.ID)
}

// ListPriceHistory returns one page of the history of a price's key, most
// recent first: when the key moved to which price, or was retired.
func (s *Service) ListPriceHistory(ctx context.Context, id billing.PriceID, page billing.PageRequest) (billing.ListPage[billing.PriceKeyMovement], error) {
	ctx, release, pinErr := s.pin(ctx)
	if pinErr != nil {
		return billing.ListPage[billing.PriceKeyMovement]{}, pinErr
	}
	defer release()

	prices, err := s.requirePriceService()
	if err != nil {
		return billing.ListPage[billing.PriceKeyMovement]{}, err
	}
	if id.IsZero() {
		return billing.ListPage[billing.PriceKeyMovement]{}, apperr.Invalidf("price_id required")
	}
	tid, err := merchant.Require(ctx)
	if err != nil {
		return billing.ListPage[billing.PriceKeyMovement]{}, err
	}
	price, err := prices.GetByID(ctx, id.UUID())
	if err != nil {
		return billing.ListPage[billing.PriceKeyMovement]{}, priceLookup(err)
	}
	movements, err := prices.ListKeyMovements(ctx, tid.UUID(), price.ProductID, price.Key, page)
	if err != nil {
		return billing.ListPage[billing.PriceKeyMovement]{}, err
	}
	out := billing.ListPage[billing.PriceKeyMovement]{Items: make([]billing.PriceKeyMovement, 0, len(movements.Items)), Next: movements.Next}
	for _, m := range movements.Items {
		p, err := prices.GetByID(ctx, m.PriceID)
		if err != nil {
			return billing.ListPage[billing.PriceKeyMovement]{}, fmt.Errorf("resolve price %s for key %q movement: %w", m.PriceID, price.Key, err)
		}
		out.Items = append(out.Items, billing.PriceKeyMovement{EffectiveAt: m.EffectiveAt, Archived: m.Archived, Price: *priceToCatalogPrice(p)})
	}
	return out, nil
}
