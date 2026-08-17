package money

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/pkg/merchant"
	"github.com/open-rails/openrails/pkg/pricing"
)

const (
	defaultMeteringPageSize = 50
	maxMeteringPageSize     = 200
)

var (
	ErrUsageMeterNotFound       = errors.New("usage meter not found")
	ErrMeterInUse               = errors.New("usage meter has recorded activity")
	ErrDefaultRateCardNotFound  = errors.New("default usage rate card not found")
	ErrDefaultRateCardRequired  = errors.New("default usage rate card required")
	ErrRateCardHasOverrides     = errors.New("usage rate card has payer overrides")
	ErrRateCardCurrencyMismatch = errors.New("usage rate card currency must match default")
	ErrRateCardProductNotFound  = errors.New("usage rate card product not found")
	ErrAllowanceMeterNotFound   = errors.New("usage rate card allowance meter not found")
)

// UsageMeter is one merchant-scoped usage stream and its optional default
// rate card, projected with billing activity and override state.
type UsageMeter struct {
	Key                string                `json:"key"`
	EventType          string                `json:"event_type,omitempty"`
	EffectiveEventType string                `json:"effective_event_type"`
	ValueProperty      string                `json:"value_property,omitempty"`
	Aggregation        string                `json:"aggregation"`
	Unit               string                `json:"unit,omitempty"`
	GroupBy            map[string]string     `json:"group_by"`
	BillingSupported   bool                  `json:"billing_supported"`
	DefaultRateCard    *DefaultUsageRateCard `json:"default_rate_card,omitempty"`
	OverrideCount      int64                 `json:"override_count"`
	HasActivity        bool                  `json:"has_activity"`
	LastEventAt        *time.Time            `json:"last_event_at,omitempty"`
	CreatedAt          time.Time             `json:"created_at"`
	UpdatedAt          time.Time             `json:"updated_at"`
}

// DefaultUsageRateCard is the merchant-default in-arrears price for a meter.
type DefaultUsageRateCard struct {
	ID         uuid.UUID           `json:"id"`
	ProductID  uuid.UUID           `json:"product_id"`
	ProductKey string              `json:"product_key"`
	Filter     map[string][]string `json:"filter"`
	Price      pricing.RatePrice   `json:"price"`
	Allowance  *pricing.Allowance  `json:"allowance,omitempty"`
	CreatedAt  time.Time           `json:"created_at"`
	UpdatedAt  time.Time           `json:"updated_at"`
}

// UsageMeterPage is a bounded offset page of meters.
type UsageMeterPage struct {
	Items  []UsageMeter
	Total  int64
	Limit  int
	Offset int
}

// UsageMeterOverride is one negotiated payer price for a meter.
type UsageMeterOverride struct {
	CustomerID uuid.UUID          `json:"customer_id"`
	Subject    string             `json:"subject,omitempty"`
	Email      string             `json:"email,omitempty"`
	Price      pricing.RatePrice  `json:"price"`
	Allowance  *pricing.Allowance `json:"allowance,omitempty"`
	CreatedAt  time.Time          `json:"created_at"`
	UpdatedAt  time.Time          `json:"updated_at"`
}

// UsageMeterOverridePage is a bounded offset page of payer overrides.
type UsageMeterOverridePage struct {
	Items  []UsageMeterOverride
	Total  int64
	Limit  int
	Offset int
}

