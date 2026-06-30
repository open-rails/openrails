package money

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/pkg/identity"
	"github.com/open-rails/openrails/pkg/merchant"
	"github.com/open-rails/openrails/pkg/pricing"
)

type MeterKind string

const (
	MeterKindCounter MeterKind = "counter"
	MeterKindGauge   MeterKind = "gauge"
)

type MeteredRate struct {
	Kind       MeterKind
	RateMicros int64
	PerUnits   int64
	Per        time.Duration
}

// RateMeteredAggregate computes usage cost in micros, rounding once after the
// full period aggregate is known. Counter aggregates are unit counts. Gauge
// aggregates are unit-seconds.
func RateMeteredAggregate(aggregate int64, rate MeteredRate) (int64, error) {
	if aggregate < 0 {
		return 0, fmt.Errorf("aggregate must be >= 0")
	}
	if rate.RateMicros <= 0 {
		return 0, fmt.Errorf("rate must be positive")
	}
	perUnits := rate.PerUnits
	if perUnits == 0 {
		perUnits = 1
	}
	if perUnits < 1 {
		return 0, fmt.Errorf("per_units must be >= 1")
	}
	denom := perUnits
	switch rate.Kind {
	case MeterKindCounter:
	case MeterKindGauge:
		if rate.Per <= 0 {
			return 0, fmt.Errorf("gauge rate requires a positive per duration")
		}
		denom *= int64(rate.Per / time.Second)
	default:
		return 0, fmt.Errorf("unknown meter kind %q", rate.Kind)
	}
	return divRoundHalfUp(aggregate, rate.RateMicros, denom)
}

func (s *MoneyService) AccrueMeteredAggregate(ctx context.Context, payer identity.CustomerID, currency, meter, sourceID string, aggregate int64, rate MeteredRate) (int64, error) {
	meter = strings.TrimSpace(meter)
	if meter == "" {
		return 0, fmt.Errorf("meter required")
	}
	amount, err := RateMeteredAggregate(aggregate, rate)
	if err != nil {
		return 0, err
	}
	if amount <= 0 {
		return 0, nil
	}
	if _, err := s.AccrueOwed(ctx, payer, currency, "metered:"+meter, sourceID, amount); err != nil {
		return 0, err
	}
	return amount, nil
}

func (s *MoneyService) AccrueCatalogMeteredAggregate(ctx context.Context, payer identity.CustomerID, currency string, priceID uuid.UUID, sourceID string, aggregate int64) (int64, error) {
	if priceID == uuid.Nil {
		return 0, fmt.Errorf("price_id required")
	}
	meter, rate, err := s.lookupCatalogMeteredRate(ctx, priceID)
	if err != nil {
		return 0, err
	}
	return s.AccrueMeteredAggregate(ctx, payer, currency, meter, sourceID, aggregate, rate)
}

type catalogRateCardRow struct {
	ID          uuid.UUID
	MeterKey    string
	EventType   string
	ValueKey    string
	Aggregation string
	GroupBy     map[string]string
	Filter      map[string][]string
	Allowance   *pricing.Allowance
	Price       pricing.RatePrice
}

