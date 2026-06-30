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
	Key           string            `json:"key"`
	Kind          string            `json:"kind,omitempty"`
	EventType     string            `json:"event_type,omitempty"`
	ValueProperty string            `json:"value_property,omitempty"`
	Aggregation   string            `json:"aggregation,omitempty"`
	Unit          string            `json:"unit,omitempty"`
	GroupBy       map[string]string `json:"group_by,omitempty"`
}

type CatalogProductIncludesSpec struct {
	ProductKey   string   `json:"product_key"`
	IncludedKeys []string `json:"included_keys"`
}

type CatalogProductUsageLimitsSpec struct {
	ProductKey string   `json:"product_key"`
	Keys       []string `json:"keys"`
}

type CatalogMeteredPriceSpec struct {
	ProductKey string `json:"product_key"`
	UnitAmount int64  `json:"unit_amount"`
	Currency   string `json:"currency"`
	// AccessDurationHours is the metered price's window in hours — the lookup key
	// that pins this meter to its price row (not a provider cadence).
	AccessDurationHours *int   `json:"access_duration_hours,omitempty"`
	MeterKey            string `json:"meter_key"`
	RateMicros          int64  `json:"rate_micros"`
	PerUnits            int64  `json:"per_units"`
	PerSeconds          *int64 `json:"per_seconds,omitempty"`
}

type CatalogRateCardSpec struct {
	ProductKey          string              `json:"product_key"`
	Ordinal             int                 `json:"ordinal"`
	MeterKey            string              `json:"meter_key,omitempty"`
	PaymentTerm         string              `json:"payment_term,omitempty"`
	BillingCadenceHours *int                `json:"billing_cadence_hours,omitempty"`
	Filter              map[string][]string `json:"filter,omitempty"`
	Allowance           json.RawMessage     `json:"allowance,omitempty"`
	Price               json.RawMessage     `json:"price"`
}

type CatalogCreditPurchaseSpec struct {
	ProductKey   string          `json:"product_key"`
	CreditType   string          `json:"credit_type"`
	Unit         string          `json:"unit"`
	Currency     string          `json:"currency"`
	ExpiresHours *int            `json:"expires_hours,omitempty"`
	Providers    []string        `json:"providers,omitempty"`
	InputMin     int64           `json:"input_min,omitempty"`
	InputMax     int64           `json:"input_max,omitempty"`
	Round        string          `json:"round,omitempty"`
	Price        json.RawMessage `json:"price"`
}

