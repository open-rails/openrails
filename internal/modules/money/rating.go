package money

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	identity "github.com/open-rails/openrails/internal/billingidentity"
	"github.com/open-rails/openrails/internal/catalogrules"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/merchantconfig"
	"github.com/open-rails/openrails/internal/retention"
	"github.com/open-rails/openrails/internal/shared/apperr"
)

// Usage is priced when it is recorded. Each meter of the event's type that
// has a rate card takes its quantity from the event; the customer's quantity
// of that meter (and matrix cell) in the event's billing period is Q, and the
// event is charged R(Q+q) - R(Q), where R is the card's price for a whole
// period. R never falls as Q grows, so no charge is negative, and a period's
// charges sum to R of its total: per-event rounding never drifts.

func usageNotPriced(param, format string, args ...any) error {
	return &apperr.Error{Status: http.StatusUnprocessableEntity, Code: billing.CodeUsageNotPriced, Message: fmt.Sprintf(format, args...), Param: param}
}

// ratingCard is a meter and the rate card that prices it.
type ratingCard struct {
	MeterKey    string
	ValueKey    string
	Aggregation catalog.Aggregation
	GroupBy     map[string]string
	Filter      map[string][]string
	Allowance   *catalog.Allowance
	Price       catalog.RatePrice
}

// usageQuantity is what one meter takes from one event.
type usageQuantity struct {
	card     ratingCard
	cell     string
	quantity int64
}

// eventFields are the parts of an event a meter reads.
type eventFields struct {
	Dimensions map[string]int64
	Metadata   map[string]any
}

// usageCards reads the rate cards that price eventType in currency. A card in
// another currency does not price the event; an event type whose cards are
// all in other currencies is not priced at all.
func usageCards(ctx context.Context, q *gen.Queries, merchantID uuid.UUID, eventType, currency string) ([]ratingCard, error) {
	rows, err := q.ListUsageRatingCards(ctx, gen.ListUsageRatingCardsParams{MerchantID: merchantID, EventType: eventType})
	if err != nil {
		return nil, fmt.Errorf("load rate cards: %w", err)
	}
	cards := make([]ratingCard, 0, len(rows))
	for _, r := range rows {
		card := ratingCard{MeterKey: r.MeterKey, ValueKey: propertyKey(r.ValueProperty), Aggregation: catalog.Aggregation(r.Aggregation)}
		if err := json.Unmarshal(r.GroupBy, &card.GroupBy); err != nil {
			return nil, fmt.Errorf("decode meter %q group_by: %w", r.MeterKey, err)
		}
		if err := json.Unmarshal(r.Filter, &card.Filter); err != nil {
			return nil, fmt.Errorf("decode meter %q filter: %w", r.MeterKey, err)
		}
		if err := decodeRateCard(r.Price, r.Allowance, &card.Price, &card.Allowance); err != nil {
			return nil, fmt.Errorf("meter %q rate card: %w", r.MeterKey, err)
		}
		cards = append(cards, card)
	}
	priced := cards[:0:0]
	for _, card := range cards {
		if c := strings.ToUpper(strings.TrimSpace(card.Price.Currency)); c == "" || c == currency {
			priced = append(priced, card)
		}
	}
	if len(cards) > 0 && len(priced) == 0 {
		return nil, usageNotPriced("currency", "no rate card prices %s usage of event type %q", currency, eventType)
	}
	return priced, nil
}

// usageQuantities is what each card takes from an event. A card whose filter
// excludes the event, or which takes nothing from it, is left out; a matrix
// with no cell for the event's value refuses it.
func usageQuantities(cards []ratingCard, ev eventFields) ([]usageQuantity, error) {
	out := make([]usageQuantity, 0, len(cards))
	for _, card := range cards {
		if !card.admits(ev) {
			continue
		}
		quantity, err := card.quantity(ev)
		if err != nil {
			return nil, err
		}
		if quantity == 0 {
			continue
		}
		cell := ""
		if m := card.matrix(); m != nil {
			property := card.property(m.Dimension)
			value, param := eventValue(ev, property)
			if _, ok := m.Cells[value]; !ok {
				return nil, usageNotPriced(param, "meter %q has no price for %s %q", card.MeterKey, m.Dimension, value)
			}
			cell = value
		}
		out = append(out, usageQuantity{card: card, cell: cell, quantity: quantity})
	}
	return out, nil
}

func (c ratingCard) matrix() *catalog.Matrix {
	if c.Price.PerUnit == nil {
		return nil
	}
	return c.Price.PerUnit.Matrix
}

