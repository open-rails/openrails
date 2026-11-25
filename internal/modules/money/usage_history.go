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

// UsageHistoryRow is a bucket of usage history with what it cost: a charge
// row's share of its bucket's price, a failed event row what it charged.
type UsageHistoryRow struct {
	eventstore.UsageBucket
	Amount int64
}

// UsageHistory reads the usage history a filter selects, by day of the
// merchant's accounting zone (ClickHouse groups the hours as it reads, so a
// zone change regroups all of it), and prices it from the event store. A
// day's price of a meter cell is the price of its period through the day less
// the price through the day before, so the days sum to what the period posts;
// it is shared across the day's rows by quantity. A row priced under a card
// print not yet recorded is left out.
func (s *MoneyService) UsageHistory(ctx context.Context, f eventstore.HistoryFilter) ([]UsageHistoryRow, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	store := s.db.EventStore()
	if store == nil {
		return nil, eventstore.Required("usage history")
	}
	if f.Zone, err = s.accountingZone(ctx); err != nil {
		return nil, err
	}
	return usageHistory(ctx, s.db.Gen(ctx), store, mid.UUID(), f)
}

func usageHistory(ctx context.Context, q *gen.Queries, store *eventstore.Store, merchantID uuid.UUID, f eventstore.HistoryFilter) ([]UsageHistoryRow, error) {
	buckets, err := store.UsageHistory(ctx, f)
	if err != nil {
		return nil, err
	}
	var periods *time.Time
	var prints []string
	for _, b := range buckets {
		if b.Meter == "" {
			continue
		}
		prints = append(prints, b.Print)
		if periods == nil || b.PeriodStartsAt.Before(*periods) {
			p := b.PeriodStartsAt
			periods = &p
		}
	}
	var prior []eventstore.UsageBucket
	if periods != nil && periods.Before(f.From) {
		if prior, err = store.UsageHistory(ctx, eventstore.HistoryFilter{CustomerID: f.CustomerID, Currency: f.Currency, From: periods.Truncate(eventstore.HistoryBucket), To: f.From, PeriodsFrom: periods, Zone: f.Zone}); err != nil {
			return nil, err
		}
		for _, b := range prior {
			prints = append(prints, b.Print)
		}
	}
	cards, err := loadPrints(ctx, q, merchantID, prints)
	if err != nil {
		return nil, err
	}
	return cards.priceHistory(buckets, prior)
}

