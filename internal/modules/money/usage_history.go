package money

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/eventstore"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/shared/calendarutil"
)

// UsageHistory reads the usage history a filter selects, by day of f.Zone,
// the merchant's accounting zone when nil (ClickHouse groups the quarter
// hours as it reads, so a zone change regroups all of it). Each charge row
// is at the price intake took it at; a volume tier's fall is its Discount.
func (s *MoneyService) UsageHistory(ctx context.Context, f eventstore.HistoryFilter) ([]eventstore.UsageBucket, error) {
	if f.Zone == nil {
		zone, err := s.AccountingZone(ctx)
		if err != nil {
			return nil, err
		}
		f.Zone = zone
	}
	return s.history(ctx, false, f)
}

// UsageBuckets is UsageHistory by the history's own quarter hours.
func (s *MoneyService) UsageBuckets(ctx context.Context, f eventstore.HistoryFilter) ([]eventstore.UsageBucket, error) {
	f.Zone = nil
	return s.history(ctx, false, f)
}

// UsageEvents reads raw usage by invoker as UsageHistory reads history, by
// f.Zone's days or by quarter hour when nil: back eventstore.EventsKept.
func (s *MoneyService) UsageEvents(ctx context.Context, f eventstore.HistoryFilter) ([]eventstore.UsageBucket, error) {
	return s.history(ctx, true, f)
}

func (s *MoneyService) history(ctx context.Context, events bool, f eventstore.HistoryFilter) ([]eventstore.UsageBucket, error) {
	if _, err := merchant.Require(ctx); err != nil {
		return nil, err
	}
	store := s.db.EventStore()
	if store == nil {
		return nil, eventstore.Required("usage history")
	}
	if events {
		return store.UsageEvents(ctx, f)
	}
	return store.UsageHistory(ctx, f)
}

// UsageForgiveness is what postings cut in (from, to] forgave of usage in a
// merchant's unit, as rows of the cut's bucket, from the split each posting
// stored (usage_postings.forgiven_lines): Forgiven, and the same taken out of
// Amount, what was charged. A reversal that takes forgiveness back is
// negative.
func (s *MoneyService) UsageForgiveness(ctx context.Context, from, to time.Time) ([]eventstore.UsageBucket, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	postings, err := s.db.Gen(ctx).ListForgivenUsagePostings(ctx, gen.ListForgivenUsagePostingsParams{MerchantID: mid.UUID(), CutAfter: from, CutThrough: to})
	if err != nil {
		return nil, err
	}
	var out []eventstore.UsageBucket
	for _, p := range postings {
		lines, err := decodeForgivenLines(p.ForgivenLines)
		if err != nil {
			return nil, fmt.Errorf("usage posting %s: %w", p.ID, err)
		}
		bucket := p.CutAt.Add(-time.Microsecond).UTC().Truncate(eventstore.HistoryBucket)
		for _, l := range lines {
			out = append(out, eventstore.UsageBucket{CustomerID: billing.CustomerID(p.CustomerID), BucketAt: bucket, Currency: p.Currency,
				EventType: l.EventType, Meter: l.MeterKey, Cell: l.Cell, Invoker: l.Invoker, Amount: -l.Amount, Forgiven: l.Amount})
		}
	}
	return out, nil
}

// ForgivenLine is one share of a posting's forgiveness: the invoker, event
// type and meter cell it was charged to.
type ForgivenLine struct {
	Invoker   string `json:"invoker"`
	EventType string `json:"event_type"`
	MeterKey  string `json:"meter_key"`
	Cell      string `json:"cell"`
	Amount    int64  `json:"amount"`
}

type forgivenKey struct{ invoker, eventType, meter, cell string }

func (l ForgivenLine) key() forgivenKey {
	return forgivenKey{l.Invoker, l.EventType, l.MeterKey, l.Cell}
}

func decodeForgivenLines(raw []byte) ([]ForgivenLine, error) {
	var lines []ForgivenLine
	if len(raw) == 0 {
		return lines, nil
	}
	if err := json.Unmarshal(raw, &lines); err != nil {
		return nil, fmt.Errorf("decode forgiven lines: %w", err)
	}
	return lines, nil
}

