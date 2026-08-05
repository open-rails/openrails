package service

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/internal/shared/moneyutil"
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

type CatalogRateCardSpec struct {
	ProductKey  string              `json:"product_key"`
	Ordinal     int                 `json:"ordinal"`
	MeterKey    string              `json:"meter_key,omitempty"`
	PaymentTerm string              `json:"payment_term,omitempty"`
	Filter      map[string][]string `json:"filter,omitempty"`
	Allowance   json.RawMessage     `json:"allowance,omitempty"`
	Price       json.RawMessage     `json:"price"`
}

type CatalogCreditBalanceSpec struct {
	Key          string `json:"key"`
	Unit         string `json:"unit"`
	ExpiresHours *int   `json:"expires_hours,omitempty"`
}

type CatalogCreditPurchasePriceSpec struct {
	ProductKey string          `json:"product_key"`
	Ordinal    int             `json:"ordinal"`
	CreditKey  string          `json:"credit_key"`
	Currency   string          `json:"currency"`
	PSPs       []string        `json:"psps,omitempty"`
	InputMin   int64           `json:"input_min,omitempty"`
	InputMax   int64           `json:"input_max,omitempty"`
	Price      json.RawMessage `json:"price"`
}

type SyncCatalogSidecarsRequest struct {
	UsageLimits     []CatalogUsageLimitSpec          `json:"usage_limits,omitempty"`
	Meters          []CatalogMeterSpec               `json:"meters,omitempty"`
	ProductIncludes []CatalogProductIncludesSpec     `json:"product_includes,omitempty"`
	ProductLimits   []CatalogProductUsageLimitsSpec  `json:"product_limits,omitempty"`
	RateCards       []CatalogRateCardSpec            `json:"rate_cards,omitempty"`
	CreditBalances  []CatalogCreditBalanceSpec       `json:"credit_balances,omitempty"`
	CreditPurchases []CatalogCreditPurchasePriceSpec `json:"credit_purchases,omitempty"`
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
		if err := syncMeters(ctx, tx, tid.UUID(), req.Meters); err != nil {
			return err
		}
		if err := syncRateCards(ctx, tx, tid.UUID(), req.RateCards); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM openrails.catalog_credit_purchase_prices WHERE merchant_id = $1`, tid.UUID()); err != nil {
			return err
		}
		if err := syncCreditBalances(ctx, tx, tid.UUID(), req.CreditBalances); err != nil {
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

func syncMeters(ctx context.Context, tx pgx.Tx, merchantID uuid.UUID, meters []CatalogMeterSpec) error {
	if _, err := tx.Exec(ctx, `DELETE FROM openrails.catalog_rate_cards WHERE merchant_id = $1 AND customer_id IS NULL`, merchantID); err != nil {
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
INSERT INTO openrails.catalog_meters (merchant_id, key, event_type, value_property, aggregation, unit, group_by)
VALUES ($1, $2, NULLIF($3, ''), NULLIF($4, ''), NULLIF($5, ''), NULLIF($6, ''), $7::jsonb)
ON CONFLICT (merchant_id, key) DO UPDATE
SET event_type = EXCLUDED.event_type,
    value_property = EXCLUDED.value_property,
    aggregation = EXCLUDED.aggregation,
    unit = EXCLUDED.unit,
    group_by = EXCLUDED.group_by,
    updated_at = now()`,
			merchantID, key, strings.TrimSpace(meter.EventType), strings.TrimSpace(meter.ValueProperty),
			strings.TrimSpace(meter.Aggregation), strings.TrimSpace(meter.Unit), string(groupBy)); err != nil {
			return fmt.Errorf("upsert meter %q: %w", key, err)
		}
	}

	if len(meterKeys) == 0 {
		_, err := tx.Exec(ctx, `DELETE FROM openrails.catalog_meters WHERE merchant_id = $1`, merchantID)
		return err
	}
	_, err := tx.Exec(ctx, `DELETE FROM openrails.catalog_meters WHERE merchant_id = $1 AND NOT (key = ANY($2::text[]))`, merchantID, meterKeys)
	return err
}