// UsageForgiveness is what postings cut in (from, to] forgave of usage in a
// merchant's unit, as rows of the cut's bucket, from the split each posting
// stored (usage_postings.forgiven_lines): Forgiven, and the same taken out of
// Amount, what was charged. A reversal that takes forgiveness back is
// negative.
func (s *MoneyService) UsageForgiveness(ctx context.Context, from, to time.Time) ([]UsageHistoryRow, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	postings, err := s.db.Gen(ctx).ListForgivenUsagePostings(ctx, gen.ListForgivenUsagePostingsParams{MerchantID: mid.UUID(), CutAfter: from, CutThrough: to})
	if err != nil {
		return nil, err
	}
	var out []UsageHistoryRow
	for _, p := range postings {
		lines, err := decodeForgivenLines(p.ForgivenLines)
		if err != nil {
			return nil, fmt.Errorf("usage posting %s: %w", p.ID, err)
		}
		bucket := p.CutAt.Add(-time.Microsecond).UTC().Truncate(eventstore.HistoryBucket)
		for _, l := range lines {
			row := UsageHistoryRow{UsageBucket: eventstore.UsageBucket{CustomerID: billing.CustomerID(p.CustomerID), BucketAt: bucket, Currency: p.Currency,
				EventType: l.EventType, Meter: l.MeterKey, Cell: l.Cell, PeriodStartsAt: p.PeriodStartsAt.UTC(), Invoker: l.Invoker,
				Outcome: eventstore.UsageSucceeded}, Amount: -l.Amount}
			row.Forgiven = l.Amount
			out = append(out, row)
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
// remainder, in keys' order, leaving out empty shares.
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
// before the cut, per invoker, event type and meter cell, from the event
// store's history, as it posts: the split is stored with the posting.
func (s *MoneyService) forgivenLines(ctx context.Context, q *gen.Queries, merchantID, customer uuid.UUID, currency string, period, cut time.Time, amount int64) ([]ForgivenLine, error) {
	store := s.eventStore(q)
	if store == nil {
		return nil, eventstore.Required("forgiving usage")
	}
	period = period.UTC()
	bucket := cut.Add(-time.Microsecond).UTC().Truncate(eventstore.HistoryBucket)
	history, err := usageHistory(ctx, q, store, merchantID, eventstore.HistoryFilter{CustomerID: billing.CustomerID(customer), Currency: currency,
		From: period.Truncate(eventstore.HistoryBucket), To: bucket.Add(eventstore.HistoryBucket), PeriodsFrom: &period})
	if err != nil {
		return nil, err
	}
	charged := map[forgivenKey]int64{}
	var keys []forgivenKey
	for _, r := range history {
		if r.Meter == "" || !r.PeriodStartsAt.Equal(period) || r.BucketAt.After(bucket) || r.Amount <= 0 {
			continue
		}
		k := forgivenKey{r.Invoker, r.EventType, r.Meter, r.Cell}
		if _, ok := charged[k]; !ok {
			keys = append(keys, k)
		}
		charged[k] = addSat(charged[k], r.Amount)
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

// UsageDays are the instants that bound the days of the merchant's
// accounting zone overlapping [from, to): history read between them is the
// hours that start in those days.
func (s *MoneyService) UsageDays(ctx context.Context, from, to time.Time) (time.Time, time.Time, error) {
	zone, err := s.accountingZone(ctx)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	start, end := LocalDays(zone, from, to)
	return start, end, nil
}

// LocalDays are the instants that bound zone's days overlapping [from, to),
// each day from its first instant (the transition where clocks skip its
// midnight).
func LocalDays(zone *time.Location, from, to time.Time) (time.Time, time.Time) {
	l := to.Add(-time.Microsecond).In(zone)
	return calendarutil.DateStart(from, zone).UTC(), calendarutil.DayStart(l.Year(), l.Month(), l.Day()+1, zone).UTC()
}

// historyCell names one customer's meter cell in a pricing period.
type historyCell struct {
	customer billing.CustomerID
	currency string
	cellKey
}

// priceHistory prices history rows (buckets) given the rows of the same
// periods' earlier buckets (prior).
func (p usagePrints) priceHistory(buckets, prior []eventstore.UsageBucket) ([]UsageHistoryRow, error) {
	cellOf := func(b eventstore.UsageBucket) historyCell {
		return historyCell{b.CustomerID, b.Currency, cellKey{b.PeriodStartsAt, b.Meter, b.Cell}}
	}
	before := map[historyCell][]printQuantity{}
	for _, b := range prior {
		if _, ok := p[b.Print]; ok && b.Meter != "" {
			before[cellOf(b)] = append(before[cellOf(b)], printQuantity{print: b.Print, quantity: b.Quantity, stated: b.Stated})
		}
	}
	out := make([]UsageHistoryRow, 0, len(buckets))
	byCell := map[historyCell][]int{}
	for _, b := range buckets {
		if b.Meter == "" {
			out = append(out, UsageHistoryRow{UsageBucket: b, Amount: b.Charged})
			continue
		}
		if _, ok := p[b.Print]; !ok {
			continue
		}
		byCell[cellOf(b)] = append(byCell[cellOf(b)], len(out))
		out = append(out, UsageHistoryRow{UsageBucket: b})
	}
	for cell, rows := range byCell {
		quantities := make([]bucketQuantity, 0, len(rows))
		for _, i := range rows {
			b := out[i]
			quantities = append(quantities, bucketQuantity{bucket: b.BucketAt, printQuantity: printQuantity{print: b.Print, quantity: b.Quantity, stated: b.Stated}})
		}
		prices, err := p.bucketPrices(cell.cell, before[cell], quantities)
		if err != nil {
			return nil, err
		}
		inBucket := map[time.Time][]int{}
		for _, i := range rows {
			inBucket[out[i].BucketAt] = append(inBucket[out[i].BucketAt], i)
		}
		for bucket, idx := range inBucket {
			weights := make([]int64, len(idx))
			for j, i := range idx {
				weights[j] = out[i].Quantity
			}
			for j, amount := range shareSigned(prices[bucket], weights) {
				out[idx[j]].Amount = amount
			}
		}
	}
	return out, nil
}

// shareSigned is share for an amount of either sign.
func shareSigned(amount int64, weights []int64) []int64 {
	if amount >= 0 {
		return share(amount, weights)
	}
	out := share(-amount, weights)
	for i := range out {
		out[i] = -out[i]
	}
	return out
}
