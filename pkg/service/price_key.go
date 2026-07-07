package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/pkg/merchant"
)

// PriceIntervalLabel derives the canonical interval name used by #774's
// auto-default price key (`<product-key>-<interval>`). Mirrored EXACTLY by
// migrations/postgres/0001_schema.up.sql's trg_prices_default_key
// (prices_default_key) trigger function's CASE expression — keep the two in
// sync so a forward re-apply never relabels a backfilled key.
func PriceIntervalLabel(accessDurationHours *int, autoRenew bool) string {
	if !autoRenew || accessDurationHours == nil {
		return "onetime"
	}
	switch *accessDurationHours {
	case 168:
		return "weekly"
	case 720, 744:
		return "monthly"
	case 2160, 2184:
		return "quarterly"
	case 8760, 8784:
		return "yearly"
	default:
		return fmt.Sprintf("%dd", *accessDurationHours/24)
	}
}

// resolvePriceKey returns the explicit key when supplied, else the
// auto-default `<product-key>-<interval>`. Ambiguity detection (does this
// product already declare a DIFFERENT price at the same interval?) is a
// PLAN-time concern for the MODE 1 manifest converge (pkg/catalog/plan.go,
// which sees the whole declared price set at once) — this imperative,
// single-price API layer trusts the caller and always resolves to a concrete
// key so "edit the amount, apply" stays a one-field diff.
func resolvePriceKey(product *models.Product, req CreatePriceRequest) string {
	key := strings.TrimSpace(req.Key)
	if key != "" {
		return key
	}
	return product.Key + "-" + PriceIntervalLabel(req.AccessDurationHours, req.AutoRenew)
}

// GetPriceByKey resolves a price by its #774 key — the CURRENT (non-archived)
// row for that key. Used wherever checkout/API accept a price_key alongside a
// price UUID.
func (s *Service) GetPriceByKey(ctx context.Context, key string) (*CatalogPrice, error) {
	prices, err := s.requirePriceService()
	if err != nil {
		return nil, err
	}
	key = strings.TrimSpace(key)
	if key == "" {
		return nil, fmt.Errorf("key required")
	}
	tid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	p, err := prices.GetCurrentByKey(ctx, tid.UUID(), key)
	if err != nil {
		return nil, err
	}
	return priceToCatalogPrice(p), nil
}

// SetPriceKey relabels a price row onto a new key — a pure label mutation
// (the row's identity/substance never changes). Used by MODE 1's YAML-is-
// truth converge when a substance-matched, otherwise-unchanged price is
// declared under a DIFFERENT key string (a plain rename): "Key renamed =
// treated as new key + archive-by-prune of the old" — this row's key moves to
// the new value in place; any archived predecessor rows keep the OLD key as
// their back-reference (a fossil), never renamed retroactively. If another
// live row already holds the target key, THAT row is archived first (the same
// repoint invariant CreatePrice/ActivatePrice enforce), so a rename can also
// double as a manual repoint.
func (s *Service) SetPriceKey(ctx context.Context, priceID uuid.UUID, key string) (*CatalogPrice, error) {
	prices, err := s.requirePriceService()
	if err != nil {
		return nil, err
	}
	if priceID == uuid.Nil {
		return nil, fmt.Errorf("price_id required")
	}
	key = strings.TrimSpace(key)
	if key == "" {
		return nil, fmt.Errorf("key required")
	}
	current, err := prices.GetByID(ctx, priceID)
	if err != nil {
		return nil, err
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
		return nil, err
	}
	if !current.Archived {
		if err := prices.RecordKeyMovement(ctx, tid.UUID(), priceID, key, time.Now().UTC()); err != nil {
			return nil, fmt.Errorf("record key movement for %q -> %s: %w", key, priceID, err)
		}
	}
	current.Key = key
	return priceToCatalogPrice(current), nil
}

// PriceKeyHistoryEntry is one entry of a price key's version chain, resolved
// from the #774 pointer-movement log — the #777 console price page's
// "version chain with dates" surface. Most-recent-first (mirrors
// ListKeyMovements).
type PriceKeyHistoryEntry struct {
	Price       CatalogPrice `json:"price"`
	EffectiveAt time.Time    `json:"effective_at"`
}

// GetPriceKeyHistory resolves a key's full pointer-movement history log into
// the priced rows it named at each effective date — NOT built by #774 (which
// exposed only the key-resolution + per-price read surface), added here
// because the #777 console needs it to render "key's history with dates"
// rather than approximating from each price row's own created_at (which is
// wrong for a REACTIVATED row: its created_at is its ORIGINAL creation, not
// the date it most recently became current again).
func (s *Service) GetPriceKeyHistory(ctx context.Context, key string) ([]PriceKeyHistoryEntry, error) {
	prices, err := s.requirePriceService()
	if err != nil {
		return nil, err
	}
	key = strings.TrimSpace(key)
	if key == "" {
		return nil, fmt.Errorf("key required")
	}
	tid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	movements, err := prices.ListKeyMovements(ctx, tid.UUID(), key)
	if err != nil {
		return nil, err
	}
	if len(movements) == 0 {
		return nil, fmt.Errorf("price key %q not found", key)
	}
	out := make([]PriceKeyHistoryEntry, 0, len(movements))
	for _, m := range movements {
		p, err := prices.GetByID(ctx, m.PriceID)
		if err != nil {
			return nil, fmt.Errorf("resolve price %s for key %q movement: %w", m.PriceID, key, err)
		}
		out = append(out, PriceKeyHistoryEntry{Price: *priceToCatalogPrice(p), EffectiveAt: m.EffectiveAt})
	}
	return out, nil
}

// ResolvePriceReference resolves a caller-supplied price identifier that may
// be either a price UUID or a #774 price_key. Tries the UUID parse first
// (the common, cheap case); falls back to a key lookup only when the string
// does not parse as a UUID, so a key that happens to collide with UUID syntax
// can never occur (uuid.Parse is total over its own format).
func (s *Service) ResolvePriceReference(ctx context.Context, ref string) (*CatalogPrice, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return nil, fmt.Errorf("price reference required")
	}
	if id, err := uuid.Parse(ref); err == nil {
		return s.GetPrice(ctx, id)
	}
	return s.GetPriceByKey(ctx, ref)
}
