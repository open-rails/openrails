package money

import (
	"context"
	"time"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/eventstore"
	"github.com/open-rails/openrails/internal/merchant"
)

// UsageHistoryRow is a row of usage history with what it cost: a charge
// row's share of its day's price, a failed event row what it charged.
type UsageHistoryRow struct {
	eventstore.UsageDay
	Amount int64
}

// UsageHistory reads the usage history a filter selects and prices it from
// the event store. A day's price of a meter cell is the price of its period
// through the day less the price through the day before, so the days sum to
// what the period posts; it is shared across the day's rows by quantity. A
// row priced under a card print not yet recorded is left out.
func (s *MoneyService) UsageHistory(ctx context.Context, f eventstore.DayFilter) ([]UsageHistoryRow, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	store := s.db.EventStore()
	if store == nil {
		return nil, eventstore.Required("usage history")
	}
	days, err := store.UsageDays(ctx, f)
	if err != nil {
		return nil, err
	}
	var periods *time.Time
	var prints []string
	for _, d := range days {
		if d.Meter == "" {
			continue
		}
		prints = append(prints, d.Print)
		if periods == nil || d.PeriodStartsAt.Before(*periods) {
			p := d.PeriodStartsAt
			periods = &p
		}
	}
	var prior []eventstore.UsageDay
	if periods != nil && periods.Before(f.From) {
		if prior, err = store.UsageDays(ctx, eventstore.DayFilter{CustomerID: f.CustomerID, Currency: f.Currency, From: *periods, To: f.From, PeriodsFrom: periods}); err != nil {
			return nil, err
		}
		for _, d := range prior {
			prints = append(prints, d.Print)
		}
	}
	cards, err := loadPrints(ctx, s.db.Gen(ctx), mid.UUID(), prints)
	if err != nil {
		return nil, err
	}
	return cards.priceHistory(days, prior)
}

// historyCell names one customer's meter cell in a pricing period.
type historyCell struct {
	customer billing.CustomerID
	currency string
	cellKey
}

// priceHistory prices history rows (days) given the rows of the same
// periods' earlier days (prior).
func (p usagePrints) priceHistory(days, prior []eventstore.UsageDay) ([]UsageHistoryRow, error) {
	cellOf := func(d eventstore.UsageDay) historyCell {
		return historyCell{d.CustomerID, d.Currency, cellKey{d.PeriodStartsAt, d.Meter, d.Cell}}
	}
	before := map[historyCell][]printQuantity{}
	for _, d := range prior {
		if _, ok := p[d.Print]; ok && d.Meter != "" {
			before[cellOf(d)] = append(before[cellOf(d)], printQuantity{print: d.Print, quantity: d.Quantity, stated: d.Stated})
		}
	}
	out := make([]UsageHistoryRow, 0, len(days))
	byCell := map[historyCell][]int{}
	for _, d := range days {
		if d.Meter == "" {
			out = append(out, UsageHistoryRow{UsageDay: d, Amount: d.Charged})
			continue
		}
		if _, ok := p[d.Print]; !ok {
			continue
		}
		byCell[cellOf(d)] = append(byCell[cellOf(d)], len(out))
		out = append(out, UsageHistoryRow{UsageDay: d})
	}
	for cell, rows := range byCell {
		quantities := make([]dayQuantity, 0, len(rows))
		for _, i := range rows {
			d := out[i]
			quantities = append(quantities, dayQuantity{day: d.Day, printQuantity: printQuantity{print: d.Print, quantity: d.Quantity, stated: d.Stated}})
		}
		prices, err := p.dayPrices(cell.cell, before[cell], quantities)
		if err != nil {
			return nil, err
		}
		onDay := map[time.Time][]int{}
		for _, i := range rows {
			onDay[out[i].Day] = append(onDay[out[i].Day], i)
		}
		for day, idx := range onDay {
			weights := make([]int64, len(idx))
			for j, i := range idx {
				weights[j] = out[i].Quantity
			}
			for j, amount := range shareSigned(prices[day], weights) {
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