// ListUsageMeters returns meters ordered by their canonical key.
func (s *MoneyService) ListUsageMeters(ctx context.Context, limit, offset int) (UsageMeterPage, error) {
	page := UsageMeterPage{}
	if s == nil || s.db == nil {
		return page, fmt.Errorf("money service not initialized")
	}
	page.Limit, page.Offset = normalizeMeteringPage(limit, offset)
	tenant, err := merchant.Require(ctx)
	if err != nil {
		return page, err
	}

	err = s.db.RunInMerchantConn(ctx, func(ctx context.Context) error {
		if err := s.db.Qx(ctx).QueryRow(ctx, `
SELECT count(*)
FROM openrails.catalog_meters
WHERE merchant_id = $1`, tenant.UUID()).Scan(&page.Total); err != nil {
			return fmt.Errorf("count usage meters: %w", err)
		}

		rows, err := s.db.Qx(ctx).Query(
			ctx,
			usageMeterSelect+` ORDER BY meter.key LIMIT $2 OFFSET $3`,
			tenant.UUID(),
			page.Limit,
			page.Offset,
		)
		if err != nil {
			return fmt.Errorf("list usage meters: %w", err)
		}
		defer rows.Close()

		page.Items = make([]UsageMeter, 0, page.Limit)
		for rows.Next() {
			meter, err := scanUsageMeter(rows)
			if err != nil {
				return err
			}
			page.Items = append(page.Items, meter)
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("iterate usage meters: %w", err)
		}
		return nil
	})
	return page, err
}

