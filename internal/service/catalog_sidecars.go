package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/catalogrules"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/money"
)

type CatalogMeterSpec struct {
	Key           string              `json:"key"`
	EventType     string              `json:"event_type,omitempty"`
	ValueProperty string              `json:"value_property,omitempty"`
	Aggregation   catalog.Aggregation `json:"aggregation,omitempty"`
	Unit          string              `json:"unit,omitempty"`
	GroupBy       map[string]string   `json:"group_by,omitempty"`
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

func (s *Service) SyncCatalogSidecars(ctx context.Context, desired SyncCatalogSidecarsRequest) error {
	_, err := catalogCommand(ctx, s, "catalog/billing", desired, "", sidecarPlan(desired), func(ctx context.Context, s *Service) (struct{}, error) {
		return struct{}{}, s.syncCatalogSidecarsCommand(ctx, desired)
	})
	return err
}

func (s *Service) syncCatalogSidecarsCommand(ctx context.Context, desired SyncCatalogSidecarsRequest) error {
	_, err := catalogMutation(ctx, s, func(ctx context.Context, scoped *Service) (struct{}, error) {
		return struct{}{}, scoped.syncCatalogSidecars(ctx, desired)
	})
	return err
}

func (s *Service) syncCatalogSidecars(ctx context.Context, desired SyncCatalogSidecarsRequest) error {
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
		// Serializes declaration merges. Re-read after acquiring the lock.
		q := gen.New(tx)
		merchantID, err := q.LockMerchantSettings(ctx, tid.UUID())
		if err != nil {
			return err
		}
		current, err := readCatalogBilling(ctx, tx, merchantID)
		if err != nil {
			return err
		}
		next := mergeCatalogBilling(current, desired)
		if err := checkCatalogBillingChanges(ctx, tx, merchantID, current, next); err != nil {
			return err
		}
		cards := next.RateCards[:0]
		for _, card := range next.RateCards {
			if card.ProductKey != "" {
				_, err := q.GetProductByKey(ctx, gen.GetProductByKeyParams{MerchantID: merchantID, Key: card.ProductKey})
				if errors.Is(err, pgx.ErrNoRows) {
					continue
				}
				if err != nil {
					return err
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
			groupBy, err := json.Marshal(meter.GroupBy)
			if err != nil {
				return err
			}
			if err := q.UpsertUsageMeter(ctx, gen.UpsertUsageMeterParams{
				MerchantID: merchantID, MeterKey: meter.Key, EventType: meter.EventType, ValueProperty: meter.ValueProperty,
				Aggregation: string(meter.Aggregation), Unit: meter.Unit, GroupBy: groupBy,
			}); err != nil {
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
			if err := q.DeleteDefaultCatalogRateCard(ctx, gen.DeleteDefaultCatalogRateCardParams{MerchantID: merchantID, ID: card.id}); err != nil {
				return err
			}
		}
		for _, meter := range current.Meters {
			if !nextMeters[meter.Key] {
				if err := q.DeleteCatalogMeter(ctx, gen.DeleteCatalogMeterParams{MerchantID: merchantID, MeterKey: meter.Key}); err != nil {
					return err
				}
			}
		}
		for _, card := range next.RateCards {
			if stored, ok := currentCards[catalogCardKey(card)]; ok && sameCatalogCards([]CatalogRateCardSpec{stored}, []CatalogRateCardSpec{card}) {
				continue
			}
			if err := syncRateCard(ctx, q, merchantID, card); err != nil {
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
	var removed []string
	for _, m := range current.Meters {
		if _, keep := nextMeters[m.Key]; !keep {
			if err := money.CheckCatalogMeterChange(ctx, tx, merchantID, m.Key, nil); err != nil {
				return err
			}
			removed = append(removed, m.Key)
		}
	}
	if len(removed) > 0 {
		counted, err := gen.New(tx).ListCatalogLimitsUsingMeters(ctx, gen.ListCatalogLimitsUsingMetersParams{MerchantID: merchantID, MeterKeys: removed})
		if err != nil {
			return err
		}
		if len(counted) > 0 {
			return fmt.Errorf("limit %q counts a removed meter: %w", counted[0], ErrMeterInUse)
		}
	}
	for _, m := range next.Meters {
		if old, ok := currentMeters[m.Key]; ok && reflect.DeepEqual(old, m) {
			continue
		}
		replacement := catalogrules.Meter{Key: m.Key, EventType: m.EventType, ValueProperty: m.ValueProperty, Aggregation: m.Aggregation, Unit: m.Unit, GroupBy: m.GroupBy}
		if err := money.CheckCatalogMeterLimitCounts(ctx, gen.New(tx), merchantID, replacement); err != nil {
			return err
		}
		if err := money.CheckCatalogMeterChange(ctx, tx, merchantID, m.Key, &replacement); err != nil {
			return err
		}
	}
	currentCards := make(map[string]CatalogRateCardSpec)
	for _, c := range current.RateCards {
		if c.MeterKey != "" {
			currentCards[c.MeterKey] = c
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
		var price catalog.RatePrice
		if err := json.Unmarshal(c.Price, &price); err != nil {
			return err
		}
		effective := catalogrules.Meter{Key: meter.Key, EventType: meter.EventType, ValueProperty: meter.ValueProperty, Aggregation: meter.Aggregation, Unit: meter.Unit, GroupBy: meter.GroupBy}
		if err := money.CheckCatalogRateCardContracts(effective, c.Filter, price); err != nil {
			return err
		}
	}
	return nil
}

func readCatalogBilling(ctx context.Context, tx pgx.Tx, merchantID uuid.UUID) (SyncCatalogSidecarsRequest, error) {
	return readCatalogBillingQueries(ctx, gen.New(tx), merchantID)
}

func readCatalogBillingQueries(ctx context.Context, q *gen.Queries, merchantID uuid.UUID) (SyncCatalogSidecarsRequest, error) {
	var out SyncCatalogSidecarsRequest
	meters, err := q.ListCatalogMeters(ctx, merchantID)
	if err != nil {
		return out, err
	}
	for _, m := range meters {
		meter := CatalogMeterSpec{Key: m.Key, EventType: m.EventType, ValueProperty: m.ValueProperty, Aggregation: catalog.Aggregation(m.Aggregation), Unit: m.Unit}
		if err := json.Unmarshal(m.GroupBy, &meter.GroupBy); err != nil {
			return out, err
		}
		out.Meters = append(out.Meters, meter)
	}
	cards, err := q.ListDefaultCatalogRateCards(ctx, merchantID)
	if err != nil {
		return out, err
	}
	for _, c := range cards {
		card := CatalogRateCardSpec{id: c.ID, createdAt: c.CreatedAt, ProductKey: c.ProductKey, Ordinal: int(c.Ordinal), MeterKey: c.MeterKey, PaymentTerm: derefString(c.PaymentTerm), Allowance: c.Allowance, Price: c.Price}
		if err := json.Unmarshal(c.Filter, &card.Filter); err != nil {
			return out, err
		}
		out.RateCards = append(out.RateCards, card)
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
		// Only a flat fee states when it is collected.
		if c.PaymentTerm == "" && c.MeterKey == "" {
			c.PaymentTerm = "in_arrears"
		}
		if len(c.Filter) == 0 {
			c.Filter = nil
		}
		for key, values := range c.Filter {
			slices.Sort(values)
			c.Filter[key] = slices.Compact(values)
		}
		var price catalog.RatePrice
		if err := json.Unmarshal(c.Price, &price); err != nil {
			return fmt.Errorf("decode catalog rate card: %w", err)
		}
		if err := catalogrules.ValidateRatePrice("catalog rate card", &price); err != nil {
			return err
		}
		if price.PerUnit != nil {
			if price.PerUnit.DivideBy == 0 {
				price.PerUnit.DivideBy = 1
			}
			if price.PerUnit.Round == "" {
				price.PerUnit.Round = catalog.RoundHalfUp
			}
		}
		c.Price, _ = json.Marshal(price)
		if len(c.Allowance) == 0 || bytes.Equal(c.Allowance, []byte("null")) {
			c.Allowance = nil
		} else {
			var allowance catalog.Allowance
			if err := json.Unmarshal(c.Allowance, &allowance); err != nil {
				return err
			}
			if err := catalogrules.ValidateAllowance("catalog allowance", &allowance); err != nil {
				return err
			}
			if allowance.Included == 0 {
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

func mergeCatalogBilling(current, desired SyncCatalogSidecarsRequest) SyncCatalogSidecarsRequest {
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
		meters[m.Key] = m
	}
	for _, c := range desired.RateCards {
		key := catalogCardKey(c)
		wantedCards[key] = true
		if previous, ok := cards[key]; ok {
			c.id = previous.id
			c.createdAt = previous.createdAt
		}
		cards[key] = c
	}
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

func syncRateCard(ctx context.Context, q *gen.Queries, merchantID uuid.UUID, spec CatalogRateCardSpec) error {
	var productID *uuid.UUID
	if spec.ProductKey != "" {
		product, err := q.GetProductByKey(ctx, gen.GetProductByKeyParams{MerchantID: merchantID, Key: strings.TrimSpace(spec.ProductKey)})
		if err != nil {
			return fmt.Errorf("resolve product %q: %w", spec.ProductKey, err)
		}
		productID = &product.ID
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
	return q.UpsertUsageRateCard(ctx, gen.UpsertUsageRateCardParams{
		MerchantID: merchantID, ProductID: productID, Ordinal: int64(spec.Ordinal), MeterKey: spec.MeterKey,
		PaymentTerm: spec.PaymentTerm, Filter: filter, Allowance: spec.Allowance, Price: spec.Price,
		ID: id, CreatedAt: created,
	})
}

func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
