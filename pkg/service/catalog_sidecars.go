package service

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/pkg/merchant"
)

type CatalogUsageLimitWindowSpec struct {
	Window string `json:"window"`
	Amount int64  `json:"amount"`
}

type CatalogUsageLimitSpec struct {
	Key     string                        `json:"key"`
	Measure string                        `json:"measure"`
	Windows []CatalogUsageLimitWindowSpec `json:"windows"`
}

type CatalogMeterSpec struct {
	Key  string `json:"key"`
	Kind string `json:"kind"`
}

type CatalogProductIncludesSpec struct {
	ProductSlug   string   `json:"product_slug"`
	IncludedSlugs []string `json:"included_slugs"`
}

type CatalogProductUsageLimitsSpec struct {
	ProductSlug string   `json:"product_slug"`
	Keys        []string `json:"keys"`
}

type CatalogMeteredPriceSpec struct {
	ProductSlug      string `json:"product_slug"`
	UnitAmount       int64  `json:"unit_amount"`
	Currency         string `json:"currency"`
	BillingCycleDays *int   `json:"billing_cycle_days,omitempty"`
	MeterKey         string `json:"meter_key"`
	RateMicros       int64  `json:"rate_micros"`
	PerUnits         int64  `json:"per_units"`
	PerSeconds       *int64 `json:"per_seconds,omitempty"`
}

type SyncCatalogSidecarsRequest struct {
	UsageLimits     []CatalogUsageLimitSpec         `json:"usage_limits,omitempty"`
	Meters          []CatalogMeterSpec              `json:"meters,omitempty"`
	ProductIncludes []CatalogProductIncludesSpec    `json:"product_includes,omitempty"`
	ProductLimits   []CatalogProductUsageLimitsSpec `json:"product_limits,omitempty"`
	MeteredPrices   []CatalogMeteredPriceSpec       `json:"metered_prices,omitempty"`
}

func (s *Service) SyncCatalogSidecars(ctx context.Context, req SyncCatalogSidecarsRequest) error {
	dbi, err := s.requireDB()
	if err != nil {
		return err
	}
	tid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	return dbi.RunInTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if err := syncUsageLimits(ctx, tx, tid.UUID(), req.UsageLimits); err != nil {
			return err
		}
		if err := syncMeters(ctx, tx, tid.UUID(), req.Meters, req.MeteredPrices); err != nil {
			return err
		}
		if err := syncProductUsageLimits(ctx, tx, tid.UUID(), req.ProductLimits); err != nil {
			return err
		}
		if err := syncProductIncludes(ctx, tx, tid.UUID(), req.ProductIncludes); err != nil {
			return err
		}
		return nil
	})
}

func syncUsageLimits(ctx context.Context, tx pgx.Tx, merchantID uuid.UUID, limits []CatalogUsageLimitSpec) error {
	keys := make([]string, 0, len(limits))
	for _, limit := range limits {
		key := strings.TrimSpace(limit.Key)
		keys = append(keys, key)
		windows, err := json.Marshal(limit.Windows)
		if err != nil {
			return fmt.Errorf("marshal usage_limit %q windows: %w", key, err)
		}
		if _, err := tx.Exec(ctx, `
INSERT INTO openrails.catalog_usage_limits (merchant_id, key, measure, windows)
VALUES ($1, $2, $3, $4::jsonb)
ON CONFLICT (merchant_id, key) DO UPDATE
SET measure = EXCLUDED.measure, windows = EXCLUDED.windows, updated_at = now()`,
			merchantID, key, strings.TrimSpace(limit.Measure), string(windows)); err != nil {
			return fmt.Errorf("upsert usage_limit %q: %w", key, err)
		}
	}
	if len(keys) == 0 {
		_, err := tx.Exec(ctx, `DELETE FROM openrails.catalog_usage_limits WHERE merchant_id = $1`, merchantID)
		return err
	}
	_, err := tx.Exec(ctx, `DELETE FROM openrails.catalog_usage_limits WHERE merchant_id = $1 AND NOT (key = ANY($2::text[]))`, merchantID, keys)
	return err
}