// GetUsageMeter returns one meter and its optional default rate card.
func (s *MoneyService) GetUsageMeter(ctx context.Context, meterKey string) (*UsageMeter, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("money service not initialized")
	}
	meterKey = pricing.NormalizeKey(meterKey)
	if meterKey == "" {
		return nil, fmt.Errorf("meter key required")
	}
	tenant, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}

	var meter UsageMeter
	err = s.db.RunInMerchantConn(ctx, func(ctx context.Context) error {
		row := s.db.Qx(ctx).QueryRow(
			ctx,
			usageMeterSelect+` AND meter.key = $2`,
			tenant.UUID(),
			meterKey,
		)
		var err error
		meter, err = scanUsageMeter(row)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrUsageMeterNotFound
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	return &meter, nil
}

// ListUsageMeterOverrides returns negotiated payer prices for one meter.
func (s *MoneyService) ListUsageMeterOverrides(
	ctx context.Context,
	meterKey string,
	limit int,
	offset int,
) (UsageMeterOverridePage, error) {
	page := UsageMeterOverridePage{}
	if s == nil || s.db == nil {
		return page, fmt.Errorf("money service not initialized")
	}
	meterKey = pricing.NormalizeKey(meterKey)
	if meterKey == "" {
		return page, fmt.Errorf("meter key required")
	}
	page.Limit, page.Offset = normalizeMeteringPage(limit, offset)
	tenant, err := merchant.Require(ctx)
	if err != nil {
		return page, err
	}

	err = s.db.RunInMerchantConn(ctx, func(ctx context.Context) error {
		var exists bool
		if err := s.db.Qx(ctx).QueryRow(ctx, `
SELECT EXISTS (
    SELECT 1 FROM openrails.catalog_meters
    WHERE merchant_id = $1 AND key = $2
)`, tenant.UUID(), meterKey).Scan(&exists); err != nil {
			return fmt.Errorf("check usage meter: %w", err)
		}
		if !exists {
			return ErrUsageMeterNotFound
		}

		if err := s.db.Qx(ctx).QueryRow(ctx, `
SELECT count(*)
FROM openrails.catalog_rate_cards
WHERE merchant_id = $1 AND meter_key = $2 AND customer_id IS NOT NULL`, tenant.UUID(), meterKey).Scan(&page.Total); err != nil {
			return fmt.Errorf("count usage meter overrides: %w", err)
		}

		rows, err := s.db.Qx(ctx).Query(ctx, `
SELECT card.customer_id,
       COALESCE(customer.subject, ''),
       COALESCE((
           SELECT BTRIM(subscription.user_email)
           FROM openrails.subscriptions subscription
           WHERE subscription.merchant_id = card.merchant_id
             AND subscription.customer_id = card.customer_id
             AND subscription.deleted_at IS NULL
             AND NULLIF(BTRIM(subscription.user_email), '') IS NOT NULL
           ORDER BY subscription.created_at DESC, subscription.id DESC
           LIMIT 1
       ), ''),
       card.price,
       card.allowance,
       card.created_at,
       card.updated_at
FROM openrails.catalog_rate_cards card
JOIN openrails.customers customer
  ON customer.merchant_id = card.merchant_id
 AND customer.id = card.customer_id
WHERE card.merchant_id = $1
  AND card.meter_key = $2
  AND card.customer_id IS NOT NULL
ORDER BY card.updated_at DESC, card.customer_id
LIMIT $3 OFFSET $4`, tenant.UUID(), meterKey, page.Limit, page.Offset)
		if err != nil {
			return fmt.Errorf("list usage meter overrides: %w", err)
		}
		defer rows.Close()

		page.Items = make([]UsageMeterOverride, 0, page.Limit)
		for rows.Next() {
			var item UsageMeterOverride
			var priceJSON, allowanceJSON []byte
			if err := rows.Scan(
				&item.CustomerID,
				&item.Subject,
				&item.Email,
				&priceJSON,
				&allowanceJSON,
				&item.CreatedAt,
				&item.UpdatedAt,
			); err != nil {
				return fmt.Errorf("scan usage meter override: %w", err)
			}
			if err := decodeRateCard(priceJSON, allowanceJSON, &item.Price, &item.Allowance); err != nil {
				return fmt.Errorf("decode usage meter override for customer %s: %w", item.CustomerID, err)
			}
			page.Items = append(page.Items, item)
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("iterate usage meter overrides: %w", err)
		}
		return nil
	})
	return page, err
}

const usageMeterSelect = `
WITH activity AS (
    SELECT event_type, count(*) AS event_count, max(occurred_at) AS last_event_at
    FROM openrails.usage_events
    WHERE merchant_id = $1
    GROUP BY event_type
), override_counts AS (
    SELECT meter_key, count(*) AS override_count
    FROM openrails.catalog_rate_cards
    WHERE merchant_id = $1 AND customer_id IS NOT NULL
    GROUP BY meter_key
)
SELECT meter.key,
       COALESCE(meter.event_type, ''),
       COALESCE(NULLIF(meter.event_type, ''), meter.key) AS effective_event_type,
       COALESCE(meter.value_property, ''),
       COALESCE(meter.aggregation, ''),
       COALESCE(meter.unit, ''),
       meter.group_by,
       meter.created_at,
       meter.updated_at,
       COALESCE(override_counts.override_count, 0),
       COALESCE(activity.event_count, 0) > 0 AS has_activity,
       activity.last_event_at,
       card.id,
       card.product_id,
       COALESCE(product.key, ''),
       card.filter,
       card.price,
       card.allowance,
       card.created_at,
       card.updated_at
FROM openrails.catalog_meters meter
LEFT JOIN activity
  ON activity.event_type = COALESCE(NULLIF(meter.event_type, ''), meter.key)
LEFT JOIN override_counts
  ON override_counts.meter_key = meter.key
LEFT JOIN openrails.catalog_rate_cards card
  ON card.merchant_id = meter.merchant_id
 AND card.meter_key = meter.key
 AND card.customer_id IS NULL
LEFT JOIN openrails.products product
  ON product.merchant_id = card.merchant_id
 AND product.id = card.product_id
WHERE meter.merchant_id = $1`

type usageMeterScanner interface {
	Scan(dest ...any) error
}

func scanUsageMeter(row usageMeterScanner) (UsageMeter, error) {
	var meter UsageMeter
	var groupByJSON []byte
	var cardID, productID *uuid.UUID
	var productKey *string
	var filterJSON, priceJSON, allowanceJSON []byte
	var cardCreatedAt, cardUpdatedAt *time.Time
	if err := row.Scan(
		&meter.Key,
		&meter.EventType,
		&meter.EffectiveEventType,
		&meter.ValueProperty,
		&meter.Aggregation,
		&meter.Unit,
		&groupByJSON,
		&meter.CreatedAt,
		&meter.UpdatedAt,
		&meter.OverrideCount,
		&meter.HasActivity,
		&meter.LastEventAt,
		&cardID,
		&productID,
		&productKey,
		&filterJSON,
		&priceJSON,
		&allowanceJSON,
		&cardCreatedAt,
		&cardUpdatedAt,
	); err != nil {
		return meter, err
	}
	if err := json.Unmarshal(groupByJSON, &meter.GroupBy); err != nil {
		return meter, fmt.Errorf("decode meter %q group_by: %w", meter.Key, err)
	}
	if meter.GroupBy == nil {
		meter.GroupBy = map[string]string{}
	}
	meter.BillingSupported = pricing.BillingSupported(meter.Aggregation)
	if cardID == nil {
		return meter, nil
	}
	if productID == nil || productKey == nil || cardCreatedAt == nil || cardUpdatedAt == nil {
		return meter, fmt.Errorf("meter %q default rate card is incomplete", meter.Key)
	}
	card := DefaultUsageRateCard{
		ID:         *cardID,
		ProductID:  *productID,
		ProductKey: *productKey,
		CreatedAt:  *cardCreatedAt,
		UpdatedAt:  *cardUpdatedAt,
	}
	if err := json.Unmarshal(filterJSON, &card.Filter); err != nil {
		return meter, fmt.Errorf("decode meter %q rate card filter: %w", meter.Key, err)
	}
	if card.Filter == nil {
		card.Filter = map[string][]string{}
	}
	if err := decodeRateCard(priceJSON, allowanceJSON, &card.Price, &card.Allowance); err != nil {
		return meter, fmt.Errorf("decode meter %q default rate card: %w", meter.Key, err)
	}
	meter.DefaultRateCard = &card
	return meter, nil
}

func decodeRateCard(
	priceJSON []byte,
	allowanceJSON []byte,
	price *pricing.RatePrice,
	allowance **pricing.Allowance,
) error {
	if err := json.Unmarshal(priceJSON, price); err != nil {
		return fmt.Errorf("decode price: %w", err)
	}
	if len(allowanceJSON) == 0 {
		return nil
	}
	var value pricing.Allowance
	if err := json.Unmarshal(allowanceJSON, &value); err != nil {
		return fmt.Errorf("decode allowance: %w", err)
	}
	*allowance = &value
	return nil
}

func normalizeMeteringPage(limit, offset int) (int, int) {
	if limit <= 0 {
		limit = defaultMeteringPageSize
	}
	if limit > maxMeteringPageSize {
		limit = maxMeteringPageSize
	}
	if offset < 0 {
		offset = 0
	}
	return limit, offset
}

func usageMeterSemanticsEqual(left, right pricing.Meter) bool {
	return left.EventType == right.EventType &&
		left.ValueProperty == right.ValueProperty &&
		left.Aggregation == right.Aggregation &&
		left.Unit == right.Unit &&
		maps.Equal(left.GroupBy, right.GroupBy)
}

func usageMeterHasActivity(
	ctx context.Context,
	tx pgx.Tx,
	merchantID uuid.UUID,
	existing pricing.Meter,
	replacement pricing.Meter,
) (bool, error) {
	eventTypes := []string{effectiveMeterEventType(existing)}
	replacementEventType := effectiveMeterEventType(replacement)
	if replacementEventType != eventTypes[0] {
		eventTypes = append(eventTypes, replacementEventType)
	}
	// Meter corrections are rare control-plane writes. Hold inserts while the
	// activity predicate is checked so a concurrent report cannot slip between
	// the check and the semantic update and then be reinterpreted.
	if _, err := tx.Exec(ctx, `LOCK TABLE openrails.usage_events IN SHARE ROW EXCLUSIVE MODE`); err != nil {
		return false, fmt.Errorf("lock usage activity: %w", err)
	}
	var hasActivity bool
	if err := tx.QueryRow(ctx, `
SELECT EXISTS (
    SELECT 1
    FROM openrails.usage_events
    WHERE merchant_id = $1 AND event_type = ANY($2::text[])
)`, merchantID, eventTypes).Scan(&hasActivity); err != nil {
		return false, fmt.Errorf("check usage meter activity: %w", err)
	}
	return hasActivity, nil
}

func effectiveMeterEventType(meter pricing.Meter) string {
	if meter.EventType != "" {
		return meter.EventType
	}
	return meter.Key
}

func loadUsageMeterForRateCard(
	ctx context.Context,
	tx pgx.Tx,
	merchantID uuid.UUID,
	meterKey string,
) (pricing.Meter, error) {
	var meter pricing.Meter
	var groupByJSON []byte
	err := tx.QueryRow(ctx, `
SELECT key,
       COALESCE(event_type, ''),
       COALESCE(value_property, ''),
       COALESCE(aggregation, ''),
       COALESCE(unit, ''),
       group_by
FROM openrails.catalog_meters
WHERE merchant_id = $1 AND key = $2
FOR UPDATE`, merchantID, meterKey).Scan(
		&meter.Key,
		&meter.EventType,
		&meter.ValueProperty,
		&meter.Aggregation,
		&meter.Unit,
		&groupByJSON,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return meter, ErrUsageMeterNotFound
	}
	if err != nil {
		return meter, fmt.Errorf("load usage meter: %w", err)
	}
	if err := json.Unmarshal(groupByJSON, &meter.GroupBy); err != nil {
		return meter, fmt.Errorf("decode usage meter group_by: %w", err)
	}
	if meter.GroupBy == nil {
		meter.GroupBy = map[string]string{}
	}
	if err := pricing.ValidateMeter("usage meter", &meter); err != nil {
		return meter, err
	}
	return meter, nil
}

func ensureAllowanceMeter(
	ctx context.Context,
	tx pgx.Tx,
	merchantID uuid.UUID,
	allowance *pricing.Allowance,
) error {
	if allowance == nil || allowance.AccrueFrom == "" {
		return nil
	}
	var exists bool
	if err := tx.QueryRow(ctx, `
SELECT EXISTS (
    SELECT 1 FROM openrails.catalog_meters
    WHERE merchant_id = $1 AND key = $2
)`, merchantID, allowance.AccrueFrom).Scan(&exists); err != nil {
		return fmt.Errorf("check allowance source meter: %w", err)
	}
	if !exists {
		return fmt.Errorf("%w: %q", ErrAllowanceMeterNotFound, allowance.AccrueFrom)
	}
	return nil
}

func loadDefaultRateCardCurrency(
	ctx context.Context,
	tx pgx.Tx,
	merchantID uuid.UUID,
	meterKey string,
) (string, error) {
	var priceJSON []byte
	err := tx.QueryRow(ctx, `
SELECT price
FROM openrails.catalog_rate_cards
WHERE merchant_id = $1 AND meter_key = $2 AND customer_id IS NULL
FOR UPDATE`, merchantID, meterKey).Scan(&priceJSON)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrDefaultRateCardRequired
	}
	if err != nil {
		return "", fmt.Errorf("load default usage rate card: %w", err)
	}
	var price pricing.RatePrice
	if err := json.Unmarshal(priceJSON, &price); err != nil {
		return "", fmt.Errorf("decode default usage rate card price: %w", err)
	}
	return strings.ToUpper(strings.TrimSpace(price.Currency)), nil
}

func ensureActiveProduct(
	ctx context.Context,
	tx pgx.Tx,
	merchantID uuid.UUID,
	productID uuid.UUID,
) error {
	var id uuid.UUID
	err := tx.QueryRow(ctx, `
SELECT id
FROM openrails.products
WHERE merchant_id = $1 AND id = $2 AND NOT archived
FOR SHARE`, merchantID, productID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrRateCardProductNotFound
	}
	if err != nil {
		return fmt.Errorf("check rate card product: %w", err)
	}
	return nil
}
