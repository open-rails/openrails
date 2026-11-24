package openrails

import (
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"strings"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/modules/limits"
	"github.com/open-rails/openrails/internal/shared/apperr"
)

type boundKey struct{ meter, cell string }

type moneyCard struct {
	meter limits.Meter
	card  catalog.RateCard
}

// pricing is how a gate costs requests of one event type in one currency:
// named-unit limits by their counts, the same code the server runs, and
// money by the cards in force at their price bounds, which never price below
// the server.
type pricing struct {
	currency string
	units    map[string]limits.Definition
	meters   map[string]limits.Meter
	money    []moneyCard
	bounds   map[boundKey]billing.PriceBound
}

func newPricing(v billing.ViewPricing) *pricing {
	p := &pricing{currency: normalizeCurrency(string(v.Currency)), units: map[string]limits.Definition{}, meters: map[string]limits.Meter{},
		bounds: map[boundKey]billing.PriceBound{}}
	meter := func(key, eventType, valueProperty string, aggregation catalog.Aggregation, groupBy map[string]string) limits.Meter {
		m := limits.Meter{Key: key, EventType: eventType, ValueProperty: limits.PropertyKey(valueProperty), Aggregation: aggregation, GroupBy: groupBy}
		if m.EventType == "" {
			m.EventType = key
		}
		if m.ValueProperty == "" {
			m.ValueProperty = key
		}
		if m.Aggregation == "" {
			m.Aggregation = catalog.AggregationSum
		}
		p.meters[key] = m
		return m
	}
	for _, c := range v.Counts {
		meter(c.Meter, c.EventType, c.ValueProperty, c.Aggregation, c.GroupBy)
		def := p.units[c.Limit]
		def.Key = c.Limit
		def.Counts = append(def.Counts, catalog.LimitCount{Meter: c.Meter, Weight: c.Weight, Per: c.Per, Filter: c.Filter})
		p.units[c.Limit] = def
	}
	for _, rc := range v.RateCards {
		m := meter(rc.Meter, rc.EventType, rc.ValueProperty, rc.Aggregation, rc.GroupBy)
		if c := normalizeCurrency(string(rc.Price.Currency)); c == "" || c == p.currency {
			p.money = append(p.money, moneyCard{meter: m, card: catalog.RateCard{Meter: rc.Meter, Filter: rc.Filter, Price: rc.Price}})
		}
	}
	for _, b := range v.PriceBounds {
		cell := ""
		if b.Cell != nil {
			cell = *b.Cell
		}
		p.bounds[boundKey{b.Meter, cell}] = b
	}
	return p
}

// request is what a decision prices: an estimate's usage, or a host-stated
// amount with none.
type request struct {
	usage  *limits.Usage
	stated *int64
	amount int64
}

func admitRequest(p billing.AdmitParams) request {
	if p.Usage == nil {
		return request{amount: p.EstimatedAmount}
	}
	return request{usage: &limits.Usage{EventType: strings.TrimSpace(p.Usage.EventType), Dimensions: p.Usage.Dimensions, Metadata: p.Usage.Metadata}, stated: p.Usage.Amount}
}

func usageRequest(u billing.LeaseUsage) request {
	return request{usage: &limits.Usage{EventType: u.EventType, Dimensions: u.Dimensions, Metadata: u.Metadata}, stated: u.Amount}
}

// errUnpriced: the gate cannot price the request; the server can.
var errUnpriced = errors.New("openrails: gate: the view cannot price this request")

// costs is what r takes from each limit of a view, by key: a money limit the
// request's money cost, a named-unit limit the units its counts make of r's
// usage (none for a host-stated request).
func (p *pricing) costs(lines []*lineState, r request) ([]int64, error) {
	out := make([]int64, len(lines))
	var money int64
	priced := false
	for i, l := range lines {
		if !l.money {
			if r.usage == nil || p == nil {
				continue
			}
			def, ok := p.units[l.line.Limit]
			if !ok {
				continue
			}
			units, err := limits.Units(def, p.meters, *r.usage)
			if err != nil {
				return nil, statusOf(err)
			}
			out[i] = units
			continue
		}
		if !priced {
			var err error
			if money, err = p.moneyCost(r); err != nil {
				return nil, err
			}
			priced = true
		}
		out[i] = money
	}
	return out, nil
}

func (p *pricing) moneyCost(r request) (int64, error) {
	if r.usage == nil {
		return r.amount, nil
	}
	if p == nil {
		return 0, errUnpriced
	}
	var total int64
	for _, mc := range p.money {
		q, cell, counted, err := limits.Measure(mc.meter, mc.card, *r.usage)
		if err != nil {
			return 0, statusOf(err)
		}
		if !counted {
			continue
		}
		var c int64
		if mc.card.Price.Model == catalog.ModelDynamic {
			if r.stated != nil {
				c = *r.stated
			}
		} else {
			b, ok := p.bounds[boundKey{mc.card.Meter, cell}]
			if !ok || b.DivideBy <= 0 {
				return 0, errUnpriced
			}
			if c, err = ceilMulDiv(q, b.UnitAmount, b.DivideBy); err != nil {
				return 0, err
			}
		}
		if total > maxGateAmount-c {
			return 0, fmt.Errorf("openrails: gate: money cost overflows int64")
		}
		total += c
	}
	return total, nil
}

const maxGateAmount = int64(^uint64(0) >> 1)

func satAdd(a, b int64) int64 {
	switch {
	case b > 0 && a > maxGateAmount-b:
		return maxGateAmount
	case b < 0 && a < -maxGateAmount-1-b:
		return -maxGateAmount - 1
	}
	return a + b
}

func ceilMulDiv(q, unit, divide int64) (int64, error) {
	n := new(big.Int).Mul(big.NewInt(q), big.NewInt(unit))
	d := big.NewInt(divide)
	quo, rem := new(big.Int).QuoRem(n, d, new(big.Int))
	if rem.Sign() > 0 {
		quo.Add(quo, big.NewInt(1))
	}
	if !quo.IsInt64() {
		return 0, fmt.Errorf("openrails: gate: money cost overflows int64")
	}
	return quo.Int64(), nil
}

// statusOf is an engine refusal as the Client reports it.
func statusOf(err error) error {
	var refusal *apperr.Error
	if !errors.As(err, &refusal) {
		return err
	}
	kind := billing.ErrorTypeInvalidRequest
	if refusal.Status >= 500 {
		kind = billing.ErrorTypeAPI
	}
	out := &billing.StatusError{Status: refusal.Status, ErrorDetails: billing.ErrorDetails{Type: kind, Code: refusal.Code, Message: refusal.Message, Metadata: refusal.Metadata}}
	if refusal.Param != "" {
		out.Param = &refusal.Param
	}
	return out
}

func invalidParam(param, msg string) error {
	return &billing.StatusError{Status: http.StatusBadRequest, ErrorDetails: billing.ErrorDetails{Type: billing.ErrorTypeInvalidRequest, Code: billing.CodeInvalidParam, Message: msg, Param: &param}}
}