func syncMeters(ctx context.Context, tx pgx.Tx, merchantID uuid.UUID, meters []CatalogMeterSpec, metered []CatalogMeteredPriceSpec) error {
	meterKeys := make([]string, 0, len(meters))
	for _, meter := range meters {
		key := strings.TrimSpace(meter.Key)
		meterKeys = append(meterKeys, key)
		if _, err := tx.Exec(ctx, `
INSERT INTO openrails.catalog_meters (merchant_id, key, kind)
VALUES ($1, $2, $3)
ON CONFLICT (merchant_id, key) DO UPDATE
SET kind = EXCLUDED.kind, updated_at = now()`,
			merchantID, key, strings.TrimSpace(meter.Kind)); err != nil {
			return fmt.Errorf("upsert meter %q: %w", key, err)
		}
	}

	desiredPriceIDs := make([]uuid.UUID, 0, len(metered))
	for _, spec := range metered {
		priceID, err := resolveCatalogPriceID(ctx, tx, merchantID, spec)
		if err != nil {
			return err
		}
		desiredPriceIDs = append(desiredPriceIDs, priceID)
		perUnits := spec.PerUnits
		if perUnits == 0 {
			perUnits = 1
		}
		if _, err := tx.Exec(ctx, `
INSERT INTO openrails.catalog_price_metered (price_id, merchant_id, meter_key, rate_micros, per_units, per_seconds)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (price_id) DO UPDATE
SET meter_key = EXCLUDED.meter_key,
    rate_micros = EXCLUDED.rate_micros,
    per_units = EXCLUDED.per_units,
    per_seconds = EXCLUDED.per_seconds,
    updated_at = now()`,
			priceID, merchantID, strings.TrimSpace(spec.MeterKey), spec.RateMicros, perUnits, spec.PerSeconds); err != nil {
			return fmt.Errorf("upsert metered price %q: %w", spec.ProductSlug, err)
		}
	}
	if len(desiredPriceIDs) == 0 {
		if _, err := tx.Exec(ctx, `DELETE FROM openrails.catalog_price_metered WHERE merchant_id = $1`, merchantID); err != nil {
			return err
		}
	} else if _, err := tx.Exec(ctx, `DELETE FROM openrails.catalog_price_metered WHERE merchant_id = $1 AND NOT (price_id = ANY($2::uuid[]))`, merchantID, desiredPriceIDs); err != nil {
		return err
	}

	if len(meterKeys) == 0 {
		_, err := tx.Exec(ctx, `DELETE FROM openrails.catalog_meters WHERE merchant_id = $1`, merchantID)
		return err
	}
	_, err := tx.Exec(ctx, `DELETE FROM openrails.catalog_meters WHERE merchant_id = $1 AND NOT (key = ANY($2::text[]))`, merchantID, meterKeys)
	return err
}

func syncProductUsageLimits(ctx context.Context, tx pgx.Tx, merchantID uuid.UUID, productLimits []CatalogProductUsageLimitsSpec) error {
	if _, err := tx.Exec(ctx, `DELETE FROM openrails.product_usage_limits WHERE merchant_id = $1`, merchantID); err != nil {
		return err
	}
	for _, spec := range productLimits {
		productID, err := resolveProductID(ctx, tx, merchantID, spec.ProductSlug)
		if err != nil {
			return err
		}
		for _, rawKey := range spec.Keys {
			key := strings.TrimSpace(rawKey)
			if key == "" {
				continue
			}
			if _, err := tx.Exec(ctx, `
	INSERT INTO openrails.product_usage_limits (merchant_id, product_id, usage_limit_key)
	VALUES ($1, $2, $3)
	ON CONFLICT DO NOTHING`, merchantID, productID, key); err != nil {
				return fmt.Errorf("insert product usage_limit %q -> %q: %w", spec.ProductSlug, key, err)
			}
		}
	}
	return nil
}

func syncProductIncludes(ctx context.Context, tx pgx.Tx, merchantID uuid.UUID, includes []CatalogProductIncludesSpec) error {
	if _, err := tx.Exec(ctx, `DELETE FROM openrails.product_includes WHERE merchant_id = $1`, merchantID); err != nil {
		return err
	}
	for _, spec := range includes {
		parentID, err := resolveProductID(ctx, tx, merchantID, spec.ProductSlug)
		if err != nil {
			return err
		}
		for _, childSlug := range spec.IncludedSlugs {
			childID, err := resolveProductID(ctx, tx, merchantID, childSlug)
			if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `
INSERT INTO openrails.product_includes (merchant_id, product_id, included_product_id)
VALUES ($1, $2, $3)
ON CONFLICT DO NOTHING`,
				merchantID, parentID, childID); err != nil {
				return fmt.Errorf("insert product include %q -> %q: %w", spec.ProductSlug, childSlug, err)
			}
		}
	}
	return nil
}

func resolveCatalogPriceID(ctx context.Context, tx pgx.Tx, merchantID uuid.UUID, spec CatalogMeteredPriceSpec) (uuid.UUID, error) {
	productID, err := resolveProductID(ctx, tx, merchantID, spec.ProductSlug)
	if err != nil {
		return uuid.Nil, err
	}
	var priceID uuid.UUID
	if err := tx.QueryRow(ctx, `
SELECT id FROM openrails.prices
WHERE merchant_id = $1
  AND product_id = $2
  AND amount = $3
  AND lower(currency) = lower($4)
  AND access_duration_days IS NOT DISTINCT FROM $5
LIMIT 1`,
		merchantID, productID, spec.UnitAmount, spec.Currency, spec.BillingCycleDays).Scan(&priceID); err != nil {
		return uuid.Nil, fmt.Errorf("resolve price for product %q: %w", spec.ProductSlug, err)
	}
	return priceID, nil
}

func resolveProductID(ctx context.Context, tx pgx.Tx, merchantID uuid.UUID, slug string) (uuid.UUID, error) {
	var id uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT id FROM openrails.products WHERE merchant_id = $1 AND slug = $2`, merchantID, strings.TrimSpace(slug)).Scan(&id); err != nil {
		return uuid.Nil, fmt.Errorf("resolve product %q: %w", slug, err)
	}
	return id, nil
}
