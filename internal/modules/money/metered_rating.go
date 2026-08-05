package money

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
	"github.com/open-rails/openrails/pkg/identity"
	"github.com/open-rails/openrails/pkg/merchant"
	"github.com/open-rails/openrails/pkg/pricing"
)

// Rate cards (#638) are the ONLY metered-pricing engine (#707) and, since
// or#893, the only metered-pricing INPUT: this sweep rates reported usage
// (openrails.usage_events) into pending owed invoice items through the #672
// per-period watermark.

type catalogRateCardRow struct {
	ID          uuid.UUID
	MeterKey    string
	PayerScoped bool
	EventType   string
	ValueKey    string
	Aggregation string
	GroupBy     map[string]string
	Filter         map[string][]string
	Allowance      *pricing.Allowance
	Price          pricing.RatePrice
}

func (s *MoneyService) sweepCatalogRateCardUsage(ctx context.Context, payer identity.CustomerID, currency string, from, to time.Time) error {
	if payer.IsZero() {
		return fmt.Errorf("payer required")
	}
	if !to.After(from) {
		return fmt.Errorf("invalid period: to must be after from")
	}
	cur := normalizeCurrency(currency)
	if err := moneyutil.ValidateCurrency(cur); err != nil {
		return err
	}
	tid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	rateCards, err := s.loadCatalogRateCards(ctx, tid.UUID(), payer, cur)
	if err != nil {
		return err
	}
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
			// The meter key — not the rate-card id — is the accrual/watermark
			// identity: rows are delete+reinserted with fresh ids on every push,
			// and the watermark must survive a mid-period re-push. One usage card
			// per meter is enforced by manifest validation + a partial unique index.
			source := "metered:" + rc.MeterKey
			if _, err := s.accrueMeteredPrefix(ctx, payer, cur, source, dimValue, from, to, amount); err != nil {
				return err
			}
		}
	}
	return nil
}

