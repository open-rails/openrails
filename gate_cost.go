package openrails

import (
	"errors"
	"fmt"
	"maps"
	"math/big"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/modules/limits"
	"github.com/open-rails/openrails/internal/shared/apperr"
)

// holdPart is one limit of a hold in one window. Its id names it across
// syncs: a capture settles in the window its admission took from.
type holdPart struct {
	id      string
	limit   string
	unit    string
	overrun catalog.Overrun
	start   *time.Time
	end     *time.Time
}

func partID(limit string, start *time.Time) string {
	if start == nil {
		return limit + "@"
	}
	return limit + "@" + strconv.FormatInt(start.Unix(), 10)
}

// partStart is the window start a part id names; nil without a window.
func partStart(id string) *time.Time {
	i := strings.LastIndexByte(id, '@')
	if i < 0 || i+1 == len(id) {
		return nil
	}
	sec, err := strconv.ParseInt(id[i+1:], 10, 64)
	if err != nil {
		return nil
	}
	start := time.Unix(sec, 0).UTC()
	return &start
}

func partLimit(id string) string {
	if i := strings.LastIndexByte(id, '@'); i >= 0 {
		return id[:i]
	}
	return id
}

// current reports whether the part's window contains now.
func (p holdPart) current(now time.Time) bool {
	return (p.start == nil || !now.Before(*p.start)) && (p.end == nil || now.Before(*p.end))
}

// holdSet is what a gate keeps of the server's holds of one customer and
// limit set. The server may hold one key in several holds (a fixed window's
// next-window hold beside the current one): each is kept by its windows, and
// the same limit in the same window adds up across them.
type holdSet struct {
	Latest  billing.Hold            `json:"latest"`
	Subs    map[string]billing.Hold `json:"subs"`
	Version int64                   `json:"version"`
}

// holdWindows names a hold among its key's others: its windowed parts.
func holdWindows(h billing.Hold) string {
	ids := []string{}
	for _, l := range h.Limits {
		if l.WindowEndsAt != nil {
			ids = append(ids, partID(l.Limit, l.WindowStartsAt))
		}
	}
	slices.Sort(ids)
	return strings.Join(ids, ",")
}

// apply takes one answer's holds of the key: each replaces the hold of its
// windows, and holds of one answer with the same windows add up. A hold with
// a window that ended by now is dropped; the server releases or reissues it.
func (s *holdSet) apply(holds []billing.Hold, now time.Time) {
	if s.Subs == nil {
		s.Subs = map[string]billing.Hold{}
	}
	fresh := map[string]billing.Hold{}
	for _, h := range holds {
		w := holdWindows(h)
		if prev, ok := fresh[w]; ok {
			h = addHolds(prev, h)
		}
		fresh[w] = h
		s.Latest = h
		s.Version = max(s.Version, h.Version)
	}
	maps.Copy(s.Subs, fresh)
	s.expire(now)
}

func (s *holdSet) expire(now time.Time) {
	for w, h := range s.Subs {
		for _, l := range h.Limits {
			if l.WindowEndsAt != nil && !now.Before(*l.WindowEndsAt) {
				delete(s.Subs, w)
				break
			}
		}
	}
}

// addHolds is two holds of one key and windows as one: b's cards, each
// part's held and the safe amounts summed.
func addHolds(a, b billing.Hold) billing.Hold {
	out := b
	out.Limits = slices.Clone(a.Limits)
	for _, l := range b.Limits {
		i := slices.IndexFunc(out.Limits, func(x billing.HoldLimit) bool {
			return partID(x.Limit, x.WindowStartsAt) == partID(l.Limit, l.WindowStartsAt)
		})
		if i < 0 {
			out.Limits = append(out.Limits, l)
			continue
		}
		out.Limits[i].Held = satAdd(out.Limits[i].Held, l.Held)
	}
	out.Safe = addSafe(a.Safe, b.Safe)
	return out
}

func addSafe(a, b *int64) *int64 {
	if a == nil && b == nil {
		return nil
	}
	v := satAdd(derefInt(a), derefInt(b))
	return &v
}