// property is the event field a dimension of the meter reads.
func (c ratingCard) property(dimension string) string {
	if p := propertyKey(c.GroupBy[dimension]); p != "" {
		return p
	}
	return dimension
}

func (c ratingCard) admits(ev eventFields) bool {
	for dimension, allowed := range c.Filter {
		value, _ := eventValue(ev, c.property(dimension))
		ok := false
		for _, a := range allowed {
			if a == value {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	return true
}

// quantity is 1 for a count meter and the value property for a sum meter,
// read from the dimensions, then the metadata.
func (c ratingCard) quantity(ev eventFields) (int64, error) {
	switch c.Aggregation {
	case catalog.AggregationCount:
		return 1, nil
	case catalog.AggregationSum:
	default:
		return 0, fmt.Errorf("meter %q: aggregation %q cannot be priced", c.MeterKey, c.Aggregation)
	}
	key := c.ValueKey
	if key == "" {
		key = c.MeterKey
	}
	if v, ok := ev.Dimensions[key]; ok {
		if v < 0 {
			return 0, apperr.Invalidf("dimensions.%s must not be negative", key).WithParam("dimensions." + key)
		}
		return v, nil
	}
	raw, ok := ev.Metadata[key]
	if !ok || raw == nil {
		return 0, nil
	}
	v, ok := integerOf(raw)
	if !ok || v < 0 {
		return 0, apperr.Invalidf("metadata.%s must be a nonnegative integer", key).WithParam("metadata." + key)
	}
	return v, nil
}

func integerOf(raw any) (int64, bool) {
	switch v := raw.(type) {
	case json.Number:
		n, err := v.Int64()
		return n, err == nil
	case string:
		n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		return n, err == nil
	}
	// A decoded JSON number: its text, as the request spelled it.
	text, err := json.Marshal(raw)
	if err != nil {
		return 0, false
	}
	n, err := strconv.ParseInt(string(text), 10, 64)
	return n, err == nil
}

// eventValue is the text of an event field, from the metadata, then the
// dimensions, and the request parameter it came from.
func eventValue(ev eventFields, property string) (string, string) {
	if raw, ok := ev.Metadata[property]; ok && raw != nil {
		var s string
		switch v := raw.(type) {
		case string:
			s = v
		case json.Number:
			s = v.String()
		default:
			b, _ := json.Marshal(v)
			s = string(b)
		}
		if s != "" {
			return s, "metadata." + property
		}
	}
	if v, ok := ev.Dimensions[property]; ok {
		return strconv.FormatInt(v, 10), "dimensions." + property
	}
	return "", "metadata." + property
}

// errVolumeTiers refuses a volume-tiered card on a meter: its price for
// earlier units falls when a later event reaches a cheaper band, so an event
// could owe a negative amount.
var errVolumeTiers = errors.New("a metered rate card cannot use volume tiers")

// periodPrice is R(quantity): what a card charges one cell for a whole
// period's quantity. An allowance's included units are in the price's units
// (divide_by units for per_unit) and are taken before the price applies.
func periodPrice(card ratingCard, cell string, quantity int64) (int64, error) {
	cm := catalogrules.Of(card.Price)
	if card.matrix() != nil {
		var ok bool
		if cm, ok = catalogrules.ForCell(card.Price, cell); !ok {
			return 0, fmt.Errorf("meter %q has no matrix cell %q", card.MeterKey, cell)
		}
	}
	if cm.Kind == catalog.ModelTiered && cm.Mode == catalog.TierModeVolume {
		return 0, fmt.Errorf("meter %q: %w", card.MeterKey, errVolumeTiers)
	}
	if card.Allowance != nil && card.Allowance.Included > 0 {
		included := big.NewInt(card.Allowance.Included)
		if cm.Kind == catalog.ModelPerUnit && cm.DivideBy > 1 {
			included.Mul(included, big.NewInt(cm.DivideBy))
		}
		billable := new(big.Int).Sub(big.NewInt(quantity), included)
		if billable.Sign() <= 0 {
			return 0, nil
		}
		quantity = billable.Int64()
	}
	return cm.Rate(quantity)
}

// marginalPrice is R(before + quantity) - R(before).
func marginalPrice(card ratingCard, cell string, before, quantity int64) (int64, error) {
	if before < 0 || quantity < 0 || before > maxQuantity-quantity {
		return 0, fmt.Errorf("meter %q: period quantity out of range", card.MeterKey)
	}
	prior, err := periodPrice(card, cell, before)
	if err != nil {
		return 0, err
	}
	after, err := periodPrice(card, cell, before+quantity)
	if err != nil {
		return 0, err
	}
	if after < prior {
		return 0, fmt.Errorf("meter %q: price fell from %d to %d", card.MeterKey, prior, after)
	}
	return after - prior, nil
}

const maxQuantity = int64(^uint64(0) >> 1)

// usagePeriodStart is the billing period an event dated occurred belongs
// to: the one usage orders close, under the merchant's boundary and anchored
// on the customer's first activity.
func (s *MoneyService) usagePeriodStart(ctx context.Context, q *gen.Queries, merchantID, customerID uuid.UUID, currency string, occurred time.Time) (time.Time, error) {
	settings, err := s.UsageSettings(ctx)
	if err != nil && !errors.Is(err, merchantconfig.ErrNoSettings) {
		return time.Time{}, err
	}
	boundary := merchantconfig.NormalizeBillingBoundary(settings.BillingPeriodBoundary)
	var anchor time.Time
	if boundary == BillingBoundaryAnniversary {
		if anchor, err = q.GetCustomerPeriodAnchor(ctx, gen.GetCustomerPeriodAnchorParams{MerchantID: merchantID, CustomerID: customerID, Currency: currency}); err != nil {
			return time.Time{}, fmt.Errorf("customer period anchor: %w", err)
		}
	}
	return CurrentBillingPeriodStart(occurred, anchor, boundary)
}

// rateUsageTx prices an event's quantities, advances the customer's period
// quantities and posts each charge, balance first and the rest owed: usage is
// never refused for money. It answers the charges and the transfer of the
// first one posted. The caller holds the customer's spend lock and has opened
// its balance account.
func (s *MoneyService) rateUsageTx(ctx context.Context, q *gen.Queries, payer identity.CustomerID, invoker, currency string, eventID uuid.UUID, occurred time.Time, quantities []usageQuantity) ([]models.UsageCharge, *uuid.UUID, error) {
	if len(quantities) == 0 {
		return []models.UsageCharge{}, nil, nil
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, nil, err
	}
	merchantID := mid.UUID()
	period, err := s.usagePeriodStart(ctx, q, merchantID, payer.UUID(), currency, occurred)
	if err != nil {
		return nil, nil, err
	}
	now := s.now()
	charges := make([]models.UsageCharge, 0, len(quantities))
	var first *uuid.UUID
	for _, u := range quantities {
		total, err := q.AddMeterPeriodQuantity(ctx, gen.AddMeterPeriodQuantityParams{
			MerchantID: merchantID, CustomerID: payer.UUID(), Currency: currency, MeterKey: u.card.MeterKey,
			Cell: u.cell, PeriodStartsAt: period, Quantity: u.quantity, Now: now,
		})
		if err != nil {
			return nil, nil, fmt.Errorf("meter %q period quantity: %w", u.card.MeterKey, err)
		}
		amount, err := marginalPrice(u.card, u.cell, total-u.quantity, u.quantity)
		if err != nil {
			return nil, nil, err
		}
		charges = append(charges, models.UsageCharge{MeterKey: u.card.MeterKey, Cell: u.cell, Quantity: u.quantity, Amount: amount})
		if amount == 0 {
			continue
		}
		key := MustIdempotencyKey(OpMeteredRating, "metered:"+u.card.MeterKey, eventID.String())
		if _, _, _, err := s.spendBalanceThenOwedTx(ctx, q, payer, invoker, currency, key, amount, true); err != nil {
			return nil, nil, err
		}
		if first == nil {
			tr, err := q.GetLedgerSpendByCoords(ctx, gen.GetLedgerSpendByCoordsParams{
				MerchantID: merchantID, CustomerID: payer.UUID(), Currency: currency,
				Operation: string(key.Operation()), Source: key.Source(), SourceID: key.SourceID(),
			})
			if err != nil {
				return nil, nil, fmt.Errorf("meter %q posting: %w", u.card.MeterKey, err)
			}
			first = &tr.ID
		}
	}
	if _, err := q.PruneMeterPeriodQuantities(ctx, gen.PruneMeterPeriodQuantitiesParams{MerchantID: merchantID, Before: now.Add(-retention.MeterPeriodQuantities)}); err != nil {
		return nil, nil, err
	}
	return charges, first, nil
}

func propertyKey(property string) string {
	property = strings.TrimSpace(property)
	property = strings.TrimPrefix(property, "$.")
	property = strings.TrimPrefix(property, "dimensions.")
	property = strings.TrimPrefix(property, "metadata.")
	return property
}
