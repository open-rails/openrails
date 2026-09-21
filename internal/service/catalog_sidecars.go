package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/pkg/merchant"
	"github.com/open-rails/openrails/pkg/pricing"
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
	id          uuid.UUID
	createdAt   time.Time
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

// CatalogMutationOptions are the same three mutation classes as catalog publish.
type CatalogMutationOptions struct{ Insert, Overwrite, Prune bool }

func (s *Service) PlanCatalogBilling(ctx context.Context, desired SyncCatalogSidecarsRequest, opts CatalogMutationOptions) (bool, bool, error) {
	ctx, release, err := s.pin(ctx)
	if err != nil {
		return false, false, err
	}
	defer release()
	dbi, err := s.requireDB()
	if err != nil {
		return false, false, err
	}
	tid, err := merchant.Require(ctx)
	if err != nil {
		return false, false, err
	}
	if err := normalizeCatalogBilling(&desired); err != nil {
		return false, false, err
	}
	var current SyncCatalogSidecarsRequest
	err = dbi.RunInTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if opts.Insert || opts.Overwrite || opts.Prune {
			var locked uuid.UUID
			if err := tx.QueryRow(ctx, `SELECT id FROM billing.merchants WHERE id=$1 FOR UPDATE`, tid.UUID()).Scan(&locked); err != nil {
				return err
			}
		}
		var err error
		current, err = readCatalogBilling(ctx, tx, tid.UUID())
		if err != nil {
			return err
		}
		return checkCatalogBillingChanges(ctx, tx, tid.UUID(), current, mergeCatalogBilling(current, desired, opts))
	})
	if err != nil {
		return false, false, err
	}
	return !reflect.DeepEqual(current.Meters, desired.Meters), !sameCatalogCards(current.RateCards, desired.RateCards), nil
}

func (s *Service) SyncCatalogSidecars(ctx context.Context, desired SyncCatalogSidecarsRequest, opts CatalogMutationOptions) error {
	ctx, release, err := s.pin(ctx)
	if err != nil {
		return err
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
	if err := normalizeCatalogBilling(&desired); err != nil {
		return err
	}
	return dbi.RunInTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		// Serializes declaration merges. Re-read after acquiring the lock so partial
		// flags cannot restore state read by an earlier, now stale plan.
		var merchantID uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT id FROM billing.merchants WHERE id=$1 FOR UPDATE`, tid.UUID()).Scan(&merchantID); err != nil {
			return err
		}
		current, err := readCatalogBilling(ctx, tx, merchantID)
		if err != nil {
			return err
		}
		next := mergeCatalogBilling(current, desired, opts)
		if err := checkCatalogBillingChanges(ctx, tx, merchantID, current, next); err != nil {
			return err
		}
		// A product excluded by Insert=false was deliberately not created by the
		// product phase. Its declared cards cannot be inserted either.
		cards := next.RateCards[:0]
		for _, card := range next.RateCards {
			if card.ProductKey != "" {
				var exists bool
				if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM billing.products WHERE merchant_id=$1 AND key=$2)`, merchantID, card.ProductKey).Scan(&exists); err != nil {
					return err
				}
				if !exists {
					continue
				}
			}
			cards = append(cards, card)
		}
		next.RateCards = cards
		currentMeters := make(map[string]CatalogMeterSpec)
		nextMeters := make(map[string]bool)
		for _, meter := range current.Meters {
			currentMeters[meter.Key] = meter
		}
		for _, meter := range next.Meters {
			nextMeters[meter.Key] = true
			if stored, ok := currentMeters[meter.Key]; ok && reflect.DeepEqual(stored, meter) {
				continue
			}
			if err := syncMeter(ctx, tx, merchantID, meter, opts.Overwrite); err != nil {
				return err
			}
		}
		nextCards := make(map[string]CatalogRateCardSpec)
		currentCards := make(map[string]CatalogRateCardSpec)
		for _, card := range current.RateCards {
			currentCards[catalogCardKey(card)] = card
		}
		for _, card := range next.RateCards {
			nextCards[catalogCardKey(card)] = card
		}
		// Delete only changed/omitted cards. Kept rows (including their ids and
		// timestamps) are never restored from a stale snapshot. Deleting all
		// changed cards before insertion allows a declared meter to move slots.
		for _, card := range current.RateCards {
			replacement, keep := nextCards[catalogCardKey(card)]
			if keep && sameCatalogCards([]CatalogRateCardSpec{card}, []CatalogRateCardSpec{replacement}) {
				continue
			}
			if _, err := tx.Exec(ctx, `DELETE FROM billing.catalog_rate_cards WHERE merchant_id=$1 AND id=$2 AND customer_id IS NULL`, merchantID, card.id); err != nil {
				return err
			}
		}
		for _, meter := range current.Meters {
			if !nextMeters[meter.Key] {
				if _, err := tx.Exec(ctx, `DELETE FROM billing.catalog_meters WHERE merchant_id=$1 AND key=$2`, merchantID, meter.Key); err != nil {
					return err
				}
			}
		}
		for _, card := range next.RateCards {
			if stored, ok := currentCards[catalogCardKey(card)]; ok && sameCatalogCards([]CatalogRateCardSpec{stored}, []CatalogRateCardSpec{card}) {
				continue
			}
			if err := syncRateCard(ctx, tx, merchantID, card, opts.Overwrite); err != nil {
				return err
			}
		}
		return nil
	})
}

