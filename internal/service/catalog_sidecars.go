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

	"github.com/open-rails/openrails/internal/db/gen"
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

// CatalogMutationOptions controls the private billing-definition merge.
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
			if _, err := gen.New(tx).LockMerchantSettings(ctx, tid.UUID()); err != nil {
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
	_, err := catalogMutation(ctx, s, func(ctx context.Context, scoped *Service) (struct{}, error) {
		return struct{}{}, scoped.syncCatalogSidecars(ctx, desired, opts)
	})
	return err
}

func (s *Service) syncCatalogSidecars(ctx context.Context, desired SyncCatalogSidecarsRequest, opts CatalogMutationOptions) error {
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
		q := gen.New(tx)
		merchantID, err := q.LockMerchantSettings(ctx, tid.UUID())
		if err != nil {
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
			if err := syncMeter(ctx, q, merchantID, meter, opts.Overwrite); err != nil {
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
			if err := syncRateCard(ctx, q, merchantID, card, opts.Overwrite); err != nil {
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
	q := gen.New(tx)
	meters, err := q.ListCatalogMeters(ctx, merchantID)
	if err != nil {
		return out, err
	}
	for _, m := range meters {
		meter := CatalogMeterSpec{Key: m.Key, EventType: m.EventType, ValueProperty: m.ValueProperty, Aggregation: m.Aggregation, Unit: m.Unit}
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
		card := CatalogRateCardSpec{id: c.ID, createdAt: c.CreatedAt, ProductKey: c.ProductKey, Ordinal: int(c.Ordinal), MeterKey: c.MeterKey, PaymentTerm: c.PaymentTerm, Allowance: c.Allowance, Price: c.Price}
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

func syncMeter(ctx context.Context, q *gen.Queries, merchantID uuid.UUID, meter CatalogMeterSpec, overwrite bool) error {
	groupBy, err := json.Marshal(meter.GroupBy)
	if err != nil {
		return err
	}
	return q.SyncCatalogMeter(ctx, gen.SyncCatalogMeterParams{
		MerchantID: merchantID, MeterKey: meter.Key, EventType: meter.EventType, ValueProperty: meter.ValueProperty,
		Aggregation: meter.Aggregation, Unit: meter.Unit, GroupBy: groupBy, Overwrite: overwrite,
	})
}

func syncRateCard(ctx context.Context, q *gen.Queries, merchantID uuid.UUID, spec CatalogRateCardSpec, overwrite bool) error {
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
	return q.SyncCatalogRateCard(ctx, gen.SyncCatalogRateCardParams{
		MerchantID: merchantID, ProductID: productID, Ordinal: int64(spec.Ordinal), MeterKey: spec.MeterKey,
		PaymentTerm: spec.PaymentTerm, Filter: filter, Allowance: spec.Allowance, Price: spec.Price,
		ID: id, CreatedAt: created, Overwrite: overwrite,
	})
}
