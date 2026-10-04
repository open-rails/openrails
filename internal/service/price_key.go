package service

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/catalog"
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
	return product.Key + "-" + cadence.PriceIntervalLabel(req.AccessDurationHours, req.AutoRenew), true
}

// defaultKeyCadenceConflict refuses a defaulted key whose current holder bills
// on another cadence: a default key never repoints across cadences.
func defaultKeyCadenceConflict(defaulted bool, holder *models.Price, req billing.CreatePriceParams, key string) error {
	if !defaulted || holder == nil || cadence.Same(holder.AccessDurationHours, holder.AutoRenew, req.AccessDurationHours, req.AutoRenew) {
		return nil
	}
	return fmt.Errorf("%w: default key %q is held by price %s on another cadence; supply an explicit key", ErrPriceKeyCadenceConflict, key, holder.ID)
}

// GetPriceByKey resolves a price by its #774 key — the CURRENT (non-archived)
// row for that key. Used wherever checkout/API accept a price_key alongside a
// price UUID.
func (s *Service) GetPriceByKey(ctx context.Context, key string) (*billing.Price, error) {
	if err := catalog.ValidateOwnerScope(ctx); err != nil {
		return nil, err
	}
	ctx, release, pinErr := s.pin(ctx)
	if pinErr != nil {
		return nil, pinErr
	}
	defer release()

	prices, err := s.requirePriceService()
	if err != nil {
		return nil, err
	}
	key = strings.TrimSpace(key)
	if key == "" {
		return nil, apperr.Invalidf("key required")
	}
	tid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	p, err := prices.GetCurrentByKey(ctx, tid.UUID(), key)
	if err != nil {
		if db.IsNotFound(err) {
			return nil, ErrPriceKeyNotFound
		}
		return nil, err
	}
	return priceToCatalogPrice(p), nil
}

// SetPriceKey relabels a price row onto a new key — a pure label mutation
// (the row's identity/substance never changes). This explicit individual-write
// operation moves the row's key in place; any archived predecessor rows keep the OLD key as
// their back-reference (a fossil), never renamed retroactively. If another
// live row already holds the target key, THAT row is archived first (the same
// repoint invariant CreatePrice/ActivatePrice enforce), so a rename can also
// double as a manual repoint.
func (s *Service) SetPriceKey(ctx context.Context, id billing.PriceID, key string) (*billing.Price, error) {
	return catalogMutation(ctx, s, func(ctx context.Context, scoped *Service) (*billing.Price, error) {
		return scoped.setPriceKey(ctx, id, key)
	})
}

func (s *Service) setPriceKey(ctx context.Context, id billing.PriceID, key string) (*billing.Price, error) {
	if err := catalog.ValidateOwnerScope(ctx); err != nil {
		return nil, err
	}
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
	priceID := id.UUID()
	key = strings.TrimSpace(key)
	if key == "" {
		return nil, apperr.Invalidf("key required")
	}
	current, err := prices.GetByID(ctx, priceID)
	if err != nil {
		return nil, priceLookup(err)
	}
	if current.Key == key {
		return priceToCatalogPrice(current), nil
	}
	tid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	if !current.Archived {
		displaced, dErr := prices.GetCurrentByKey(ctx, tid.UUID(), key)
		if dErr != nil && !errors.Is(dErr, pgx.ErrNoRows) {
			return nil, fmt.Errorf("resolve current holder of price key %q: %w", key, dErr)
		}
		if displaced != nil && displaced.ID != priceID {
			if err := prices.SetArchived(ctx, displaced.ID, true); err != nil {
				return nil, fmt.Errorf("archive displaced price %s for key %q: %w", displaced.ID, key, err)
			}
		}
	}
	if err := prices.SetKey(ctx, priceID, key); err != nil {
		return nil, priceLookup(err)
	}
	if !current.Archived {
		if err := prices.RecordAuthoredKeyMovement(ctx, tid.UUID(), priceID, key); err != nil {
			return nil, fmt.Errorf("record key movement for %q -> %s: %w", key, priceID, err)
		}
	}
	current.Key = key
	return priceToCatalogPrice(current), nil
}

// ListPriceKeyHistory returns one page of a price key's history, most recent
// first: when the key moved to which price, or was retired.
func (s *Service) ListPriceKeyHistory(ctx context.Context, key string, page billing.PageRequest) (billing.ListPage[billing.PriceKeyMovement], error) {
	if err := catalog.ValidateOwnerScope(ctx); err != nil {
		return billing.ListPage[billing.PriceKeyMovement]{}, err
	}
	ctx, release, pinErr := s.pin(ctx)
	if pinErr != nil {
		return billing.ListPage[billing.PriceKeyMovement]{}, pinErr
	}
	defer release()

	prices, err := s.requirePriceService()
	if err != nil {
		return billing.ListPage[billing.PriceKeyMovement]{}, err
	}
	key = strings.TrimSpace(key)
	if key == "" {
		return billing.ListPage[billing.PriceKeyMovement]{}, apperr.Invalidf("key required")
	}
	tid, err := merchant.Require(ctx)
	if err != nil {
		return billing.ListPage[billing.PriceKeyMovement]{}, err
	}
	movements, err := prices.ListKeyMovements(ctx, tid.UUID(), key, page)
	if err != nil {
		return billing.ListPage[billing.PriceKeyMovement]{}, err
	}
	if len(movements.Items) == 0 && page.Cursor == "" {
		return billing.ListPage[billing.PriceKeyMovement]{}, ErrPriceKeyNotFound
	}
	out := billing.ListPage[billing.PriceKeyMovement]{Items: make([]billing.PriceKeyMovement, 0, len(movements.Items)), Next: movements.Next}
	for _, m := range movements.Items {
		p, err := prices.GetByID(ctx, m.PriceID)
		if err != nil {
			return billing.ListPage[billing.PriceKeyMovement]{}, fmt.Errorf("resolve price %s for key %q movement: %w", m.PriceID, key, err)
		}
		out.Items = append(out.Items, billing.PriceKeyMovement{EffectiveAt: m.EffectiveAt, Archived: m.Archived, Price: *priceToCatalogPrice(p)})
	}
	return out, nil
}