// Called before product/provider changes and again inside the write transaction.
// The first check rejects predictable failures; the second closes concurrent
// usage/override races without holding database locks during provider requests.
func checkCatalogBillingChanges(ctx context.Context, tx pgx.Tx, merchantID uuid.UUID, current, next SyncCatalogSidecarsRequest) error {
	if reflect.DeepEqual(current.Meters, next.Meters) && sameCatalogCards(current.RateCards, next.RateCards) {
		return nil
	}
	currentMeters := make(map[string]CatalogMeterSpec)
	nextMeters := make(map[string]CatalogMeterSpec)
	for _, m := range current.Meters {
		currentMeters[m.Key] = m
	}
	for _, m := range next.Meters {
		nextMeters[m.Key] = m
	}
	for _, m := range current.Meters {
		if _, keep := nextMeters[m.Key]; !keep {
			if err := money.CheckCatalogMeterChange(ctx, tx, merchantID, m.Key, nil); err != nil {
				return err
			}
		}
	}
	for _, m := range next.Meters {
		if old, ok := currentMeters[m.Key]; ok && reflect.DeepEqual(old, m) {
			continue
		}
		replacement := pricing.Meter{Key: m.Key, EventType: m.EventType, ValueProperty: m.ValueProperty, Aggregation: m.Aggregation, Unit: m.Unit, GroupBy: m.GroupBy}
		if err := money.CheckCatalogMeterChange(ctx, tx, merchantID, m.Key, &replacement); err != nil {
			return err
		}
	}
	currentCards := make(map[string]CatalogRateCardSpec)
	nextCards := make(map[string]CatalogRateCardSpec)
	for _, c := range current.RateCards {
		if c.MeterKey != "" {
			currentCards[c.MeterKey] = c
		}
	}
	for _, c := range next.RateCards {
		if c.MeterKey != "" {
			nextCards[c.MeterKey] = c
		}
	}
	for _, currentCard := range current.RateCards {
		key := currentCard.MeterKey
		if key == "" {
			continue
		}
		if _, keep := nextCards[key]; !keep {
			if err := money.CheckCatalogRateCardRemoval(ctx, tx, merchantID, key); err != nil {
				return err
			}
		}
	}
	for _, c := range next.RateCards {
		key := c.MeterKey
		if key == "" {
			continue
		}
		meter, exists := nextMeters[key]
		if !exists {
			return fmt.Errorf("rate card needs meter %q: %w", key, ErrMeterRateCardConflict)
		}
		if old, ok := currentCards[key]; ok && sameCatalogCards([]CatalogRateCardSpec{old}, []CatalogRateCardSpec{c}) && reflect.DeepEqual(currentMeters[key], meter) {
			continue
		}
		var price pricing.RatePrice
		if err := json.Unmarshal(c.Price, &price); err != nil {
			return err
		}
		effective := pricing.Meter{Key: meter.Key, EventType: meter.EventType, ValueProperty: meter.ValueProperty, Aggregation: meter.Aggregation, Unit: meter.Unit, GroupBy: meter.GroupBy}
		if err := money.CheckCatalogRateCardContracts(ctx, tx, merchantID, effective, c.Filter, price); err != nil {
			return err
		}
	}
	return nil
}