// merged is the key's holds as one: each limit in each window once with its
// held summed, and the latest answer's currency, cards and bounds. With no
// hold it has no parts and admits nothing.
func (s *holdSet) merged() billing.Hold {
	out := s.Latest
	out.Limits = []billing.HoldLimit{}
	out.Safe = nil
	for _, w := range slices.Sorted(maps.Keys(s.Subs)) {
		h := s.Subs[w]
		for _, l := range h.Limits {
			i := slices.IndexFunc(out.Limits, func(x billing.HoldLimit) bool {
				return partID(x.Limit, x.WindowStartsAt) == partID(l.Limit, l.WindowStartsAt)
			})
			if i < 0 {
				out.Limits = append(out.Limits, l)
				continue
			}
			out.Limits[i].Held = satAdd(out.Limits[i].Held, l.Held)
		}
		out.Safe = addSafe(out.Safe, h.Safe)
	}
	return out
}

// consume takes usage the server settled without answering its hold off the
// holds that hold its part, in their windows' order.
func (s *holdSet) consume(part string, cost int64) {
	for _, w := range slices.Sorted(maps.Keys(s.Subs)) {
		if cost <= 0 {
			return
		}
		h := s.Subs[w]
		for i, l := range h.Limits {
			if partID(l.Limit, l.WindowStartsAt) != part || l.Held <= 0 {
				continue
			}
			take := min(cost, l.Held)
			h.Limits = slices.Clone(h.Limits)
			h.Limits[i].Held -= take
			cost -= take
			s.Subs[w] = h
			break
		}
	}
}

func satAdd(a, b int64) int64 {
	if b > 0 && a > maxGateAmount-b {
		return maxGateAmount
	}
	return a + b
}

type boundKey struct{ meter, cell string }

type moneyCard struct {
	meter limits.Meter
	card  catalog.LimitRateCard
}

// holdPricing is how a gate prices a request against one hold: named-unit
// limits by their rate cards, the same code the server runs; money by the
// hold's pinned cards at their price bounds, which never price below the
// server.
type holdPricing struct {
	currency string
	parts    []holdPart
	units    map[string]limits.Definition
	meters   map[string]limits.Meter
	money    []moneyCard
	bounds   map[boundKey]billing.PriceBound
	// events are the event types the server priced for this hold.
	events map[string]bool
}