func syncRateCards(ctx context.Context, tx pgx.Tx, merchantID uuid.UUID, rateCards []CatalogRateCardSpec) error {
	if _, err := tx.Exec(ctx, `DELETE FROM openrails.catalog_rate_cards WHERE merchant_id = $1 AND customer_id IS NULL`, merchantID); err != nil {
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
    (merchant_id, product_id, ordinal, meter_key, payment_term, filter, allowance, price)
VALUES ($1, $2, $3, NULLIF($4, ''), $5, $6::jsonb, NULLIF($7, '')::jsonb, $8::jsonb)`,
			merchantID, productID, spec.Ordinal, strings.TrimSpace(spec.MeterKey), paymentTerm,
			string(filter), string(spec.Allowance), string(spec.Price)); err != nil {
			return fmt.Errorf("insert rate card %q #%d: %w", spec.ProductKey, spec.Ordinal, err)
		}
	}
	return nil
}

func syncCreditBalances(ctx context.Context, tx pgx.Tx, merchantID uuid.UUID, balances []CatalogCreditBalanceSpec) error {
	keys := make([]string, 0, len(balances))
	for _, spec := range balances {
		key := strings.TrimSpace(spec.Key)
		keys = append(keys, key)
		if _, err := tx.Exec(ctx, `
INSERT INTO openrails.catalog_credit_balances (merchant_id, key, unit, expires_hours)
VALUES ($1, $2, $3, $4)
ON CONFLICT (merchant_id, key) DO UPDATE
SET unit = EXCLUDED.unit, expires_hours = EXCLUDED.expires_hours, updated_at = now()`,
			merchantID, key, strings.TrimSpace(spec.Unit), spec.ExpiresHours); err != nil {
			return fmt.Errorf("upsert credit balance %q: %w", key, err)
		}
		if err := defineCustomCreditUnit(ctx, tx, merchantID, key, spec.Unit); err != nil {
			return err
		}
	}
	if len(keys) == 0 {
		_, err := tx.Exec(ctx, `DELETE FROM openrails.catalog_credit_balances WHERE merchant_id = $1`, merchantID)
		return err
	}
	_, err := tx.Exec(ctx, `DELETE FROM openrails.catalog_credit_balances WHERE merchant_id = $1 AND NOT (key = ANY($2::text[]))`, merchantID, keys)
	return err
}

// defineCustomCreditUnit auto-defines (#706) the custom_credit_types row a
// custom-unit credit balance needs at runtime: the ledger's ResolveUnit
// hard-rejects qualified units without an active row, and the credit-purchase
// flow qualifies units — so the manifest is the registry's writer. Built-in
// currencies need no row. Removed balances deliberately leave their type
// ACTIVE: grants/balances may still reference the unit, and entitlements must
// never be lost to catalog churn. Decimals are 0 (credits are whole units;
// decimals only scale display).
func defineCustomCreditUnit(ctx context.Context, tx pgx.Tx, merchantID uuid.UUID, balanceKey, unit string) error {
	unit = strings.ToLower(strings.TrimSpace(unit))
	name := unit
	if slug, qualified, ok := strings.Cut(unit, "/"); ok {
		// A qualified unit is only resolvable in its owning merchant's ledger —
		// declaring another merchant's slug would push an unusable balance.
		var merchantSlug string
		if err := tx.QueryRow(ctx, `SELECT slug FROM openrails.merchants WHERE id = $1`, merchantID).Scan(&merchantSlug); err != nil {
			return fmt.Errorf("resolve merchant slug: %w", err)
		}
		if slug != merchantSlug {
			return fmt.Errorf("credit balance %q unit %q is qualified with slug %q, not this merchant's %q", balanceKey, unit, slug, merchantSlug)
		}
		name = qualified
	} else if _, builtin := moneyutil.CurrencyScale(unit); builtin {
		return nil
	}
	if name == "" || strings.ContainsAny(name, "/ ") {
		return fmt.Errorf("credit balance %q has invalid custom credit unit %q", balanceKey, unit)
	}
	if _, err := tx.Exec(ctx, `
INSERT INTO openrails.custom_credit_types (id, merchant_id, name, decimals, active)
VALUES (uuidv7(), $1, $2, 0, true)
ON CONFLICT (merchant_id, name) DO UPDATE
SET active = true, updated_at = now()`,
		merchantID, name); err != nil {
		return fmt.Errorf("define custom credit unit %q: %w", unit, err)
	}
	return nil
}

func syncCreditPurchases(ctx context.Context, tx pgx.Tx, merchantID uuid.UUID, purchases []CatalogCreditPurchasePriceSpec) error {
	if _, err := tx.Exec(ctx, `DELETE FROM openrails.catalog_credit_purchase_prices WHERE merchant_id = $1`, merchantID); err != nil {
		return err
	}
	for _, spec := range purchases {
		productID, err := resolveProductID(ctx, tx, merchantID, spec.ProductKey)
		if err != nil {
			return err
		}
		// rails is NOT NULL with a '{}' default; a nil slice encodes as SQL NULL
		// and would violate the constraint, so coalesce to an empty array.
		providers := spec.PSPs
		if providers == nil {
			providers = []string{}
		}
		if _, err := tx.Exec(ctx, `
INSERT INTO openrails.catalog_credit_purchase_prices
    (merchant_id, product_id, ordinal, credit_key, currency, rails, input_min, input_max, price)
VALUES ($1, $2, $3, $4, $5, $6::text[], $7, $8, $9::jsonb)`,
			merchantID, productID, spec.Ordinal, strings.TrimSpace(spec.CreditKey), strings.TrimSpace(spec.Currency),
			providers, spec.InputMin, spec.InputMax, string(spec.Price)); err != nil {
			return fmt.Errorf("insert credit purchase %q #%d: %w", spec.ProductKey, spec.Ordinal, err)
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

func resolveProductID(ctx context.Context, tx pgx.Tx, merchantID uuid.UUID, key string) (uuid.UUID, error) {
	var id uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT id FROM openrails.products WHERE merchant_id = $1 AND key = $2`, merchantID, strings.TrimSpace(key)).Scan(&id); err != nil {
		return uuid.Nil, fmt.Errorf("resolve product %q: %w", key, err)
	}
	return id, nil
}