// loadCatalogRateCards loads the arrears usage rate cards that apply to one
// payer, joined to their meters. A payer-scoped card (#798 negotiated
// pricing, customer_id set) REPLACES the merchant-default card for the same
// meter_key. event_type and value_property default to the meter key when the
// meter leaves them unset; aggregation defaults to sum. or#893 removed the
// #599 counter/gauge bridge — aggregation is the only meter shape.
func (s *MoneyService) loadCatalogRateCards(ctx context.Context, merchantID uuid.UUID, payer identity.CustomerID, currency string) ([]catalogRateCardRow, error) {
	var out []catalogRateCardRow
	err := s.db.RunInMerchantConn(ctx, func(ctx context.Context) error {
		rows, qerr := s.db.Qx(ctx).Query(ctx, `
SELECT rc.id,
       rc.meter_key,
       rc.customer_id IS NOT NULL AS payer_scoped,
       COALESCE(NULLIF(cm.event_type, ''), cm.key) AS event_type,
       COALESCE(NULLIF(cm.value_property, ''), cm.key) AS value_property,
       COALESCE(NULLIF(cm.aggregation, ''), 'sum') AS aggregation,
       COALESCE(cm.group_by, '{}'::jsonb) AS group_by,
       COALESCE(rc.filter, '{}'::jsonb) AS filter,
       rc.allowance,
       rc.price
FROM openrails.catalog_rate_cards rc
JOIN openrails.catalog_meters cm
  ON cm.merchant_id = rc.merchant_id AND cm.key = rc.meter_key
WHERE rc.merchant_id = $1
  AND rc.meter_key IS NOT NULL
  AND (rc.customer_id IS NULL OR rc.customer_id = $3)
  AND lower(COALESCE(rc.price ->> 'currency', $2)) = lower($2)
  AND rc.payment_term = 'in_arrears'
ORDER BY rc.ordinal`, merchantID, currency, payer.UUID())
		if qerr != nil {
			return qerr
		}
		defer rows.Close()
		for rows.Next() {
			var row catalogRateCardRow
			var groupByJSON, filterJSON, priceJSON []byte
			var allowanceJSON []byte
			if err := rows.Scan(&row.ID, &row.MeterKey, &row.PayerScoped, &row.EventType, &row.ValueKey, &row.Aggregation, &groupByJSON, &filterJSON, &allowanceJSON, &priceJSON); err != nil {
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
	if err != nil {
		return nil, err
	}
	return resolvePayerRateCardOverrides(out), nil
}

// resolvePayerRateCardOverrides collapses the loaded set to one card per
// meter_key: a payer-scoped card (#798) replaces the merchant default.
func resolvePayerRateCardOverrides(cards []catalogRateCardRow) []catalogRateCardRow {
	overridden := map[string]bool{}
	for _, rc := range cards {
		if rc.PayerScoped {
			overridden[rc.MeterKey] = true
		}
	}
	if len(overridden) == 0 {
		return cards
	}
	out := cards[:0]
	for _, rc := range cards {
		if !rc.PayerScoped && overridden[rc.MeterKey] {
			continue
		}
		out = append(out, rc)
	}
	return out
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
		rows, qerr := s.db.Qx(ctx).Query(ctx, `
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
		rows, qerr := s.db.Qx(ctx).Query(ctx, `
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

// meteredPeriodSourceID derives a deterministic idempotency source_id for a
// metered accrual over [from, to). Re-running the sweep for the same window
// yields the same source_id, so AccrueOwed (idempotent on (payer, source,
// source_id)) refuses to double-accrue.
func meteredPeriodSourceID(from, to time.Time) string {
	return fmt.Sprintf("period:%d-%d", from.UTC().Unix(), to.UTC().Unix())
}

// accrueMeteredPrefix accrues owed for a metered source over the period prefix
// [periodFrom, ratedThrough), where ratedPrefix is the cost of rating the WHOLE
// prefix once (allowances/rounding applied over the full aggregate). A durable
// watermark row per (payer, currency, source[+dim], period start) records what
// has already been accrued for the period; only the delta above it is accrued —
// in the SAME transaction as the ledger transfer — so overlapping closes over
// one period (threshold close mid-period, a later threshold close, the
// month-end finalize) bill each unit of usage exactly once (#672). The
// watermark is monotone: a stale or repeated close computes delta <= 0 and
// accrues nothing. Returns the newly accrued delta in micros.
func (s *MoneyService) accrueMeteredPrefix(ctx context.Context, payer identity.CustomerID, currency, source, dimValue string, periodFrom, ratedThrough time.Time, ratedPrefix int64) (int64, error) {
	if s == nil || s.db == nil {
		return 0, fmt.Errorf("money service not initialized")
	}
	cur := normalizeCurrency(currency)
	if err := RequireBillingCurrency(cur); err != nil {
		return 0, err
	}
	source = strings.TrimSpace(source)
	if source == "" {
		return 0, fmt.Errorf("source required")
	}
	if ratedPrefix <= 0 {
		return 0, nil
	}
	tid, err := merchant.Require(ctx)
	if err != nil {
		return 0, err
	}
	tenantID := tid.UUID()
	payerID := payer.UUID()
	now := s.now()
	wmSource := source
	sourceID := meteredPeriodSourceID(periodFrom, ratedThrough)
	if dimValue != "" {
		wmSource += ":dim:" + dimValue
		sourceID += ":dim:" + dimValue
	}

	var accrued int64
	// or#868 B2: merchant-pinned, matching AccrueOwed. The watermark INSERT
	// below is the exact statement that failed with "new row violates
	// row-level security policy for table metered_rating_watermarks" (42501)
	// off the request path — the bare RunInTx this replaces carried no
	// app.merchant_id, so nothing but a caller-supplied pin ever made it work.
	err = s.db.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		// Upsert-lock the watermark row: ON CONFLICT DO UPDATE takes the row lock
		// and returns the current committed values, serializing concurrent sweeps.
		var alreadyAccrued int64
		if err := tx.QueryRow(ctx, `
INSERT INTO openrails.metered_rating_watermarks (
    merchant_id, customer_id, currency, source, period_from,
    rated_through, accrued_amount, created_at, updated_at
) VALUES ($1, $2, $3, $4, $5, $5, 0, $6, $6)
ON CONFLICT (merchant_id, customer_id, currency, source, period_from)
DO UPDATE SET updated_at = openrails.metered_rating_watermarks.updated_at
RETURNING accrued_amount`,
			tenantID, payerID, cur, wmSource, periodFrom.UTC(), now).Scan(&alreadyAccrued); err != nil {
			return err
		}
		delta := ratedPrefix - alreadyAccrued
		if delta <= 0 {
			// Everything in this prefix is already billed; just advance rated_through.
			_, err := tx.Exec(ctx, `
UPDATE openrails.metered_rating_watermarks
SET rated_through = GREATEST(rated_through, $6), updated_at = $7
WHERE merchant_id = $1 AND customer_id = $2 AND currency = $3 AND source = $4 AND period_from = $5`,
				tenantID, payerID, cur, wmSource, periodFrom.UTC(), ratedThrough.UTC(), now)
			return err
		}
		q := gen.New(tx)
		if err := s.ensureSettingsRowTx(ctx, q, tenantID, payerID, cur, BillingModeArrears, now); err != nil {
			return err
		}
		ml := s.moneyLedger(q, tenantID)
		ratingKey, kerr := NewIdempotencyKey(OpMeteredRating, source, sourceID)
		if kerr != nil {
			return kerr
		}
		if _, err := ml.AccrueOwed(ctx, payerID, cur, delta, ratingKey.Coord(), nil); err != nil {
			return err
		}
		// #798: the accrual belongs to its RATING PERIOD, not the sweep time —
		// stamp invoice_at = periodFrom so a close over a past window (e.g. the
		// previous-month finalize) attaches it instead of leaking it into the
		// next period's invoice.
		if err := insertPendingInvoiceItemTx(ctx, q, tenantID, payerID, cur, txOwedAccrual, ratingKey.invoiceItemSourceID(), delta, periodFrom.UTC(), map[string]any{
			"operation": string(ratingKey.Operation()),
			"source":    ratingKey.Source(),
		}); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
UPDATE openrails.metered_rating_watermarks
SET rated_through = GREATEST(rated_through, $6), accrued_amount = accrued_amount + $7, updated_at = $8
WHERE merchant_id = $1 AND customer_id = $2 AND currency = $3 AND source = $4 AND period_from = $5`,
			tenantID, payerID, cur, wmSource, periodFrom.UTC(), ratedThrough.UTC(), delta, now); err != nil {
			return err
		}
		accrued = delta
		return nil
	})
	if err != nil {
		return 0, err
	}
	return accrued, nil
}
