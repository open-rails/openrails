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

type CatalogMeterSpec struct {
	Key           string            `json:"key"`
	EventType     string            `json:"event_type,omitempty"`
	ValueProperty string            `json:"value_property,omitempty"`
	Aggregation   string            `json:"aggregation,omitempty"`
	Unit          string            `json:"unit,omitempty"`
	GroupBy       map[string]string `json:"group_by,omitempty"`
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

type SyncCatalogSidecarsRequest struct {
	Meters    []CatalogMeterSpec    `json:"meters,omitempty"`
	RateCards []CatalogRateCardSpec `json:"rate_cards,omitempty"`
}

func (s *Service) SyncCatalogSidecars(ctx context.Context, req SyncCatalogSidecarsRequest) error {
	ctx, release, pinErr := s.pin(ctx)
	if pinErr != nil {
		return pinErr
	}
	defer release()

	dbi, err := s.requireDB()
	if err != nil {
		return err
	}
	tid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	return dbi.RunInTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if err := syncMeters(ctx, tx, tid.UUID(), req.Meters); err != nil {
			return err
		}
		if err := syncRateCards(ctx, tx, tid.UUID(), req.RateCards); err != nil {
			return err
		}
		return nil
	})
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

func resolveProductID(ctx context.Context, tx pgx.Tx, merchantID uuid.UUID, key string) (uuid.UUID, error) {
	var id uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT id FROM openrails.products WHERE merchant_id = $1 AND key = $2`, merchantID, strings.TrimSpace(key)).Scan(&id); err != nil {
		return uuid.Nil, fmt.Errorf("resolve product %q: %w", key, err)
	}
	return id, nil
}
