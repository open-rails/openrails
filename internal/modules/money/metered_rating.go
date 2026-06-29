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
	tid, err := merchant.Require(ctx)
	if err != nil {
		return 0, err
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
		return 0, fmt.Errorf("catalog metered price %s not found", priceID)
	}
	if err != nil {
		return 0, err
	}
	rate := MeteredRate{
		Kind:       MeterKind(kind),
		RateMicros: rateMicros,
		PerUnits:   perUnits,
	}
	if perSeconds != nil {
		rate.Per = time.Duration(*perSeconds) * time.Second
	}
	return s.AccrueMeteredAggregate(ctx, payer, currency, meter, sourceID, aggregate, rate)
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