type SyncCatalogSidecarsRequest struct {
	UsageLimits     []CatalogUsageLimitSpec         `json:"usage_limits,omitempty"`
	Meters          []CatalogMeterSpec              `json:"meters,omitempty"`
	ProductIncludes []CatalogProductIncludesSpec    `json:"product_includes,omitempty"`
	ProductLimits   []CatalogProductUsageLimitsSpec `json:"product_limits,omitempty"`
	MeteredPrices   []CatalogMeteredPriceSpec       `json:"metered_prices,omitempty"`
	RateCards       []CatalogRateCardSpec           `json:"rate_cards,omitempty"`
	CreditPurchases []CatalogCreditPurchaseSpec     `json:"credit_purchases,omitempty"`
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
		if err := syncRateCards(ctx, tx, tid.UUID(), req.RateCards); err != nil {
			return err
		}
		if err := syncCreditPurchases(ctx, tx, tid.UUID(), req.CreditPurchases); err != nil {
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
	if _, err := tx.Exec(ctx, `DELETE FROM openrails.catalog_rate_cards WHERE merchant_id = $1`, merchantID); err != nil {
		return err
	}
	meterKeys := make([]string, 0, len(meters))
	for _, meter := range meters {
		key := strings.TrimSpace(meter.Key)
		meterKeys = append(meterKeys, key)
		groupBy, err := json.Marshal(meter.GroupBy)
		if err != nil {
			return fmt.Errorf("marshal meter %q group_by: %w", key, err)
		}
		if _, err := tx.Exec(ctx, `
INSERT INTO openrails.catalog_meters (merchant_id, key, kind, event_type, value_property, aggregation, unit, group_by)
VALUES ($1, $2, NULLIF($3, ''), NULLIF($4, ''), NULLIF($5, ''), NULLIF($6, ''), NULLIF($7, ''), $8::jsonb)
ON CONFLICT (merchant_id, key) DO UPDATE
SET kind = EXCLUDED.kind,
    event_type = EXCLUDED.event_type,
    value_property = EXCLUDED.value_property,
    aggregation = EXCLUDED.aggregation,
    unit = EXCLUDED.unit,
    group_by = EXCLUDED.group_by,
    updated_at = now()`,
			merchantID, key, strings.TrimSpace(meter.Kind), strings.TrimSpace(meter.EventType), strings.TrimSpace(meter.ValueProperty),
			strings.TrimSpace(meter.Aggregation), strings.TrimSpace(meter.Unit), string(groupBy)); err != nil {
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
			return fmt.Errorf("upsert metered price %q: %w", spec.ProductKey, err)
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

func syncRateCards(ctx context.Context, tx pgx.Tx, merchantID uuid.UUID, rateCards []CatalogRateCardSpec) error {
	if _, err := tx.Exec(ctx, `DELETE FROM openrails.catalog_rate_cards WHERE merchant_id = $1`, merchantID); err != nil {
		return err
	}
	for _, spec := range rateCards {
		productID, err := resolveProductID(ctx, tx, merchantID, spec.ProductKey)
		if err != nil {
			return err
		}
		filter, err := json.Marshal(spec.Filter)
		if err != nil {
			return fmt.Errorf("marshal rate card %q #%d filter: %w", spec.ProductKey, spec.Ordinal, err)
		}
		paymentTerm := strings.TrimSpace(spec.PaymentTerm)
		if paymentTerm == "" {
			paymentTerm = "in_arrears"
		}
		if _, err := tx.Exec(ctx, `
INSERT INTO openrails.catalog_rate_cards
    (merchant_id, product_id, ordinal, meter_key, payment_term, billing_cadence_hours, filter, allowance, price)
VALUES ($1, $2, $3, NULLIF($4, ''), $5, $6, $7::jsonb, NULLIF($8, '')::jsonb, $9::jsonb)`,
			merchantID, productID, spec.Ordinal, strings.TrimSpace(spec.MeterKey), paymentTerm, spec.BillingCadenceHours,
			string(filter), string(spec.Allowance), string(spec.Price)); err != nil {
			return fmt.Errorf("insert rate card %q #%d: %w", spec.ProductKey, spec.Ordinal, err)
		}
	}
	return nil
}

func syncCreditPurchases(ctx context.Context, tx pgx.Tx, merchantID uuid.UUID, purchases []CatalogCreditPurchaseSpec) error {
	if _, err := tx.Exec(ctx, `DELETE FROM openrails.catalog_credit_purchases WHERE merchant_id = $1`, merchantID); err != nil {
		return err
	}
	for _, spec := range purchases {
		productID, err := resolveProductID(ctx, tx, merchantID, spec.ProductKey)
		if err != nil {
			return err
		}
		// providers is NOT NULL with a '{}' default; a nil slice encodes as SQL NULL
		// and would violate the constraint, so coalesce to an empty array.
		providers := spec.Providers
		if providers == nil {
			providers = []string{}
		}
		if _, err := tx.Exec(ctx, `
INSERT INTO openrails.catalog_credit_purchases
    (product_id, merchant_id, credit_type, unit, currency, expires_hours, providers, input_min, input_max, round, price)
VALUES ($1, $2, $3, $4, $5, $6, $7::text[], $8, $9, NULLIF($10, ''), $11::jsonb)`,
			productID, merchantID, strings.TrimSpace(spec.CreditType), strings.TrimSpace(spec.Unit), strings.TrimSpace(spec.Currency),
			spec.ExpiresHours, providers, spec.InputMin, spec.InputMax, strings.TrimSpace(spec.Round), string(spec.Price)); err != nil {
			return fmt.Errorf("insert credit purchase %q: %w", spec.ProductKey, err)
		}
	}
	return nil
}

func syncProductUsageLimits(ctx context.Context, tx pgx.Tx, merchantID uuid.UUID, productLimits []CatalogProductUsageLimitsSpec) error {
	if _, err := tx.Exec(ctx, `DELETE FROM openrails.product_usage_limits WHERE merchant_id = $1`, merchantID); err != nil {
		return err
	}
	for _, spec := range productLimits {
		productID, err := resolveProductID(ctx, tx, merchantID, spec.ProductKey)
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
				return fmt.Errorf("insert product usage_limit %q -> %q: %w", spec.ProductKey, key, err)
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
		parentID, err := resolveProductID(ctx, tx, merchantID, spec.ProductKey)
		if err != nil {
			return err
		}
		for _, childKey := range spec.IncludedKeys {
			childID, err := resolveProductID(ctx, tx, merchantID, childKey)
			if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `
INSERT INTO openrails.product_includes (merchant_id, product_id, included_product_id)
VALUES ($1, $2, $3)
ON CONFLICT DO NOTHING`,
				merchantID, parentID, childID); err != nil {
				return fmt.Errorf("insert product include %q -> %q: %w", spec.ProductKey, childKey, err)
			}
		}
	}
	return nil
}

func resolveCatalogPriceID(ctx context.Context, tx pgx.Tx, merchantID uuid.UUID, spec CatalogMeteredPriceSpec) (uuid.UUID, error) {
	productID, err := resolveProductID(ctx, tx, merchantID, spec.ProductKey)
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
  AND access_duration_hours IS NOT DISTINCT FROM $5
LIMIT 1`,
		merchantID, productID, spec.UnitAmount, spec.Currency, spec.AccessDurationHours).Scan(&priceID); err != nil {
		return uuid.Nil, fmt.Errorf("resolve price for product %q: %w", spec.ProductKey, err)
	}
	return priceID, nil
}

func resolveProductID(ctx context.Context, tx pgx.Tx, merchantID uuid.UUID, key string) (uuid.UUID, error) {
	var id uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT id FROM openrails.products WHERE merchant_id = $1 AND key = $2`, merchantID, strings.TrimSpace(key)).Scan(&id); err != nil {
		return uuid.Nil, fmt.Errorf("resolve product %q: %w", key, err)
	}
	return id, nil
}
