package money

import (
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"slices"
	"time"

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

// UsageForgiveness is what usage receipts cut in (from, to] forgave of usage
// in a merchant's unit, as rows of the cut's bucket, from each receipt line's
// forgiven: by event type and meter cell, with no invoker. Forgiven, and the
// same taken out of Amount, what was charged. A period whose price fell
// takes forgiveness back: negative.
func (s *MoneyService) UsageForgiveness(ctx context.Context, from, to time.Time) ([]eventstore.UsageBucket, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	q := s.db.Gen(ctx)
	receipts, err := q.ListForgivenUsageReceipts(ctx, gen.ListForgivenUsageReceiptsParams{MerchantID: mid.UUID(), CutAfter: from, CutThrough: to})
	if err != nil || len(receipts) == 0 {
		return nil, err
	}
	decoded := make([][]receiptLine, len(receipts))
	var prints []string
	for i, r := range receipts {
		if err := json.Unmarshal(r.Lines, &decoded[i]); err != nil {
			return nil, fmt.Errorf("usage receipt %s: decode lines: %w", r.ID, err)
		}
		for _, l := range decoded[i] {
			if l.Forgiven != 0 {
				prints = append(prints, l.Print)
			}
		}
	}
	// A line's event type is its card's.
	cards, err := loadPrints(ctx, q, mid.UUID(), prints)
	if err != nil {
		return nil, err
	}
	var out []eventstore.UsageBucket
	for i, r := range receipts {
		bucket := r.CutAt.Add(-time.Microsecond).UTC().Truncate(eventstore.HistoryBucket)
		for _, l := range decoded[i] {
			if l.Forgiven == 0 {
				continue
			}
			out = append(out, eventstore.UsageBucket{CustomerID: billing.CustomerID(r.CustomerID), BucketAt: bucket, Currency: r.Currency,
				EventType: cards[l.Print].card.EventType, Meter: l.MeterKey, Cell: l.Cell, Amount: -l.Forgiven, Forgiven: l.Forgiven})
		}
	}
	return out, nil
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