func (s *MoneyService) sweepCatalogRateCardUsage(ctx context.Context, payer identity.CustomerID, currency string, from, to time.Time) error {
	if payer.IsZero() {
		return fmt.Errorf("payer required")
	}
	if !to.After(from) {
		return fmt.Errorf("invalid period: to must be after from")
	}
	cur := normalizeCurrency(currency)
	if err := ValidateCurrency(cur); err != nil {
		return err
	}
	tid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	rateCards, err := s.loadCatalogRateCards(ctx, tid.UUID(), cur)
	if err != nil {
		return err
	}
	period := meteredPeriodSourceID(from, to)
	for _, rc := range rateCards {
		includedOverride := int64(0)
		if rc.Allowance != nil && strings.TrimSpace(rc.Allowance.AccrueFrom) != "" {
			source, ok := findAllowanceSourceRateCard(rateCards, rc.Allowance.AccrueFrom)
			if !ok {
				return fmt.Errorf("rate card %s allowance references unknown source meter %q", rc.ID, rc.Allowance.AccrueFrom)
			}
			includedOverride, err = s.accruedAllowanceUnits(ctx, tid.UUID(), payer, cur, source, rc.Allowance, from.UTC(), to.UTC())
			if err != nil {
				return err
			}
		}
		dimensionKey := ""
		if rc.Price.PerUnit != nil && rc.Price.PerUnit.Matrix != nil {
			dimensionKey = rc.Price.PerUnit.Matrix.Dimension
		}
		groupProperty := propertyKey(rc.GroupBy[dimensionKey])
		if groupProperty == "" {
			groupProperty = dimensionKey
		}
		aggregates, err := s.aggregateRateCardUsage(ctx, tid.UUID(), payer, cur, rc, groupProperty, from.UTC(), to.UTC())
		if err != nil {
			return err
		}
		for dimValue, quantity := range aggregates {
			if quantity <= 0 || !rateCardFilterAllows(rc.Filter, dimensionKey, dimValue) {
				continue
			}
			amount, err := rateCatalogRateCardUsage(rc.Price, rc.Allowance, dimValue, quantity, includedOverride)
			if err != nil {
				return err
			}
			if amount <= 0 {
				continue
			}
			source := "metered:" + rc.MeterKey + ":rate_card:" + rc.ID.String()
			sourceID := period
			if dimValue != "" {
				sourceID += ":dim:" + dimValue
			}
			if _, err := s.AccrueOwed(ctx, payer, cur, source, sourceID, amount); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *MoneyService) loadCatalogRateCards(ctx context.Context, merchantID uuid.UUID, currency string) ([]catalogRateCardRow, error) {
	var out []catalogRateCardRow
	err := s.db.RunInMerchantConn(ctx, func(ctx context.Context) error {
		rows, qerr := s.db.Pool().Query(ctx, `
SELECT rc.id,
       rc.meter_key,
       COALESCE(NULLIF(cm.event_type, ''), cm.key) AS event_type,
       COALESCE(NULLIF(cm.value_property, ''), cm.key) AS value_property,
       COALESCE(NULLIF(cm.aggregation, ''), CASE WHEN cm.kind = 'counter' THEN 'count' ELSE 'sum' END) AS aggregation,
       COALESCE(cm.group_by, '{}'::jsonb) AS group_by,
       COALESCE(rc.filter, '{}'::jsonb) AS filter,
       rc.allowance,
       rc.price
FROM openrails.catalog_rate_cards rc
JOIN openrails.catalog_meters cm
  ON cm.merchant_id = rc.merchant_id AND cm.key = rc.meter_key
WHERE rc.merchant_id = $1
  AND rc.meter_key IS NOT NULL
  AND COALESCE(rc.price ->> 'currency', $2) = $2
  AND rc.payment_term = 'in_arrears'
ORDER BY rc.ordinal`, merchantID, currency)
		if qerr != nil {
			return qerr
		}
		defer rows.Close()
		for rows.Next() {
			var row catalogRateCardRow
			var groupByJSON, filterJSON, priceJSON []byte
			var allowanceJSON []byte
			if err := rows.Scan(&row.ID, &row.MeterKey, &row.EventType, &row.ValueKey, &row.Aggregation, &groupByJSON, &filterJSON, &allowanceJSON, &priceJSON); err != nil {
				return err
			}
			if err := json.Unmarshal(groupByJSON, &row.GroupBy); err != nil {
				return fmt.Errorf("decode rate card %s group_by: %w", row.ID, err)
			}
			if err := json.Unmarshal(filterJSON, &row.Filter); err != nil {
				return fmt.Errorf("decode rate card %s filter: %w", row.ID, err)
			}
			if len(allowanceJSON) > 0 {
				var allowance pricing.Allowance
				if err := json.Unmarshal(allowanceJSON, &allowance); err != nil {
					return fmt.Errorf("decode rate card %s allowance: %w", row.ID, err)
				}
				row.Allowance = &allowance
			}
			if err := json.Unmarshal(priceJSON, &row.Price); err != nil {
				return fmt.Errorf("decode rate card %s price: %w", row.ID, err)
			}
			row.ValueKey = propertyKey(row.ValueKey)
			out = append(out, row)
		}
		return rows.Err()
	})
	return out, err
}

func (s *MoneyService) aggregateRateCardUsage(ctx context.Context, merchantID uuid.UUID, payer identity.CustomerID, currency string, rc catalogRateCardRow, groupProperty string, from, to time.Time) (map[string]int64, error) {
	valueKey := propertyKey(rc.ValueKey)
	if valueKey == "" {
		valueKey = rc.MeterKey
	}
	agg := strings.ToLower(strings.TrimSpace(rc.Aggregation))
	if agg == "" {
		agg = "sum"
	}
	if agg != "sum" && agg != "count" {
		return nil, fmt.Errorf("meter %q aggregation %q is not supported for billing", rc.MeterKey, agg)
	}
	out := map[string]int64{}
	err := s.db.RunInMerchantConn(ctx, func(ctx context.Context) error {
		rows, qerr := s.db.Pool().Query(ctx, `
SELECT COALESCE(NULLIF(ue.metadata ->> $8, ''), NULLIF(ue.dimensions ->> $8, ''), '') AS dim_value,
       COALESCE(SUM(
           CASE WHEN $9 = 'count' THEN 1
                ELSE COALESCE((ue.dimensions ->> $7)::bigint, (ue.metadata ->> $7)::bigint, 0)
           END
       ), 0)::bigint AS quantity
FROM openrails.usage_events ue
WHERE ue.merchant_id = $1
  AND ue.customer_id = $2
  AND ue.currency = $3
  AND ue.event_type = $4
  AND ue.occurred_at >= $5::timestamptz
  AND ue.occurred_at < $6::timestamptz
GROUP BY dim_value`, merchantID, payer.UUID(), currency, rc.EventType, from, to, valueKey, groupProperty, agg)
		if qerr != nil {
			return qerr
		}
		defer rows.Close()
		for rows.Next() {
			var dimValue string
			var quantity int64
			if err := rows.Scan(&dimValue, &quantity); err != nil {
				return err
			}
			out[dimValue] += quantity
		}
		return rows.Err()
	})
	return out, err
}

func (s *MoneyService) accruedAllowanceUnits(ctx context.Context, merchantID uuid.UUID, payer identity.CustomerID, currency string, source catalogRateCardRow, allowance *pricing.Allowance, from, to time.Time) (int64, error) {
	if allowance == nil || strings.TrimSpace(allowance.AccrueFrom) == "" {
		return 0, nil
	}
	if source.Price.PerUnit == nil || source.Price.PerUnit.Matrix == nil {
		return 0, fmt.Errorf("allowance source meter %q must use matrix cells with included units", source.MeterKey)
	}
	dimensionKey := source.Price.PerUnit.Matrix.Dimension
	dimensionProperty := propertyKey(source.GroupBy[dimensionKey])
	resourceProperty := propertyKey(source.GroupBy["resource_id"])
	if dimensionProperty == "" || resourceProperty == "" {
		return 0, fmt.Errorf("allowance source meter %q requires group_by %q and resource_id", source.MeterKey, dimensionKey)
	}
	capSeconds, err := allowanceCapSeconds(allowance.Cap)
	if err != nil {
		return 0, err
	}
	valueKey := propertyKey(source.ValueKey)
	if valueKey == "" {
		valueKey = source.MeterKey
	}
	agg := strings.ToLower(strings.TrimSpace(source.Aggregation))
	if agg == "" {
		agg = "sum"
	}
	if agg != "sum" && agg != "count" {
		return 0, fmt.Errorf("allowance source meter %q aggregation %q is not supported", source.MeterKey, agg)
	}

	total := int64(0)
	err = s.db.RunInMerchantConn(ctx, func(ctx context.Context) error {
		rows, qerr := s.db.Pool().Query(ctx, `
SELECT COALESCE(NULLIF(ue.metadata ->> $8, ''), NULLIF(ue.dimensions ->> $8, ''), '') AS dim_value,
       COALESCE(NULLIF(ue.metadata ->> $9, ''), NULLIF(ue.dimensions ->> $9, ''), '') AS resource_id,
       COALESCE(SUM(
           CASE WHEN $10 = 'count' THEN 1
                ELSE COALESCE((ue.dimensions ->> $7)::bigint, (ue.metadata ->> $7)::bigint, 0)
           END
       ), 0)::bigint AS quantity
FROM openrails.usage_events ue
WHERE ue.merchant_id = $1
  AND ue.customer_id = $2
  AND ue.currency = $3
  AND ue.event_type = $4
  AND ue.occurred_at >= $5::timestamptz
  AND ue.occurred_at < $6::timestamptz
GROUP BY dim_value, resource_id`, merchantID, payer.UUID(), currency, source.EventType, from, to, valueKey, dimensionProperty, resourceProperty, agg)
		if qerr != nil {
			return qerr
		}
		defer rows.Close()
		for rows.Next() {
			var dimValue, resourceID string
			var quantity int64
			if err := rows.Scan(&dimValue, &resourceID, &quantity); err != nil {
				return err
			}
			if resourceID == "" || quantity <= 0 {
				continue
			}
			cell, ok := source.Price.PerUnit.Matrix.Cells[dimValue]
			if !ok || cell.Included <= 0 {
				continue
			}
			effective := quantity
			if capSeconds > 0 && effective > capSeconds {
				effective = capSeconds
			}
			if capSeconds > 0 {
				allowanceUnits, err := pricing.ChargeModel{Kind: pricing.ModelPerUnit, UnitAmount: cell.Included, DivideBy: capSeconds, Round: pricing.RoundDown}.Rate(effective)
				if err != nil {
					return err
				}
				total += allowanceUnits
			} else {
				total += cell.Included
			}
		}
		return rows.Err()
	})
	return total, err
}

func findAllowanceSourceRateCard(rateCards []catalogRateCardRow, meter string) (catalogRateCardRow, bool) {
	meter = strings.TrimSpace(meter)
	for _, rc := range rateCards {
		if rc.MeterKey == meter {
			return rc, true
		}
	}
	return catalogRateCardRow{}, false
}

func allowanceCapSeconds(spec string) (int64, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return 0, nil
	}
	unit := spec[len(spec)-1:]
	n := int64(0)
	if _, err := fmt.Sscanf(spec[:len(spec)-1], "%d", &n); err != nil {
		return 0, fmt.Errorf("allowance cap %q: %w", spec, err)
	}
	switch unit {
	case "d":
		return n * 24 * 3600, nil
	case "h":
		return n * 3600, nil
	case "m":
		return n * 60, nil
	case "s":
		return n, nil
	default:
		return 0, fmt.Errorf("allowance cap %q must end in d/h/m/s", spec)
	}
}

func rateCatalogRateCardUsage(price pricing.RatePrice, allowance *pricing.Allowance, dimValue string, quantity, includedOverride int64) (int64, error) {
	cm := price.ToChargeModel()
	if price.PerUnit != nil && price.PerUnit.Matrix != nil {
		cellCM, ok := price.ChargeModelForCell(dimValue)
		if !ok {
			return 0, fmt.Errorf("rate card has no matrix cell for %q=%q", price.PerUnit.Matrix.Dimension, dimValue)
		}
		cm = cellCM
	}
	// A matrix cell's `included` is the SOURCE value that OTHER cards' allowances
	// draw from (via allowance.accrue_from / accruedAllowanceUnits), NOT a
	// self-allowance against this card's own usage. Only an explicit flat
	// allowance or an accrued override nets here.
	included := int64(0)
	if allowance != nil && allowance.Included > 0 {
		included = allowance.Included
	}
	if includedOverride > 0 {
		included = includedOverride
	}
	if included <= 0 || cm.Kind != pricing.ModelPerUnit {
		return cm.Rate(quantity)
	}
	unitCounter := pricing.ChargeModel{
		Kind:       pricing.ModelPerUnit,
		UnitAmount: 1,
		DivideBy:   cm.DivideBy,
		Round:      cm.Round,
	}
	pricedUnits, err := unitCounter.Rate(quantity)
	if err != nil {
		return 0, err
	}
	billable := pricedUnits - included
	if billable <= 0 {
		return 0, nil
	}
	cm.DivideBy = 1
	return cm.Rate(billable)
}

func rateCardFilterAllows(filter map[string][]string, dimensionKey, dimValue string) bool {
	if len(filter) == 0 || dimensionKey == "" {
		return true
	}
	allowed, ok := filter[dimensionKey]
	if !ok || len(allowed) == 0 {
		return true
	}
	for _, candidate := range allowed {
		if candidate == dimValue {
			return true
		}
	}
	return false
}

func propertyKey(property string) string {
	property = strings.TrimSpace(property)
	property = strings.TrimPrefix(property, "$.")
	property = strings.TrimPrefix(property, "dimensions.")
	property = strings.TrimPrefix(property, "metadata.")
	return property
}

// lookupCatalogMeteredRate resolves the catalog metered sidecar for priceID into
// its meter_key and MeteredRate (kind/rate/per). The sidecar is the source of
// truth for how reported usage is rated: catalog_price_metered carries the
// per-price rate, catalog_meters carries the meter kind, joined on
// (merchant_id, meter_key). RLS-scoped to the request merchant; explicit
// merchant_id predicate keeps it fail-closed.
func (s *MoneyService) lookupCatalogMeteredRate(ctx context.Context, priceID uuid.UUID) (string, MeteredRate, error) {
	tid, err := merchant.Require(ctx)
	if err != nil {
		return "", MeteredRate{}, err
	}
	var meter string
	var kind string
	var rateMicros int64
	var perUnits int64
	var perSeconds *int64
	err = s.db.RunInMerchantConn(ctx, func(ctx context.Context) error {
		return s.db.Qx(ctx).QueryRow(ctx, `
	SELECT cpm.meter_key, cm.kind, cpm.rate_micros, cpm.per_units, cpm.per_seconds
	FROM openrails.catalog_price_metered cpm
	JOIN openrails.catalog_meters cm
  ON cm.merchant_id = cpm.merchant_id AND cm.key = cpm.meter_key
WHERE cpm.merchant_id = $1 AND cpm.price_id = $2`,
			tid.UUID(), priceID).Scan(&meter, &kind, &rateMicros, &perUnits, &perSeconds)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return "", MeteredRate{}, fmt.Errorf("catalog metered price %s not found", priceID)
	}
	if err != nil {
		return "", MeteredRate{}, err
	}
	rate := MeteredRate{
		Kind:       MeterKind(kind),
		RateMicros: rateMicros,
		PerUnits:   perUnits,
	}
	if perSeconds != nil {
		rate.Per = time.Duration(*perSeconds) * time.Second
	}
	return meter, rate, nil
}

// RateMeteredUsageFromEvents is the bridge from reported usage (openrails.usage_events,
// written by RecordUsage) to catalog metered pricing on an invoice (#615). It
// resolves the sidecar meter_key+kind for priceID, aggregates the payer's matching
// usage over [from, to), and accrues the rated cost as a pending owed invoice item
// at a DETERMINISTIC period source_id so a re-finalize of the same window never
// double-accrues. Returns the rated amount in micros (0 when there is no usage).
//
// Aggregation convention (the meter_key IS the dimension key, and equals the
// usage event_type — RecordUsage records usage under event_type == meter_key):
//
//   - An event's metered quantity lives in dimensions[meter_key].
//   - counter meters: aggregate = SUM over matching events of
//     COALESCE((dimensions->>meter_key)::bigint, 1) — the per-event unit count,
//     defaulting to 1 (count the event itself) when the dimension is absent.
//   - gauge meters: aggregate = SUM over matching events of
//     COALESCE((dimensions->>meter_key)::bigint, 0) — the per-event unit-seconds,
//     0 when the dimension is absent.
//
// Events match on (merchant_id, customer_id, currency, event_type=meter_key,
// occurred_at ∈ [from, to)). RateMeteredAggregate then rounds once over the full
// period aggregate, so integer-micros math is exact and rate-once.
func (s *MoneyService) RateMeteredUsageFromEvents(ctx context.Context, payer identity.CustomerID, currency string, priceID uuid.UUID, from, to time.Time) (int64, error) {
	if priceID == uuid.Nil {
		return 0, fmt.Errorf("price_id required")
	}
	if payer.IsZero() {
		return 0, fmt.Errorf("payer required")
	}
	if !to.After(from) {
		return 0, fmt.Errorf("invalid period: to must be after from")
	}
	cur := normalizeCurrency(currency)
	if err := ValidateCurrency(cur); err != nil {
		return 0, err
	}
	meter, rate, err := s.lookupCatalogMeteredRate(ctx, priceID)
	if err != nil {
		return 0, err
	}
	tid, err := merchant.Require(ctx)
	if err != nil {
		return 0, err
	}

	// Per-event default contribution when dimensions[meter_key] is absent:
	// counter counts the row (1), gauge contributes nothing (0).
	missingDefault := int64(0)
	if rate.Kind == MeterKindCounter {
		missingDefault = 1
	}

	var aggregate int64
	err = s.db.RunInMerchantConn(ctx, func(ctx context.Context) error {
		return s.db.Pool().QueryRow(ctx, `
SELECT COALESCE(SUM(COALESCE((dimensions ->> $4)::bigint, $5::bigint)), 0)::bigint
FROM openrails.usage_events
WHERE merchant_id = $1 AND customer_id = $2 AND currency = $3
  AND event_type = $4
  AND occurred_at >= $6::timestamptz AND occurred_at < $7::timestamptz`,
			tid.UUID(), payer.UUID(), cur, meter, missingDefault, from.UTC(), to.UTC()).Scan(&aggregate)
	})
	if err != nil {
		return 0, err
	}
	if aggregate <= 0 {
		return 0, nil
	}

	return s.AccrueMeteredAggregate(ctx, payer, cur, meter, meteredPeriodSourceID(from, to), aggregate, rate)
}

// meteredPeriodSourceID derives a deterministic idempotency source_id for a
// metered accrual over [from, to). Re-running the sweep for the same window
// yields the same source_id, so AccrueOwed (idempotent on (payer, source,
// source_id)) refuses to double-accrue.
func meteredPeriodSourceID(from, to time.Time) string {
	return fmt.Sprintf("period:%d-%d", from.UTC().Unix(), to.UTC().Unix())
}

// sweepCatalogMeteredUsage rates every catalog metered sidecar the payer reported
// usage for in [from, to) into pending owed invoice items, BEFORE invoice
// finalization rolls them up. Bounded to "meters the customer actually reported
// usage for in the period" by joining distinct reported event_types to the
// merchant's catalog_price_metered.meter_key. Idempotent via the deterministic
// period source_id, so re-finalize is a no-op.
func (s *MoneyService) sweepCatalogMeteredUsage(ctx context.Context, payer identity.CustomerID, currency string, from, to time.Time) error {
	if err := s.sweepCatalogRateCardUsage(ctx, payer, currency, from, to); err != nil {
		return err
	}
	tid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	cur := normalizeCurrency(currency)

	var priceIDs []uuid.UUID
	err = s.db.RunInMerchantConn(ctx, func(ctx context.Context) error {
		rows, qerr := s.db.Pool().Query(ctx, `
SELECT DISTINCT cpm.price_id
FROM openrails.catalog_price_metered cpm
JOIN (
    SELECT DISTINCT event_type
    FROM openrails.usage_events
    WHERE merchant_id = $1 AND customer_id = $2 AND currency = $3
      AND occurred_at >= $4::timestamptz AND occurred_at < $5::timestamptz
) ue ON ue.event_type = cpm.meter_key
WHERE cpm.merchant_id = $1`,
			tid.UUID(), payer.UUID(), cur, from.UTC(), to.UTC())
		if qerr != nil {
			return qerr
		}
		defer rows.Close()
		for rows.Next() {
			var id uuid.UUID
			if serr := rows.Scan(&id); serr != nil {
				return serr
			}
			priceIDs = append(priceIDs, id)
		}
		return rows.Err()
	})
	if err != nil {
		return err
	}

	for _, priceID := range priceIDs {
		if _, err := s.RateMeteredUsageFromEvents(ctx, payer, cur, priceID, from, to); err != nil {
			return err
		}
	}
	return nil
}

func divRoundHalfUp(a, b, denom int64) (int64, error) {
	if denom <= 0 {
		return 0, fmt.Errorf("denominator must be positive")
	}
	n := new(big.Int).Mul(big.NewInt(a), big.NewInt(b))
	d := big.NewInt(denom)
	q, r := new(big.Int).QuoRem(n, d, new(big.Int))
	r.Mul(r, big.NewInt(2))
	if r.Cmp(d) >= 0 {
		q.Add(q, big.NewInt(1))
	}
	if !q.IsInt64() {
		return 0, fmt.Errorf("rated amount overflows int64")
	}
	return q.Int64(), nil
}