func newHoldPricing(h billing.Hold, events map[string]bool) *holdPricing {
	p := &holdPricing{
		currency: strings.ToUpper(strings.TrimSpace(h.Currency)),
		units:    map[string]limits.Definition{},
		meters:   map[string]limits.Meter{},
		bounds:   map[boundKey]billing.PriceBound{},
		events:   map[string]bool{},
	}
	for e := range events {
		p.events[e] = true
	}
	for _, l := range h.Limits {
		p.parts = append(p.parts, holdPart{id: partID(l.Limit, l.WindowStartsAt), limit: l.Limit, unit: l.Unit, overrun: l.Overrun, start: l.WindowStartsAt, end: l.WindowEndsAt})
	}
	for _, rc := range h.RateCards {
		m := limits.Meter{Key: rc.Meter, EventType: rc.EventType, ValueProperty: limits.PropertyKey(rc.ValueProperty), Aggregation: rc.Aggregation, GroupBy: rc.GroupBy}
		if m.EventType == "" {
			m.EventType = rc.Meter
		}
		if m.ValueProperty == "" {
			m.ValueProperty = rc.Meter
		}
		if m.Aggregation == "" {
			m.Aggregation = catalog.AggregationSum
		}
		p.meters[rc.Meter] = m
		p.events[m.EventType] = true
		card := catalog.LimitRateCard{Meter: rc.Meter, Filter: rc.Filter, Price: rc.Price}
		if rc.Limit == nil {
			if c := strings.ToUpper(strings.TrimSpace(rc.Price.Currency)); c == "" || c == p.currency {
				p.money = append(p.money, moneyCard{meter: m, card: card})
			}
			continue
		}
		def := p.units[*rc.Limit]
		def.Key = *rc.Limit
		def.RateCards = append(def.RateCards, card)
		p.units[*rc.Limit] = def
	}
	for _, b := range h.PriceBounds {
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

// take is what a decision takes from one part.
type take struct {
	part    string
	cost    int64
	overrun catalog.Overrun
	money   bool
}

// errUnpriced: the hold cannot price the request; a sync can.
var errUnpriced = errors.New("openrails: gate: the hold cannot price this request")

// cost is what r takes from each limit of the hold it counts toward, in the
// window current at now, as the server claims it: a currency limit takes
// every request's money cost, a named-unit limit the units its rate cards
// count of r's usage (a host-stated request has none). The money cost is
// priced when a currency limit (or withMoney) needs it. errUnpriced asks for
// a sync: every window of a limit r counts toward has ended, or the hold has
// no card or bound for r.
func (p *holdPricing) cost(now time.Time, r request, withMoney bool) ([]take, int64, error) {
	var claimed []holdPart
	seen := map[string]bool{}
	for _, part := range p.parts {
		if seen[part.limit] || !p.counts(part, r) {
			continue
		}
		w, ok := p.window(part.limit, now)
		if !ok {
			return nil, 0, errUnpriced
		}
		claimed = append(claimed, w)
		seen[part.limit] = true
		withMoney = withMoney || p.isMoney(part)
	}
	var money int64
	if withMoney {
		var err error
		if money, err = p.moneyCost(r); err != nil {
			return nil, 0, err
		}
	}
	out := make([]take, 0, len(claimed))
	for _, part := range claimed {
		c := money
		if !p.isMoney(part) {
			var err error
			if c, err = p.unitCost(part.limit, r); err != nil {
				return nil, 0, err
			}
		}
		out = append(out, take{part: part.id, cost: c, overrun: part.overrun, money: p.isMoney(part)})
	}
	return out, money, nil
}

// window is the limit's part a request counts in at now: the one whose
// window contains now, else the next to begin. The server places windows by
// its own clock, and holds only its current window and the next: a window it
// holds that has not begun by the gate's clock is current on the server's,
// which is ahead (a first-use or session window it just opened).
func (p *holdPricing) window(limit string, now time.Time) (holdPart, bool) {
	var next *holdPart
	for i, part := range p.parts {
		switch {
		case part.limit != limit:
		case part.current(now):
			return part, true
		case part.start != nil && now.Before(*part.start) && (next == nil || part.start.Before(*next.start)):
			next = &p.parts[i]
		}
	}
	if next == nil {
		return holdPart{}, false
	}
	return *next, true
}

func (p *holdPricing) isMoney(part holdPart) bool { return strings.EqualFold(part.unit, p.currency) }

// counts reports whether r counts toward the part's limit.
func (p *holdPricing) counts(part holdPart, r request) bool {
	if p.isMoney(part) {
		return true
	}
	if r.usage == nil {
		return false
	}
	for _, card := range p.units[part.limit].RateCards {
		if m, ok := p.meters[card.Meter]; ok && m.EventType == r.usage.EventType {
			return true
		}
	}
	return false
}

// costOf is what r counts toward each taken part's limit, by the same rules:
// a capture settles in the windows its admission took from.
func (p *holdPricing) costOf(r request, taken []take) ([]take, error) {
	var money int64
	var priced bool
	out := make([]take, len(taken))
	for i, t := range taken {
		out[i] = t
		if t.money {
			if !priced {
				var err error
				if money, err = p.moneyCost(r); err != nil {
					return nil, err
				}
				priced = true
			}
			out[i].cost = money
			continue
		}
		c, err := p.unitCost(partLimit(t.part), r)
		if err != nil {
			return nil, err
		}
		out[i].cost = c
	}
	return out, nil
}

func (p *holdPricing) unitCost(limit string, r request) (int64, error) {
	def, ok := p.units[limit]
	if r.usage == nil || !ok {
		return 0, nil
	}
	units, err := limits.Units(def, p.meters, *r.usage)
	return units, statusOf(err)
}

func (p *holdPricing) moneyCost(r request) (int64, error) {
	if r.usage == nil {
		return r.amount, nil
	}
	var total int64
	priced := false
	for _, mc := range p.money {
		q, cell, counted, err := limits.Measure(mc.meter, mc.card, *r.usage)
		if err != nil {
			return 0, statusOf(err)
		}
		if mc.meter.EventType == r.usage.EventType {
			priced = true
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
	if !priced && !p.events[r.usage.EventType] {
		return 0, errUnpriced
	}
	return total, nil
}

const maxGateAmount = int64(^uint64(0) >> 1)

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
