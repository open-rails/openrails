package money

import (
	"context"
	"time"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/eventstore"
	"github.com/open-rails/openrails/internal/merchant"
)

// UsageHistoryRow is a 15-minute bucket of usage history with what it cost:
// a charge row's share of its bucket's price, a failed event row what it
// charged. Readers fold buckets into the days and months of their zone.
type UsageHistoryRow struct {
	eventstore.UsageBucket
	Amount int64
}

// UsageHistory reads the usage history a filter selects and prices it from
// the event store. A bucket's price of a meter cell is the price of its period
// through the bucket less the price through the bucket before, so any days
// the buckets fold into sum to what the period posts; it is shared across the
// bucket's rows by quantity. A row priced under a card print not yet recorded
// is left out.
func (s *MoneyService) UsageHistory(ctx context.Context, f eventstore.HistoryFilter) ([]UsageHistoryRow, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	store := s.db.EventStore()
	if store == nil {
		return nil, eventstore.Required("usage history")
	}
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
		if prior, err = store.UsageHistory(ctx, eventstore.HistoryFilter{CustomerID: f.CustomerID, Currency: f.Currency, From: periods.Truncate(eventstore.HistoryBucket), To: f.From, PeriodsFrom: periods}); err != nil {
			return nil, err
		}
		for _, b := range prior {
			prints = append(prints, b.Print)
		}
	}
	cards, err := loadPrints(ctx, s.db.Gen(ctx), mid.UUID(), prints)
	if err != nil {
		return nil, err
	}
	return cards.priceHistory(buckets, prior)
}

// UsageForgiveness is what postings cut in (from, to] forgave of usage in a
// merchant's unit, as rows of its cut's bucket: Forgiven, and the same taken
// out of Amount, what was charged. A posting's forgiveness is shared across
// its lines by amount; a reversal that takes forgiveness back is negative.
func (s *MoneyService) UsageForgiveness(ctx context.Context, from, to time.Time) ([]UsageHistoryRow, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	q := s.db.Gen(ctx)
	postings, err := q.ListForgivenUsagePostings(ctx, gen.ListForgivenUsagePostingsParams{MerchantID: mid.UUID(), CutAfter: from, CutThrough: to})
	if err != nil || len(postings) == 0 {
		return nil, err
	}
	lines := make([][]PostingLine, len(postings))
	var prints []string
	for i, p := range postings {
		if lines[i], err = decodePostingLines(p.Lines); err != nil {
			return nil, err
		}
		for _, l := range lines[i] {
			prints = append(prints, l.Print)
		}
	}
	cards, err := loadPrints(ctx, q, mid.UUID(), prints)
	if err != nil {
		return nil, err
	}
	var out []UsageHistoryRow
	for i, p := range postings {
		sign := int64(1)
		if p.Forgiven < 0 {
			sign = -1
		}
		var shared []PostingLine
		var weights []int64
		for _, l := range lines[i] {
			if l.Amount*sign > 0 {
				shared, weights = append(shared, l), append(weights, l.Amount*sign)
			}
		}
		if len(shared) == 0 {
			shared, weights = []PostingLine{{}}, []int64{1}
		}
		bucket := p.CutAt.Add(-time.Microsecond).UTC().Truncate(eventstore.HistoryBucket)
		for j, x := range share(p.Forgiven*sign, weights) {
			l := shared[j]
			event := l.MeterKey
			if c, ok := cards[l.Print]; ok && c.card.EventType != "" {
				event = c.card.EventType
			}
			out = append(out, UsageHistoryRow{UsageBucket: eventstore.UsageBucket{CustomerID: billing.CustomerID(p.CustomerID), BucketAt: bucket,
				Currency: p.Currency, EventType: event, Meter: l.MeterKey, Cell: l.Cell, Print: l.Print, PeriodStartsAt: p.PeriodStartsAt,
				Outcome: eventstore.UsageSucceeded, Forgiven: x * sign}, Amount: -x * sign})
		}
	}
	return out, nil
}

// UsageDays are the instants that bound the days of the merchant's
// accounting zone overlapping [from, to): history read between them folds
// into whole local days, since every zone's offset is a whole number of
// buckets.
func (s *MoneyService) UsageDays(ctx context.Context, from, to time.Time) (time.Time, time.Time, error) {
	zone, err := s.accountingZone(ctx)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	start, end := LocalDays(zone, from, to)
	return start, end, nil
}

// LocalDays are the instants that bound zone's days overlapping [from, to).
func LocalDays(zone *time.Location, from, to time.Time) (time.Time, time.Time) {
	f := from.In(zone)
	start := time.Date(f.Year(), f.Month(), f.Day(), 0, 0, 0, 0, zone)
	l := to.Add(-time.Microsecond).In(zone)
	end := time.Date(l.Year(), l.Month(), l.Day()+1, 0, 0, 0, 0, zone)
	return start.UTC(), end.UTC()
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