func readCatalogBilling(ctx context.Context, tx pgx.Tx, merchantID uuid.UUID) (SyncCatalogSidecarsRequest, error) {
	var out SyncCatalogSidecarsRequest
	rows, err := tx.Query(ctx, `SELECT key, COALESCE(event_type,''), COALESCE(value_property,''), COALESCE(aggregation,''), COALESCE(unit,''), group_by FROM billing.catalog_meters WHERE merchant_id=$1`, merchantID)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var meter CatalogMeterSpec
		if err := rows.Scan(&meter.Key, &meter.EventType, &meter.ValueProperty, &meter.Aggregation, &meter.Unit, &meter.GroupBy); err != nil {
			rows.Close()
			return out, err
		}
		out.Meters = append(out.Meters, meter)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return out, err
	}
	rows, err = tx.Query(ctx, `SELECT rc.id, rc.created_at, COALESCE(p.key,''), rc.ordinal, COALESCE(rc.meter_key,''), rc.payment_term, rc.filter, rc.allowance, rc.price FROM billing.catalog_rate_cards rc LEFT JOIN billing.products p ON p.merchant_id=rc.merchant_id AND p.id=rc.product_id WHERE rc.merchant_id=$1 AND rc.customer_id IS NULL`, merchantID)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var card CatalogRateCardSpec
		if err := rows.Scan(&card.id, &card.createdAt, &card.ProductKey, &card.Ordinal, &card.MeterKey, &card.PaymentTerm, &card.Filter, &card.Allowance, &card.Price); err != nil {
			return out, err
		}
		out.RateCards = append(out.RateCards, card)
	}
	if err := rows.Err(); err != nil {
		return out, err
	}
	err = normalizeCatalogBilling(&out)
	return out, err
}

// Storage ordinals are declared positions within a product. Collection/map
// iteration order, empty/default values and filter set ordering are not changes.
func normalizeCatalogBilling(state *SyncCatalogSidecarsRequest) error {
	for i := range state.Meters {
		m := &state.Meters[i]
		if m.EventType == "" {
			m.EventType = m.Key
		}
		if len(m.GroupBy) == 0 {
			m.GroupBy = nil
		}
	}
	slices.SortFunc(state.Meters, func(a, b CatalogMeterSpec) int { return strings.Compare(a.Key, b.Key) })
	for i := range state.RateCards {
		c := &state.RateCards[i]
		if c.PaymentTerm == "" {
			c.PaymentTerm = "in_arrears"
		}
		if len(c.Filter) == 0 {
			c.Filter = nil
		}
		for key, values := range c.Filter {
			slices.Sort(values)
			c.Filter[key] = slices.Compact(values)
		}
		var price pricing.RatePrice
		if err := json.Unmarshal(c.Price, &price); err != nil {
			return fmt.Errorf("decode catalog rate card: %w", err)
		}
		if err := pricing.ValidateRatePrice("catalog rate card", &price); err != nil {
			return err
		}
		if price.PerUnit != nil {
			if price.PerUnit.DivideBy == 0 {
				price.PerUnit.DivideBy = 1
			}
			if price.PerUnit.Round == "" {
				price.PerUnit.Round = pricing.RoundHalfUp
			}
		}
		c.Price, _ = json.Marshal(price)
		if len(c.Allowance) == 0 || bytes.Equal(c.Allowance, []byte("null")) {
			c.Allowance = nil
		} else {
			var allowance pricing.Allowance
			if err := json.Unmarshal(c.Allowance, &allowance); err != nil {
				return err
			}
			if err := pricing.ValidateAllowance("catalog allowance", &allowance); err != nil {
				return err
			}
			if allowance.Cap != "" {
				duration, err := pricing.ParseDurationSpec(allowance.Cap)
				if err != nil {
					return err
				}
				allowance.Cap = duration.String()
			}
			if allowance.Included == 0 && allowance.AccrueFrom == "" && allowance.Cap == "" {
				c.Allowance = nil
			} else {
				c.Allowance, _ = json.Marshal(allowance)
			}
		}
	}
	slices.SortFunc(state.RateCards, func(a, b CatalogRateCardSpec) int { return strings.Compare(catalogCardKey(a), catalogCardKey(b)) })
	if len(state.Meters) == 0 {
		state.Meters = nil
	}
	if len(state.RateCards) == 0 {
		state.RateCards = nil
	}
	return nil
}

func catalogCardKey(c CatalogRateCardSpec) string {
	if c.ProductKey == "" {
		return "meter:" + c.MeterKey
	}
	return fmt.Sprintf("product:%s:%d", c.ProductKey, c.Ordinal)
}

