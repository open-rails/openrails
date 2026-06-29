package money

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/pkg/identity"
	"github.com/open-rails/openrails/pkg/merchant"
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
		return s.db.Pool().QueryRow(ctx, `
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