// shareForgiven splits amount (either sign) across weights by largest
// remainder, in keys' invoice, leaving out empty shares.
func shareForgiven(amount int64, keys []forgivenKey, weights []int64) []ForgivenLine {
	sign := int64(1)
	if amount < 0 {
		sign = -1
	}
	var out []ForgivenLine
	for i, x := range largestRemainder(amount*sign, weights) {
		if x != 0 {
			k := keys[i]
			out = append(out, ForgivenLine{Invoker: k.invoker, EventType: k.eventType, MeterKey: k.meter, Cell: k.cell, Amount: x * sign})
		}
	}
	return out
}

// forgivenLines shares what a posting forgives across what its period charged
// before the cut, per invoker, event type and meter cell, from the store's
// raw events at intake's prices, as it posts: the split is stored with the
// posting.
func (s *MoneyService) forgivenLines(ctx context.Context, q *gen.Queries, customer uuid.UUID, currency string, period, cut time.Time, amount int64) ([]ForgivenLine, error) {
	store := s.eventStore(q)
	if store == nil {
		return nil, eventstore.Required("forgiving usage")
	}
	charges, err := store.PeriodCharges(ctx, billing.CustomerID(customer), currency, period.UTC(), cut)
	if err != nil {
		return nil, err
	}
	charged := map[forgivenKey]int64{}
	var keys []forgivenKey
	for _, c := range charges {
		if c.Amount <= 0 {
			continue
		}
		k := forgivenKey{c.Invoker, c.EventType, c.Meter, c.Cell}
		if _, ok := charged[k]; !ok {
			keys = append(keys, k)
		}
		charged[k] = addSat(charged[k], c.Amount)
	}
	if len(keys) == 0 {
		keys, charged = []forgivenKey{{}}, map[forgivenKey]int64{{}: 1}
	}
	slices.SortFunc(keys, func(a, b forgivenKey) int {
		return cmp.Or(strings.Compare(a.invoker, b.invoker), strings.Compare(a.eventType, b.eventType), strings.Compare(a.meter, b.meter), strings.Compare(a.cell, b.cell))
	})
	weights := make([]int64, len(keys))
	for i, k := range keys {
		weights[i] = charged[k]
	}
	return shareForgiven(amount, keys, weights), nil
}

// takeBackForgiven shares a reversal's take-back (negative) across what the
// period's postings forgave, net, by the same rule: no share goes below
// what it forgave.
func takeBackForgiven(forgave map[forgivenKey]int64, amount int64) []ForgivenLine {
	var keys []forgivenKey
	for k, v := range forgave {
		if v > 0 {
			keys = append(keys, k)
		}
	}
	slices.SortFunc(keys, func(a, b forgivenKey) int {
		return cmp.Or(strings.Compare(a.invoker, b.invoker), strings.Compare(a.eventType, b.eventType), strings.Compare(a.meter, b.meter), strings.Compare(a.cell, b.cell))
	})
	weights := make([]int64, len(keys))
	for i, k := range keys {
		weights[i] = forgave[k]
	}
	return shareForgiven(amount, keys, weights)
}

// largestRemainder splits a nonnegative amount across positive weights: each
// part is its exact share's floor, and what the floors leave goes one at a
// time to the largest fractions, earlier first on a tie.
func largestRemainder(amount int64, weights []int64) []int64 {
	out := make([]int64, len(weights))
	var total big.Int
	for _, w := range weights {
		total.Add(&total, big.NewInt(w))
	}
	if total.Sign() <= 0 {
		return out
	}
	a := big.NewInt(amount)
	fractions := make([]*big.Int, len(weights))
	left := amount
	for i, w := range weights {
		q, r := new(big.Int).QuoRem(new(big.Int).Mul(a, big.NewInt(w)), &total, new(big.Int))
		out[i], fractions[i] = q.Int64(), r
		left -= out[i]
	}
	order := make([]int, len(weights))
	for i := range order {
		order[i] = i
	}
	slices.SortStableFunc(order, func(x, y int) int { return fractions[y].Cmp(fractions[x]) })
	for _, i := range order[:left] {
		out[i]++
	}
	return out
}

// LocalDays are the instants that bound zone's days overlapping [from, to),
// each day from its first instant (the transition where clocks skip its
// midnight): history read between them is the quarter hours of those days.
func LocalDays(zone *time.Location, from, to time.Time) (time.Time, time.Time) {
	l := to.Add(-time.Microsecond).In(zone)
	return calendarutil.DateStart(from, zone).UTC(), calendarutil.DayStart(l.Year(), l.Month(), l.Day()+1, zone).UTC()
}