func mergeCatalogBilling(current, desired SyncCatalogSidecarsRequest, opts CatalogMutationOptions) SyncCatalogSidecarsRequest {
	meters := make(map[string]CatalogMeterSpec)
	cards := make(map[string]CatalogRateCardSpec)
	for _, m := range current.Meters {
		meters[m.Key] = m
	}
	for _, c := range current.RateCards {
		cards[catalogCardKey(c)] = c
	}
	wantedMeters := make(map[string]bool)
	wantedCards := make(map[string]bool)
	for _, m := range desired.Meters {
		wantedMeters[m.Key] = true
		_, exists := meters[m.Key]
		if (exists && opts.Overwrite) || (!exists && opts.Insert) {
			meters[m.Key] = m
		}
	}
	for _, c := range desired.RateCards {
		key := catalogCardKey(c)
		wantedCards[key] = true
		_, exists := cards[key]
		if (exists && opts.Overwrite) || (!exists && opts.Insert) {
			if previous, ok := cards[key]; ok {
				c.id = previous.id
				c.createdAt = previous.createdAt
			}
			cards[key] = c
		}
	}
	if opts.Prune {
		for key := range meters {
			if !wantedMeters[key] {
				delete(meters, key)
			}
		}
		for key := range cards {
			if !wantedCards[key] {
				delete(cards, key)
			}
		}
	}
	var out SyncCatalogSidecarsRequest
	for _, m := range meters {
		out.Meters = append(out.Meters, m)
	}
	for _, c := range cards {
		out.RateCards = append(out.RateCards, c)
	}
	slices.SortFunc(out.Meters, func(a, b CatalogMeterSpec) int { return strings.Compare(a.Key, b.Key) })
	slices.SortFunc(out.RateCards, func(a, b CatalogRateCardSpec) int { return strings.Compare(catalogCardKey(a), catalogCardKey(b)) })
	return out
}

// The metadata fields are deliberately excluded: the plan compares declared
// meaning, while an edited row retains its original identity and creation time.
func sameCatalogCards(a, b []CatalogRateCardSpec) bool {
	left, _ := json.Marshal(a)
	right, _ := json.Marshal(b)
	return bytes.Equal(left, right)
}

func syncMeter(ctx context.Context, tx pgx.Tx, merchantID uuid.UUID, meter CatalogMeterSpec, overwrite bool) error {
	groupBy, err := json.Marshal(meter.GroupBy)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `
INSERT INTO billing.catalog_meters (merchant_id,key,event_type,value_property,aggregation,unit,group_by)
VALUES ($1,$2,NULLIF($3,''),NULLIF($4,''),NULLIF($5,''),NULLIF($6,''),$7::jsonb)
ON CONFLICT (merchant_id,key) DO UPDATE SET event_type=EXCLUDED.event_type,
 value_property=EXCLUDED.value_property,aggregation=EXCLUDED.aggregation,
 unit=EXCLUDED.unit,group_by=EXCLUDED.group_by,updated_at=now() WHERE $8`,
		merchantID, meter.Key, meter.EventType, meter.ValueProperty, meter.Aggregation, meter.Unit, string(groupBy), overwrite)
	return err
}

func syncRateCard(ctx context.Context, tx pgx.Tx, merchantID uuid.UUID, spec CatalogRateCardSpec, overwrite bool) error {
	var productID *uuid.UUID
	if spec.ProductKey != "" {
		id, err := resolveProductID(ctx, tx, merchantID, spec.ProductKey)
		if err != nil {
			return err
		}
		productID = &id
	}
	filter, err := json.Marshal(spec.Filter)
	if err != nil {
		return err
	}
	var id *uuid.UUID
	var created *time.Time
	if spec.id != uuid.Nil {
		id = &spec.id
		created = &spec.createdAt
	}
	_, err = tx.Exec(ctx, `
INSERT INTO billing.catalog_rate_cards
 (merchant_id,product_id,ordinal,meter_key,payment_term,filter,allowance,price,id,created_at)
VALUES ($1,$2,$3,NULLIF($4,''),$5,$6::jsonb,NULLIF($7,'')::jsonb,$8::jsonb,COALESCE($9,gen_random_uuid()),COALESCE($10,now()))
ON CONFLICT (merchant_id,product_id,ordinal) DO UPDATE SET meter_key=EXCLUDED.meter_key,
 payment_term=EXCLUDED.payment_term,filter=EXCLUDED.filter,allowance=EXCLUDED.allowance,
 price=EXCLUDED.price,updated_at=now() WHERE $11`, merchantID, productID, spec.Ordinal, spec.MeterKey, spec.PaymentTerm, string(filter), string(spec.Allowance), string(spec.Price), id, created, overwrite)
	return err
}

func resolveProductID(ctx context.Context, tx pgx.Tx, merchantID uuid.UUID, key string) (uuid.UUID, error) {
	var id uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT id FROM billing.products WHERE merchant_id = $1 AND key = $2`, merchantID, strings.TrimSpace(key)).Scan(&id); err != nil {
		return uuid.Nil, fmt.Errorf("resolve product %q: %w", key, err)
	}
	return id, nil
}
